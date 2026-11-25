// Command trackferry runs the TrackFerry home song-request service.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo
	"unicode/utf8"

	"trackferry/internal/api"
	"trackferry/internal/auth"
	"trackferry/internal/cache"
	"trackferry/internal/config"
	"trackferry/internal/feiniu"
	"trackferry/internal/fscheck"
	"trackferry/internal/home"
	"trackferry/internal/intent"
	"trackferry/internal/library"
	"trackferry/internal/live"
	"trackferry/internal/logx"
	"trackferry/internal/observe"
	"trackferry/internal/online"
	"trackferry/internal/privs"
	"trackferry/internal/relay"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/stream"
	"trackferry/internal/takeover"
	"trackferry/internal/update"
	"trackferry/internal/webui"
	"trackferry/internal/xiaomi"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
	repo    = "madrays/trackferry" // where releases are published
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	// Before anything in the data folder is read, let alone run.
	if err := drop(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "trackferry:", err)
		os.Exit(1)
	}
	var err error
	switch cmd {
	case "serve", "setup-code", "reset-admin":
		launch(cmd == "serve")
	}
	switch cmd {
	case "serve":
		err = serve()
	case "relay":
		err = runRelay()
	case "healthcheck":
		err = healthcheck()
	case "setup-code":
		err = setupCode()
	case "reset-admin":
		err = resetAdmin()
	case "version":
		fmt.Printf("trackferry %s (%s)\n", version, commit)
	default:
		err = fmt.Errorf("unknown command %q (want serve, relay, healthcheck, setup-code, reset-admin or version)", cmd)
	}
	if errors.Is(err, errRestart) {
		err = update.Relaunch(handOver)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trackferry:", err)
		os.Exit(1)
	}
}

// dataMount is where the image keeps its data; only this and the relay's volume are ever
// given away, whatever a configuration file says.
const dataMount = "/data"

// cacheMount is where the kept online songs go when a folder was mounted for them. The image
// has no /cache, so the folder above exists only then.
const cacheMount = "/cache/trackferry"

func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// openCache uses the mounted folder if it can be written, else the data folder.
func openCache(data string, limitMB int, log *slog.Logger) (*cache.Cache, error) {
	dir := filepath.Join(data, "cache")
	if isDir(filepath.Dir(cacheMount)) {
		c, err := cache.Open(dir, cacheMount, limitMB, log)
		if err == nil {
			return c, nil
		}
		log.Warn("缓存　挂载的目录不能写，这次放在 data/cache", "目录", filepath.Dir(cacheMount), "原因", err,
			"解决", fmt.Sprintf("给身份 %d:%d 写权限，然后重启容器", os.Getuid(), os.Getgid()))
	}
	return cache.Open(dir, "", limitMB, log)
}

// drop leaves root behind, after handing over the folders the command is the owner of.
func drop(cmd string) error {
	var dirs []string
	switch cmd {
	case "serve":
		dirs = []string{dataMount, filepath.Dir(relay.Socket)}
		if os.Geteuid() == 0 && isDir(filepath.Dir(cacheMount)) { // never the mount itself: it may hold other things
			os.Mkdir(cacheMount, 0o700)
			dirs = append(dirs, cacheMount)
		}
	case "relay":
		dirs = []string{filepath.Dir(relay.Socket)}
	}
	err := privs.Drop(dirs...)
	if errors.Is(err, privs.ErrCannotDrop) && cmd != "healthcheck" {
		logx.Banner(os.Stderr, "启动不了：不能切换到普通身份",
			logx.Row{Name: "原因", Value: err.Error()},
			logx.Row{Name: "解决", Value: "compose.yaml 里加 cap_add: [CHOWN, DAC_OVERRIDE, SETGID, SETUID]"},
			logx.Row{Name: "或者", Value: "用 user 指定身份，并把 data 的属主改成它"})
	}
	return err
}

// errRestart asks main to start again, into an update.
var errRestart = errors.New("restart")

// handOver replaces this process, keeping its process ID and arguments.
func handOver(path string, env []string) error {
	args := append([]string{path}, os.Args[1:]...)
	return syscall.Exec(path, args, env)
}

// launch runs the version installed from the settings page, if there is one.
func launch(serving bool) {
	cfg, err := config.Load(configPath())
	if err != nil {
		return // the command reports it
	}
	if note := update.Launch(cfg.DataDir, version, serving, handOver); note != "" {
		newLog().Warn("更新　" + note)
	}
}

func configPath() string {
	if p := os.Getenv("TRACKFERRY_CONFIG"); p != "" {
		return p
	}
	return "/data/config.yaml"
}

func newLog() *slog.Logger {
	return logx.New(os.Stdout, os.Getenv("TRACKFERRY_LOG"), os.Getenv("TRACKFERRY_DEBUG") != "")
}

// writable explains a data folder that the identity we run as cannot write, and that could
// not be given to it: the container was told who to be, or the mount refuses.
func writable(dir string) error {
	d := fscheck.CheckData(dir)
	if d.Writable {
		return nil
	}
	logx.Banner(os.Stderr, "启动不了：数据目录不可写",
		logx.Row{Name: "目录", Value: dir + "（compose.yaml 同级的 data）"},
		logx.Row{Name: "原因", Value: d.Error},
		logx.Row{Name: "解决", Value: fmt.Sprintf("在 compose.yaml 所在目录执行 sudo chown -R %d:%d data", os.Getuid(), os.Getgid())},
		logx.Row{Name: "然后", Value: "docker compose restart"},
		logx.Row{Name: "或者", Value: "换成最新的 compose.yaml（没有 user 一行），启动时会自动处理"})
	return errors.New("data directory is not writable: " + d.Error)
}

func serve() error {
	log := newLog()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	if err := writable(cfg.DataDir); err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "trackferry.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	boot, err := st.RecordBoot(ctx, version, commit, time.Now())
	if err != nil {
		return err
	}
	token, err := api.LoadOrCreateToken(filepath.Join(cfg.DataDir, "private", "admin_token"))
	if err != nil {
		return err
	}

	private := filepath.Join(cfg.DataDir, "private")
	setupCodePath := filepath.Join(private, "setup_code")
	firstCode := ""
	// Deliberately logged: whoever can read the container log owns the install.
	showCode := func(code string) { logx.SetupCode(os.Stdout, code, cfg.PublicBaseURL) }
	if _, err := st.AdminCredential(ctx); errors.Is(err, store.ErrNotFound) {
		if firstCode, err = auth.LoadOrCreateSetupCode(setupCodePath); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	sink := func(ctx context.Context, found []xiaomi.Device) error {
		devs := make([]store.DiscoveredDevice, 0, len(found))
		for _, d := range found {
			devs = append(devs, store.DiscoveredDevice{DeviceID: d.DeviceID, Name: d.Name, Alias: d.Alias,
				Model: d.Model, Hardware: d.Hardware, MiotDID: d.MiotDID, Presence: d.Presence})
		}
		res, err := st.SyncDevices(ctx, devs, time.Now())
		if err == nil {
			log.Info("音箱　列表已保存", "新增", res.Added, "更新", res.Updated, "不见了", res.Missing)
		}
		return err
	}
	xm, err := xiaomi.NewManager(xiaomi.NewClient(xiaomi.DefaultEndpoints), filepath.Join(private, "xiaomi.json"), sink, log)
	if err != nil {
		return err
	}
	go xm.Run(ctx)
	hub := live.New()
	streams := stream.New(cfg.PublicBaseURL, cfg.MusicDir)
	streams.Log = log
	if streams.Ready() != nil {
		log.Warn("配置　没有填 TRACKFERRY_URL，音箱取不到歌，接管不会开始")
	}
	if err := st.FailRunningRuns(ctx, time.Now()); err != nil {
		return err
	}
	engine := takeover.New(xm, st, log, takeover.DefaultTiming)
	engine.Style = func(id string) string {
		d, err := st.Device(ctx, id)
		if err != nil {
			return takeover.HushNormal
		}
		return takeover.HushStyle(d.Hush, d.Hardware, d.MiotDID)
	}
	engine.Floor = func(id string) int {
		if d, err := st.Device(ctx, id); err == nil {
			if n, ok := xiaomi.VolumeFloor(d.Hardware); ok {
				return n
			}
		}
		return takeover.DefaultFloor
	}
	engine.Deep = func(c context.Context, id string, on bool) error {
		d, err := st.Device(ctx, id)
		if err != nil || !xiaomi.CanHush(d.Hardware, d.MiotDID) {
			return nil
		}
		return xm.Hush(c, d.Hardware, d.MiotDID, on)
	}
	go func() { // what the first request would otherwise wait for
		defer logx.Recover(log, "音箱")
		devs, _ := st.Devices(ctx)
		for _, d := range devs {
			if !d.InAccount || d.Mode != store.ModeTakeover {
				continue
			}
			if engine.Style(d.DeviceID) != takeover.HushNormal {
				xm.WarmHome(ctx)
			}
		}
	}()
	go engine.Refresh(ctx, func() (out []string) {
		devs, _ := st.Devices(ctx)
		for _, d := range devs {
			if d.InAccount && d.Mode == store.ModeTakeover {
				out = append(out, d.DeviceID)
			}
		}
		return out
	})
	go engine.RecoverVolumes(ctx) // a crash mid-takeover must not leave a speaker muted
	lib := library.New(cfg.MusicDir, st, log)
	fn, err := feiniu.NewManager(filepath.Join(private, "feiniu.json"))
	if err != nil {
		return err
	}
	lib.Remote = fn
	streams.RemotePrefix = library.RemotePrefix
	streams.Remote = func(rel string) (stream.Opener, string) {
		t, ok := lib.ByPath(rel)
		if !ok || !fn.Ready() {
			return nil, ""
		}
		ext := t.Format
		if ext == "alac" {
			ext = "m4a"
		}
		return func(ctx context.Context, rng string) (*http.Response, error) { return fn.Open(ctx, t.GUID, rng) }, "." + ext
	}
	go fn.Run(ctx)
	go lib.Run(ctx, 6*time.Hour)
	client := online.Client()
	// 2026-11-25 patch: ask 飞牛音乐 first. An enhanced 飞牛音乐 (飞牛音乐增强)
	// answers with its library plus its online catalogue, and its audio is read
	// through it over a signed loopback address. The four public sources stay in
	// the list as the fallback for whatever 飞牛音乐 does not have.
	fnKey := make([]byte, 32)
	if _, err := rand.Read(fnKey); err != nil {
		return err
	}
	fnOnline := api.NewFeiniuOnline(fn.Open, cfg.Listen, fnKey)
	netease := &online.Netease{Client: client, File: filepath.Join(private, "netease.json")}
	if err := netease.Load(); err != nil {
		log.Warn("网易云　登录记录读不出来，按没登录处理")
	}
	go netease.Run(ctx)
	sources := online.New(log, client, &online.Feiniu{Cat: feiniuCatalogue{fn: fn, link: fnOnline.Link}},
		&online.Kuwo{Client: client}, &online.Kugou{Client: client}, &online.Migu{Client: client}, netease)
	router := &routing.Router{Library: lib, Online: sources, Log: log, Openable: streams.Openable}
	router.LoadSettings(ctx, st)
	kept, err := openCache(cfg.DataDir, cacheLimit(ctx, st), log)
	if err != nil {
		log.Warn("缓存　打不开，这次不用", "原因", err)
	} else {
		router.Cached = kept.Find
	}
	speakerName := func(id string) string {
		d, err := st.Device(ctx, id)
		if err != nil {
			return ""
		}
		if d.Alias != "" {
			return d.Alias
		}
		return d.Name
	}
	dispatcher := &takeover.Dispatcher{Live: hub, Name: speakerName, Engine: engine, Store: st, Router: router, Streams: streams, Log: log,
		Allowed: func(id string) bool {
			name := speakerName(id)
			return name != "" && cfg.TakeoverAllowed(name)
		},
		Open: func(rt *routing.Route) stream.Opener {
			if kept == nil {
				return sources.Opener(rt.Found())
			}
			return kept.Opener(rt.Found(), sources.Opener(rt.Found()))
		},
		Keep: func(rt *routing.Route) {
			if kept == nil {
				return
			}
			defer logx.Recover(log, "缓存")
			kctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			if err := kept.Keep(kctx, rt.Found(), rt.Asked(), sources.Opener(rt.Found())); err != nil && ctx.Err() == nil {
				log.Warn("缓存　没有存下", "音频", rt.Label()) // the error may carry the address
			}
		}}
	obs := observe.New(xm, st, log, time.Second)
	obs.Live = hub
	dispatcher.Favorite = func(ctx context.Context, id int64, on bool) error {
		_, err := lib.SetFavorite(ctx, id, on)
		return err
	}
	router.Lists = fn
	dispatcher.Next, dispatcher.Put, dispatcher.Random = router.Next, router.Put, lib.RandomIDs
	router.ArtistQueue = func() bool { return takeover.LoadPrefs(ctx, st).Artist }
	played := func(dev, title, artist string, track int64) {
		dispatcher.Drop(dev) // a song sent by hand ends a list
		dispatcher.Remember(dev, title, artist, track)
	}
	obs.Follows = dispatcher.Follows
	obs.OnCommand(dispatcher.OnCommand)
	obs.OnOther = func(id string, r xiaomi.Record) {
		if slices.Contains(r.AnswerTypes, "AUDIO") {
			dispatcher.Forget(id)
		}
		if intent.Volume(r.Query) {
			go engine.Turned(ctx, id)
		}
		engine.Hear(id, takeover.Heard{Stop: intent.Silences(r.Query), Audio: slices.Contains(r.AnswerTypes, "AUDIO"), Answer: utf8.RuneCountInString(r.Answer)})
	}
	go obs.Run(ctx)

	checker := update.New(repo, version, cfg.UpdateCheck, log)
	if err := checker.LoadProxy(filepath.Join(private, "update_proxy")); err != nil {
		log.Warn("更新　保存的代理地址不能用", "原因", "格式不对或读不出来")
	}
	restart := make(chan struct{})
	if runtime.GOOS == "linux" {
		checker.Installer = &update.Installer{Data: cfg.DataDir, Backup: st.Backup, Restart: func() { close(restart) }}
	}
	go checker.Run(ctx)
	atHome := home.New(home.Source{Hub: hub, Devices: st.Devices, Pending: st.PendingVolumes, Status: xm.PlayStatus,
		Colors: func(ctx context.Context, id string) []string {
			resp, err := fn.Cover(ctx, id, "160")
			if err != nil {
				return nil
			}
			defer resp.Body.Close()
			pic, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			if err != nil {
				return nil
			}
			return home.Colors(pic)
		}}, log, home.DefaultCadence)
	go atHome.Run(ctx)
	srv := api.New(api.Options{
		Config: cfg, Store: st, Token: token, SetupCodePath: setupCodePath,
		Build: api.Build{Version: version, Commit: commit}, Boot: boot,
		Xiaomi: xm, Observer: obs, Engine: engine, Stream: streams, Library: lib, Cache: kept, Router: router, Feiniu: fn, FnOnline: fnOnline, Netease: netease, Live: hub, Home: atHome, Played: played, Dispatcher: dispatcher, Web: webui.Handler(), Log: log, Update: checker, SetupLog: showCode,
	})
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // /stream/ extends its own deadline
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if len(cfg.Legacy) > 0 {
		log.Warn("配置　config.yaml 里的 devices 已不使用，音箱在管理页设置")
	}
	or := func(yes bool, a, b string) string {
		if yes {
			return a
		}
		return b
	}
	address := cfg.PublicBaseURL
	if address == "" {
		address = "没有设置，音箱取不到歌"
	}
	logx.Banner(os.Stdout, fmt.Sprintf("TrackFerry %s（%s）已启动", version, commit),
		logx.Row{Name: "地址", Value: address},
		logx.Row{Name: "小米账号", Value: or(xm.Status().Linked, "已关联", "未关联")},
		logx.Row{Name: "飞牛音乐", Value: or(fn.Linked(), "已连接", "未连接")},
		logx.Row{Name: "接管", Value: or(len(cfg.TakeoverSpeakers) == 0, "没有放行", or(slices.Contains(cfg.TakeoverSpeakers, "*"), "所有音箱可选", fmt.Sprintf("放行 %d 项", len(cfg.TakeoverSpeakers))))},
		logx.Row{Name: "数据", Value: cfg.DataDir},
		logx.Row{Name: "检查更新", Value: or(cfg.UpdateCheck, "开", "关")})
	if firstCode != "" {
		showCode(firstCode)
	}
	srv.MarkReady()
	time.AfterFunc(update.Settle, func() {
		if err := update.Confirm(cfg.DataDir, version); err != nil {
			log.Warn("更新　没能记下这一版运行正常", "原因", err)
		}
	})

	var again error
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	case <-restart:
		again = errRestart
		stop() // everything else ends too
	}
	log.Info("停止　正在收尾")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("停止　有连接没有收尾", "原因", err)
	}
	log.Info("停止　完成")
	return again
}

// runRelay is the whole of the relay container (compose.yaml).
func runRelay() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	target := os.Getenv("TRACKFERRY_RELAY_TARGET")
	if target == "" {
		target = "127.0.0.1:5667"
	}
	return relay.Serve(ctx, relay.Socket, target, newLog())
}

// setupCode shows the code of an install without a password:
// docker exec trackferry /trackferry setup-code
func setupCode() error {
	ctx := context.Background()
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "trackferry.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.AdminCredential(ctx); err == nil {
		fmt.Println("管理密码已经设置过，直接登录。忘记密码：docker exec trackferry /trackferry reset-admin")
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	code, err := auth.LoadOrCreateSetupCode(filepath.Join(cfg.DataDir, "private", "setup_code"))
	if err != nil {
		return err
	}
	logx.SetupCode(os.Stdout, code, cfg.PublicBaseURL)
	return nil
}

// resetAdmin clears the admin password so it can be set again with a fresh setup code.
// Run it inside the container: docker exec trackferry /trackferry reset-admin
func resetAdmin() error {
	ctx := context.Background()
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "trackferry.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ResetAdmin(ctx); err != nil {
		return err
	}
	path := filepath.Join(cfg.DataDir, "private", "setup_code")
	_ = os.Remove(path)
	code, err := auth.LoadOrCreateSetupCode(path)
	if err != nil {
		return err
	}
	fmt.Println("管理密码已清除，所有浏览器已退出登录。")
	logx.SetupCode(os.Stdout, code, cfg.PublicBaseURL)
	return nil
}

// healthcheck is the container probe: liveness of the local HTTP server only.
func healthcheck() error {
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	local, err := cfg.LocalURL()
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(local + "/healthz/live")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("live check: HTTP %d", resp.StatusCode)
	}
	return nil
}

// cacheLimit is the cache's limit in MB as set on the settings page.
func cacheLimit(ctx context.Context, st *store.Store) int {
	v, err := st.Setting(ctx, api.SettingCache)
	if n, e := strconv.Atoi(v); err == nil && e == nil && cache.Valid(n) {
		return n
	}
	return cache.DefaultMB
}

// feiniuCatalogue adapts 飞牛音乐 to what the online search needs (2026-11-25 patch).
type feiniuCatalogue struct {
	fn   *feiniu.Manager
	link func(string) string
}

func (c feiniuCatalogue) SearchFeiniu(ctx context.Context, q string) ([]online.FeiniuSong, error) {
	songs, err := c.fn.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]online.FeiniuSong, 0, len(songs))
	for _, s := range songs {
		names := make([]string, 0, len(s.Artists))
		for _, a := range s.Artists {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		out = append(out, online.FeiniuSong{
			GUID:       s.GUID,
			Title:      s.Title,
			Artist:     strings.Join(names, "、"),
			DurationMS: int64(s.Duration),
		})
	}
	return out, nil
}

func (c feiniuCatalogue) FeiniuLink(guid string) string { return c.link(guid) }
