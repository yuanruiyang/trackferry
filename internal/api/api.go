// Package api serves health checks, the management JSON API and the embedded web UI.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"trackferry/internal/auth"
	"trackferry/internal/cache"
	"trackferry/internal/config"
	"trackferry/internal/feiniu"
	"trackferry/internal/fscheck"
	"trackferry/internal/home"
	"trackferry/internal/library"
	"trackferry/internal/live"
	"trackferry/internal/observe"
	"trackferry/internal/online"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/stream"
	"trackferry/internal/takeover"
	"trackferry/internal/update"
	"trackferry/internal/xiaomi"
)

// Build identifies the running binary.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Options holds everything the handlers need.
type Options struct {
	Config        config.Config
	Store         *store.Store
	Token         string // bearer token for scripts
	SetupCodePath string // file holding the one-time code that authorises the first admin password
	Build         Build
	Boot          store.Boot
	Xiaomi        *xiaomi.Manager
	Played        func(deviceID, title, artist string, track int64) // optional: a song was sent to a speaker
	Dispatcher    *takeover.Dispatcher                              // nil: nothing follows a song
	Observer      *observe.Observer                                 // nil in tests that do not need it
	Engine        *takeover.Engine                                  // nil: no takeover in this server
	Stream        *stream.Server                                    // serves /stream/ to speakers
	Library       *library.Library
	Cache         *cache.Cache // nil: online songs are not kept
	Router        *routing.Router
	Feiniu        *feiniu.Manager   // nil: no 飞牛音乐 in this server
	FnOnline      *FeiniuOnline     // nil: the online search does not ask 飞牛音乐
	Netease       *online.Netease   // nil: no 网易云 login
	Live          *live.Hub         // nil: no live view
	Home          *home.Tracker     // nil: no endpoint for home automation
	Update        *update.Checker   // nil: never looks for a newer release
	SetupLog      func(code string) // prints the setup code where the owner finds it
	Web           http.Handler      // embedded UI; nil serves nothing at /
	Log           *slog.Logger
}

// Server serves the HTTP API.
type Server struct {
	Options
	missMu  sync.Mutex // the list of songs taken off
	started time.Time
	ready   atomic.Bool
	limiter *auth.Limiter
	// xmLimiter throttles Xiaomi password attempts so a typo loop cannot get the account locked.
	xmLimiter *auth.Limiter
	fnLimiter *auth.Limiter // same for 飞牛音乐

	setupMu    sync.Mutex
	setupShown time.Time
}

const (
	sessionCookie = "tf_session"
	sessionTTL    = 30 * 24 * time.Hour
	keptTTL       = 180 * 24 * time.Hour // "保存密码"
	visitTTL      = 12 * time.Hour       // without it, and gone with the browser
	csrfHeader    = "X-Requested-With"
	csrfValue     = "trackferry"
)

// New builds a Server. Call MarkReady once initialisation has finished.
func New(o Options) *Server {
	return &Server{Options: o, started: time.Now(), limiter: auth.NewLimiter(), xmLimiter: auth.NewLimiter(), fnLimiter: auth.NewLimiter()}
}

// MarkReady flips readiness on.
func (s *Server) MarkReady() { s.ready.Store(true) }

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz/live", s.live)
	mux.HandleFunc("GET /healthz/ready", s.readyz)

	mux.HandleFunc("GET /api/session", s.session)
	mux.Handle("POST /api/setup", s.csrf(http.HandlerFunc(s.setup)))
	mux.Handle("POST /api/login", s.csrf(http.HandlerFunc(s.login)))
	mux.Handle("POST /api/logout", s.csrf(http.HandlerFunc(s.logout)))

	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	authed("POST /api/password", s.changePassword)
	authed("GET /api/status", s.status)
	if s.Update != nil {
		authed("GET /api/update", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Update.Status()) })
		authed("POST /api/update", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.Update.Check(r.Context())) })
		authed("PUT /api/update/proxy", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Proxy string `json:"proxy"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "请求格式不对")
				return
			}
			if err := s.Update.SetProxy(in.Proxy); err != nil {
				writeError(w, http.StatusBadRequest, "bad_proxy", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, s.Update.Check(r.Context()))
		})
		authed("POST /api/update/install", func(w http.ResponseWriter, r *http.Request) {
			// Outlives this request.
			if err := s.Update.Install(context.WithoutCancel(r.Context())); err != nil {
				writeError(w, http.StatusConflict, "no_update", err.Error())
				return
			}
			s.Log.Info("更新　开始安装")
			writeJSON(w, http.StatusAccepted, s.Update.Status())
		})
	}
	authed("GET /api/xiaomi", s.xiaomiStatus)
	authed("DELETE /api/xiaomi", s.xiaomiUnlink)
	authed("POST /api/xiaomi/qr", s.qrStart)
	authed("GET /api/xiaomi/qr", s.qrStatus)
	authed("DELETE /api/xiaomi/qr", s.qrCancel)
	authed("GET /api/xiaomi/qr.png", s.qrImage)
	authed("POST /api/xiaomi/password", s.passwordStart)
	authed("GET /api/xiaomi/password", s.passwordStatus)
	authed("DELETE /api/xiaomi/password", s.passwordCancel)
	authed("POST /api/xiaomi/password/captcha", s.passwordCaptcha)
	authed("GET /api/xiaomi/password/captcha.png", s.passwordCaptchaImage)
	authed("POST /api/xiaomi/password/captcha/refresh", s.passwordCaptchaRefresh)
	authed("POST /api/xiaomi/password/verify", s.passwordVerify)
	authed("GET /api/devices", s.devices)
	authed("POST /api/devices/sync", s.devicesSync)
	authed("PATCH /api/devices/{id}", s.deviceUpdate)
	if s.Live != nil {
		authed("GET /api/live", s.liveEvents)
	}
	authed("GET /api/observer", s.observerStatus)
	authed("GET /api/commands", s.commands)
	authed("PATCH /api/commands/{id}", s.commandUpdate)
	authed("POST /api/devices/{id}/takeover-test", s.takeoverTest)
	authed("GET /api/takeover/runs", s.takeoverRuns)
	authed("GET /api/playing", s.playing)
	authed("PUT /api/playing", s.playingUpdate)
	if s.Dispatcher != nil {
		authed("POST /api/devices/{id}/play", s.play)
	}
	if s.Engine != nil {
		authed("POST /api/devices/{id}/control", s.control)
		authed("GET /api/devices/{id}/control", s.controlState)
	}
	if s.Home != nil {
		authed("GET /api/home", s.homeStatus)
		authed("PUT /api/home", s.homeSwitch)
		authed("POST /api/home/token", s.homeToken)
		ha := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.haAuth(h)) }
		ha("GET /api/ha/state", s.haState)
		ha("GET /api/ha/events", s.haEvents)
		if s.Engine != nil {
			ha("POST /api/ha/speakers/{id}/control", s.control)
		}
		if s.Feiniu != nil && s.Library != nil {
			ha("GET /api/ha/cover/{id}", s.cover)
			ha("GET /api/ha/lyrics/{id}", s.haLyrics)
		}
	}
	if s.Library != nil && s.Router != nil {
		authed("GET /api/library", s.libraryStatus)
		authed("GET /api/library/search", s.librarySearch)
		authed("POST /api/library/scan", s.libraryScan)
		authed("GET /api/library/missing", s.libraryMissing)
		authed("GET /api/library/missing.csv", s.libraryMissingCSV)
		authed("POST /api/library/missing/dismiss", s.libraryMissingDismiss)
		authed("POST /api/library/playlists/{guid}/tracks", s.libraryPlaylistAdd)
		authed("GET /api/library/shelf", s.libraryShelf)
		authed("GET /api/library/albums/{id}", s.libraryAlbum)
		authed("GET /api/library/artist", s.libraryArtist)
		authed("GET /api/library/favorites", s.libraryFavorites)
		authed("GET /api/library/playlists", s.libraryPlaylists)
		authed("GET /api/library/playlists/{guid}", s.libraryPlaylist)
		authed("GET /api/library/online", s.librarySearchOnline)
		authed("GET /api/library/tracks/{id}/lyrics", s.libraryLyrics)
		authed("POST /api/route/try", s.routeTry)
		authed("GET /api/intent", s.intentGuide)
		authed("POST /api/intent", s.intentExplain)
		authed("GET /api/library/tracks/{id}/favorite", s.isFavorite)
		authed("PUT /api/library/tracks/{id}/favorite", s.favorite)
		authed("PATCH /api/sources/{name}", s.sourceUpdate)
		if s.Netease != nil {
			authed("GET /api/sources/netease/account", s.neteaseAccount)
			authed("DELETE /api/sources/netease/account", s.neteaseLogout)
			authed("POST /api/sources/netease/account", s.neteaseSignIn)
			authed("POST /api/sources/netease/qr", s.neteaseStart)
			authed("GET /api/sources/netease/qr", s.neteaseQR)
			authed("DELETE /api/sources/netease/qr", s.neteaseCancel)
		}
	}
	if s.Cache != nil {
		authed("GET /api/cache", s.cacheStatus)
		authed("PUT /api/cache", s.cacheLimit)
		authed("DELETE /api/cache", s.cacheClear)
	}
	if s.Feiniu != nil && s.Library != nil {
		authed("GET /api/feiniu", s.feiniuStatus)
		authed("GET /api/feiniu/discover", s.feiniuDiscover)
		authed("POST /api/feiniu", s.feiniuLogin)
		authed("DELETE /api/feiniu", s.feiniuLogout)
		authed("GET /api/cover/{id}", s.cover)
	}
	if s.Stream != nil {
		// Speakers fetch audio without a login; the unguessable token is the access control.
		mux.Handle("GET /stream/", s.Stream)
	}
	if s.FnOnline != nil {
		// Only this process fetches it: the HMAC signature is the access
		// control, and the handler refuses anything but a loopback caller.
		mux.HandleFunc("GET /internal/fn/{guid}", s.FnOnline.serve)
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "没有这个接口")
	})
	if s.Web != nil {
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.Web.ServeHTTP(w, r)
		}))
	}
	return s.logRequests(securityHeaders(mux))
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

type check struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) checks(ctx context.Context) []check {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out := []check{{Name: "initialised", OK: s.ready.Load()}}
	db := check{Name: "database", OK: true}
	if err := s.Store.Ping(ctx); err != nil {
		db.OK, db.Error = false, err.Error()
	}
	data := fscheck.CheckData(s.Config.DataDir)
	// The folder is optional: with 飞牛音乐 nothing is mounted.
	if _, err := os.Stat(s.Config.MusicDir); err == nil {
		music := fscheck.CheckMusic(s.Config.MusicDir)
		out = append(out, check{Name: "music_read_only", OK: music.OK(), Error: music.Error})
	}
	return append(out, db,
		check{Name: "data_writable", OK: data.Writable, Error: data.Error})
}

// readyz reports whether TrackFerry's own prerequisites hold. External services (Xiaomi,
// online sources) are reported elsewhere and never make the process unready.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	checks := s.checks(r.Context())
	code := http.StatusOK
	for _, c := range checks {
		if !c.OK {
			code = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, map[string]any{"ready": code == http.StatusOK, "checks": checks})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := map[string]any{
		"build":              s.Build,
		"started_at":         s.started.UTC(),
		"uptime_s":           int(time.Since(s.started).Seconds()),
		"process":            map[string]int{"uid": os.Getuid(), "gid": os.Getgid()},
		"music":              fscheck.CheckMusic(s.Config.MusicDir),
		"data":               fscheck.CheckData(s.Config.DataDir),
		"current_boot":       s.Boot,
		"takeover_supported": len(s.Config.TakeoverSpeakers) > 0,
	}
	if n, err := s.Store.BootCount(ctx); err == nil {
		resp["boot_count"] = n
	}
	if fb, err := s.Store.FirstBoot(ctx); err == nil {
		resp["first_boot"] = fb
	}
	if v, err := s.Store.SchemaVersion(ctx); err == nil {
		resp["schema_version"] = v
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- admin session ---

func (s *Server) setupRequired(ctx context.Context) (bool, error) {
	_, err := s.Store.AdminCredential(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	return false, err
}

func (s *Server) cookieSession(r *http.Request) bool {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return false
	}
	ok, err := s.Store.SessionValid(r.Context(), ck.Value, time.Now())
	return err == nil && ok
}

func (s *Server) bearer(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	need, err := s.setupRequired(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if need {
		s.showSetupCode()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_required": need,
		"authenticated":  !need && (s.cookieSession(r) || s.bearer(r)),
		"build":          s.Build,
	})
}

// showSetupCode puts the code at the end of the log when somebody opens the setup page, at
// most once every two minutes.
func (s *Server) showSetupCode() {
	if s.SetupLog == nil {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if time.Since(s.setupShown) < 2*time.Minute {
		return
	}
	if code, err := auth.LoadOrCreateSetupCode(s.SetupCodePath); err == nil {
		s.setupShown = time.Now()
		s.SetupLog(code)
	}
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) throttled(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := s.limiter.Allow(clientKey(r), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "throttled", "尝试次数过多，请 "+wait.Round(time.Minute).String()+" 后再试")
		return true
	}
	return false
}

// startSession signs the browser in. A session that is not to outlive the browser leaves the
// cookie without an end.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, ttl time.Duration) error {
	tok := auth.NewToken()
	exp := time.Now().Add(ttl)
	if err := s.Store.CreateSession(r.Context(), tok, exp); err != nil {
		return err
	}
	_ = s.Store.PruneSessions(r.Context(), time.Now())
	ck := &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	}
	if ttl != visitTTL {
		ck.Expires, ck.MaxAge = exp, int(ttl.Seconds())
	}
	http.SetCookie(w, ck)
	return nil
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupCode string `json:"setup_code"`
		Password  string `json:"password"`
	}
	if !readJSON(w, r, &req) || s.throttled(w, r) {
		return
	}
	need, err := s.setupRequired(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !need {
		writeError(w, http.StatusConflict, "already_setup", "管理密码已经设置过了，请直接登录")
		return
	}
	want, err := auth.LoadOrCreateSetupCode(s.SetupCodePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !auth.SetupCodeMatches(want, req.SetupCode) {
		s.limiter.Fail(clientKey(r), time.Now())
		writeError(w, http.StatusForbidden, "bad_setup_code", "初始化码不对。在 NAS 上运行 docker exec trackferry /trackferry setup-code 查看")
		return
	}
	hash, salt, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", "密码至少 8 位")
		return
	}
	if err := s.Store.SetAdminCredential(r.Context(), store.AdminCredential{Hash: hash, Salt: salt, Iterations: auth.Iterations}, false); err != nil {
		writeError(w, http.StatusConflict, "already_setup", "管理密码已经设置过了，请直接登录")
		return
	}
	s.limiter.Reset(clientKey(r))
	_ = os.Remove(s.SetupCodePath) // single use
	s.Log.Info("网页　管理密码已设置")
	if err := s.startSession(w, r, sessionTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Remember *bool  `json:"remember"`
	}
	if !readJSON(w, r, &req) || s.throttled(w, r) {
		return
	}
	cred, err := s.Store.AdminCredential(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "setup_required", "还没有设置管理密码")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !auth.Verify(req.Password, cred.Hash, cred.Salt, cred.Iterations) {
		s.limiter.Fail(clientKey(r), time.Now())
		writeError(w, http.StatusUnauthorized, "bad_password", "密码不对")
		return
	}
	s.limiter.Reset(clientKey(r))
	ttl := time.Duration(sessionTTL)
	if req.Remember != nil && *req.Remember {
		ttl = keptTTL
	} else if req.Remember != nil {
		ttl = visitTTL
	}
	if err := s.startSession(w, r, ttl); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		_ = s.Store.DeleteSession(r.Context(), ck.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) || s.throttled(w, r) {
		return
	}
	cred, err := s.Store.AdminCredential(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !auth.Verify(req.Current, cred.Hash, cred.Salt, cred.Iterations) {
		s.limiter.Fail(clientKey(r), time.Now())
		writeError(w, http.StatusUnauthorized, "bad_password", "当前密码不对")
		return
	}
	hash, salt, err := auth.Hash(req.New)
	if err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", "新密码至少 8 位")
		return
	}
	if err := s.Store.SetAdminCredential(r.Context(), store.AdminCredential{Hash: hash, Salt: salt, Iterations: auth.Iterations}, true); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	_ = s.Store.DeleteAllSessions(r.Context())
	if err := s.startSession(w, r, sessionTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Xiaomi account ---

func (s *Server) xiaomiStatus(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{"account": s.Xiaomi.Status()}
	if qr, ok := s.Xiaomi.QR(); ok {
		resp["qr"] = qr
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) xiaomiUnlink(w http.ResponseWriter, _ *http.Request) {
	if err := s.Xiaomi.Unlink(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.Log.Info("小米　已解除关联")
	writeJSON(w, http.StatusOK, map[string]any{"account": s.Xiaomi.Status()})
}

func xiaomiError(w http.ResponseWriter, err error) {
	kind := xiaomi.KindOf(err)
	msg := map[string]string{
		xiaomi.KindNetwork:  "连不上小米服务器，请检查 NAS 的外网连接",
		xiaomi.KindUpstream: "小米服务器拒绝了请求",
		xiaomi.KindProtocol: "小米接口返回了看不懂的内容，可能接口有变化",
		xiaomi.KindExpired:  "二维码已过期，请刷新",
		xiaomi.KindAuth:     "小米登录已失效，请重新扫码",
	}[kind]
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "xiaomi_" + kind, "message": msg, "detail": err.Error()})
}

func (s *Server) qrStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	st, err := s.Xiaomi.StartQR(ctx)
	if err != nil {
		xiaomiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) qrStatus(w http.ResponseWriter, _ *http.Request) {
	st, ok := s.Xiaomi.QR()
	if !ok {
		writeError(w, http.StatusNotFound, "no_qr", "没有进行中的扫码登录")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) qrCancel(w http.ResponseWriter, _ *http.Request) {
	s.Xiaomi.CancelQR()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) qrImage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var buf bytes.Buffer
	ct, err := s.Xiaomi.QRImage(ctx, &buf)
	if err != nil {
		xiaomiError(w, err)
		return
	}
	if !strings.HasPrefix(ct, "image/") {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// --- devices ---

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.Store.Devices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.deviceViews(devs), "takeover_supported": len(s.Config.TakeoverSpeakers) > 0})
}

func (s *Server) devicesSync(w http.ResponseWriter, r *http.Request) {
	if !s.Xiaomi.Status().Linked {
		writeError(w, http.StatusConflict, "not_linked", "还没有关联小米账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	info := s.Xiaomi.Sync(ctx)
	devs, err := s.Store.Devices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sync": info, "devices": s.deviceViews(devs), "takeover_supported": len(s.Config.TakeoverSpeakers) > 0})
}

func (s *Server) deviceUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string  `json:"mode"`
		Hush *string `json:"hush"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Hush != nil {
		s.deviceHush(w, r, *req.Hush)
		return
	}
	switch req.Mode {
	case store.ModeOff, store.ModeObserve:
	case store.ModeTakeover:
		d, err := s.Store.Device(r.Context(), r.PathValue("id"))
		if err == nil && !s.takeoverAllowed(d) {
			writeError(w, http.StatusConflict, "takeover_unavailable", "这台音箱还没开放接管：先在 config.yaml 的 takeover_speakers 里加上它的名字")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_mode", "mode 只能是 off、observe 或 takeover")
		return
	}
	d, err := s.Store.SetDeviceMode(r.Context(), r.PathValue("id"), req.Mode)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no_device", "没有这台音箱")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.Log.Info("音箱　改了模式", "音箱", d.Name, "模式", d.Mode)
	writeJSON(w, http.StatusOK, s.deviceView(d))
}

type deviceView struct {
	store.Device
	TakeoverAllowed bool `json:"takeover_allowed"`
	CanWake         bool `json:"can_wake"`
	CanHush         bool `json:"can_hush"`
}

func (s *Server) deviceView(d store.Device) deviceView {
	d.Hush = firstNonEmpty(takeover.HushStyle(d.Hush, d.Hardware, d.MiotDID), takeover.HushChosenNormal)
	return deviceView{d, s.takeoverAllowed(d), xiaomi.CanWake(d.Hardware, d.MiotDID), xiaomi.CanHush(d.Hardware, d.MiotDID)}
}

func (s *Server) takeoverAllowed(d store.Device) bool {
	return s.Config.TakeoverAllowed(firstNonEmpty(d.Alias, d.Name))
}

func (s *Server) deviceViews(devs []store.Device) []deviceView {
	out := []deviceView{}
	for _, d := range devs {
		out = append(out, s.deviceView(d))
	}
	return out
}

// --- middleware ---

// csrf requires a custom header on state-changing requests. Browsers cannot add it to a
// cross-site form post, and cross-origin fetches with it need a preflight we never grant.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != csrfValue {
			writeError(w, http.StatusForbidden, "csrf", "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.bearer(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.cookieSession(r) {
			s.csrf(next).ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="trackferry"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/healthz/") || !strings.HasPrefix(r.URL.Path, "/api/") && rec.code < 400 {
			return
		}
		// Reading pages is no event; changes and refusals are.
		level := slog.LevelInfo
		if rec.code >= 400 && rec.code != http.StatusUnauthorized {
			level = slog.LevelWarn
		} else if r.Method == http.MethodGet {
			level = slog.LevelDebug
		}
		s.Log.Log(r.Context(), level, "网页　"+r.Method+" "+r.URL.Path, "状态", rec.code, "用时ms", time.Since(start).Milliseconds())
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "请求格式不对："+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
