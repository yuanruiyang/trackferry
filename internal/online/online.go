// Package online finds songs on online platforms through their own public endpoints.
//
// Songs the platform restricts are never resolved by other means: no paid-content parsers.
// The one account used is the owner's own, on 网易云. What comes back is probed, and trial
// clips are refused.
package online

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"trackferry/internal/audio"
	"trackferry/internal/names"
)

// Reading is one way to understand a request. Artist may be empty.
type Reading struct {
	Title  string
	Artist string
}

// Candidate is a song a platform offers.
type Candidate struct {
	Source     string        `json:"source"`
	ID         string        `json:"-"`
	Key        string        `json:"key,omitempty"` // names it to Take
	ref        string        // second platform ID, when resolving needs one
	Title      string        `json:"title"`
	Artist     string        `json:"artist"`
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"duration_ms"`
	Restricted bool          `json:"restricted"` // members only or paid
	Member     bool          `json:"-"`          // played by the rights of an account: never kept
	Note       string        `json:"note,omitempty"`
	Why        string        `json:"why,omitempty"` // not the song asked for
}

// Stream is a resolved, probed audio address.
type Stream struct {
	URL      string
	Duration time.Duration
	Type     string
}

// LocalAudio marks a source whose audio is served by this machine itself
// (飞牛音乐 over its signed loopback address). The private-address guard on the
// probe exists to stop a public platform from steering requests inward; such a
// source is trusted past it, nothing else is.
type LocalAudio interface{ ServesLocally() bool }

func servesLocally(src Source) bool {
	l, ok := src.(LocalAudio)
	return ok && l.ServesLocally()
}

// Source is one platform.
type Source interface {
	Name() string
	Search(ctx context.Context, query string) ([]Candidate, error)
	// Resolve returns the audio address of a candidate (not probed yet).
	Resolve(ctx context.Context, c Candidate) (string, error)
}

// Kinds of failure.
var (
	ErrRestricted = errors.New("会员或付费歌曲")
	ErrTrial      = errors.New("只有试听片段")
	ErrNoAudio    = errors.New("平台没有给出可播音频")
	ErrLimited    = errors.New("平台限流")
)

// States of a layer.
const (
	StateHit        = "hit"
	StateMiss       = "miss"
	StateAmbiguous  = "ambiguous"
	StateRestricted = "restricted" // found, but no full audio may be played
	StateError      = "error"
	StateTimeout    = "timeout"
	StateDisabled   = "disabled"
	StateOpen       = "open"    // circuit breaker: not asked
	StateSkipped    = "skipped" // not needed
)

// Outcome is one platform's part in a lookup.
type Outcome struct {
	Source     string      `json:"source"`
	State      string      `json:"state"`
	MS         int64       `json:"ms"`
	Detail     string      `json:"detail,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

// Found is a playable song.
type Found struct {
	Candidate Candidate
	Stream    Stream
}

// Health is a platform's recent record.
type Health struct {
	Source    string `json:"source"`
	Enabled   bool   `json:"enabled"`
	OK        int    `json:"ok"`
	Failed    int    `json:"failed"`
	LastMS    int64  `json:"last_ms"`
	LastAt    int64  `json:"last_at_ms"`
	LastError string `json:"last_error,omitempty"`
	OpenUntil int64  `json:"open_until_ms,omitempty"`
}

type entry struct {
	src     Source
	enabled bool
	h       Health
	fails   int
	open    time.Time
}

// Limits of one lookup.
const (
	searchTimeout  = 2200 * time.Millisecond
	totalBudget    = 5 * time.Second
	breakAfter     = 4
	breakFor       = time.Minute
	minFullShare   = 0.85
	maxPerSource   = 3
	shortestSong   = 45 * time.Second
	shownPerSource = 6
)

// Service asks all platforms. It is safe for concurrent use.
type Service struct {
	log   *slog.Logger
	probe *http.Client
	local *http.Client // for sources that serve audio from this machine
	mu    sync.Mutex
	srcs  []*entry
	sem   chan struct{}

	offMu   sync.Mutex
	offered map[string]Candidate // shown by Search, until picked
	order   []string
}

// New returns a Service over sources in order of preference, all enabled.
func New(log *slog.Logger, client *http.Client, sources ...Source) *Service {
	s := &Service{log: log, probe: client, local: localClient(), sem: make(chan struct{}, 4)}
	for _, x := range sources {
		s.srcs = append(s.srcs, &entry{src: x, enabled: true, h: Health{Source: x.Name()}})
	}
	return s
}

// SetEnabled switches a platform; false for an unknown name.
func (s *Service) SetEnabled(name string, on bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.srcs {
		if e.src.Name() == name {
			e.enabled = on
			return true
		}
	}
	return false
}

// Health lists all platforms.
func (s *Service) Health() []Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Health{}
	for _, e := range s.srcs {
		h := e.h
		h.Enabled = e.enabled
		if time.Now().Before(e.open) {
			h.OpenUntil = e.open.UnixMilli()
		}
		out = append(out, h)
	}
	return out
}

func (s *Service) record(e *entry, took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.h.LastMS, e.h.LastAt = took.Milliseconds(), time.Now().UnixMilli()
	if err == nil {
		e.h.OK++
		e.fails, e.h.LastError = 0, ""
		return
	}
	e.h.Failed++
	e.h.LastError = describe(err)
	if e.fails++; e.fails >= breakAfter {
		e.open, e.fails = time.Now().Add(breakFor), 0
	}
}

// Describe is err for people, without addresses.
func Describe(err error) string { return describe(err) }

func describe(err error) string {
	var r refused
	if errors.As(err, &r) {
		return r.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "超时"
	}
	for _, k := range []error{ErrRestricted, ErrTrial, ErrNoAudio, ErrLimited} {
		if errors.Is(err, k) {
			return k.Error()
		}
	}
	var he httpError
	if errors.As(err, &he) {
		return he.Error()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "超时"
		}
		return "连不上平台"
	}
	return "平台返回了看不懂的内容"
}

// reason is describe with what the platform's answer looked like, for the log. Other errors
// may carry addresses.
func reason(err error) string {
	for _, k := range []error{ErrRestricted, ErrTrial, ErrNoAudio} {
		if errors.Is(err, k) {
			return err.Error()
		}
	}
	return describe(err)
}

type progressKey struct{}

// WithProgress has f called with every change of a platform's outcome during Find, from
// several goroutines.
func WithProgress(ctx context.Context, f func(Outcome)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// StateAsking is an outcome still being worked out.
const StateAsking = "asking"

type hit struct {
	c     Candidate
	named bool // the request names the artist
}

// Find looks a song up on every enabled platform. request is the whole utterance.
func (s *Service) Find(ctx context.Context, readings []Reading, request string) (*Found, []Outcome) {
	tell, _ := ctx.Value(progressKey{}).(func(Outcome))
	if tell == nil {
		tell = func(Outcome) {}
	}
	ctx, cancel := context.WithTimeout(ctx, totalBudget)
	defer cancel()
	outs := make([]Outcome, len(s.srcs))
	hits := make([][]hit, len(s.srcs))
	query := strings.TrimSpace(readings[0].Title + " " + readings[0].Artist)
	if len(readings) > 1 {
		query = readings[len(readings)-1].Title // the whole phrase
	}
	var wg sync.WaitGroup
	for i, e := range s.srcs {
		outs[i] = Outcome{Source: e.src.Name(), Candidates: []Candidate{}}
		s.mu.Lock()
		on, open := e.enabled, time.Now().Before(e.open)
		s.mu.Unlock()
		if !on {
			outs[i].State = StateDisabled
			continue
		}
		if open {
			outs[i].State, outs[i].Detail = StateOpen, "连续失败，暂停询问一会儿"
			continue
		}
		wg.Go(func() {
			start := time.Now()
			tell(Outcome{Source: e.src.Name(), State: StateAsking, Detail: "正在搜", Candidates: []Candidate{}})
			sctx, done := context.WithTimeout(ctx, searchTimeout)
			defer done()
			s.sem <- struct{}{}
			cands, err := s.search(sctx, e.src, query)
			<-s.sem
			if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.record(e, time.Since(start), err)
			}
			outs[i].MS = time.Since(start).Milliseconds()
			if err != nil {
				outs[i].State, outs[i].Detail = StateError, describe(err)
				if outs[i].Detail == "超时" {
					outs[i].State = StateTimeout
				}
				tell(outs[i])
				return
			}
			outs[i].Candidates = slices.Clone(cands[:min(shownPerSource, len(cands))])
			hits[i] = choose(cands, readings, request)
			explain(outs[i].Candidates, hits[i], readings, request)
			if len(hits[i]) == 0 {
				outs[i].State = StateMiss
				if n := len(cands); n > 0 {
					outs[i].Detail = fmt.Sprintf("搜到 %d 首，没有一首对得上", n)
				}
				tell(outs[i])
			} else {
				o := outs[i]
				o.State, o.Detail = StateAsking, "正在核对能不能放"
				tell(o)
			}
		})
	}
	wg.Wait()

	// Nobody named the singer: the platforms' first choices should agree whose song it is.
	// When they do not, the more preferred platform's choice is tried first.
	split := false
	var first *hit
	for i := range hits {
		if len(hits[i]) == 0 || hits[i][0].named {
			continue
		}
		h := &hits[i][0]
		if first == nil {
			first = h
			continue
		}
		if !shareArtist(first.c.Artist, h.c.Artist) {
			split = true
			break
		}
	}

	type result struct {
		i int
		f *Found
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	ch := make(chan result, len(s.srcs))
	turn := make(chan struct{}) // closed when the next platform may go, if they take turns
	close(turn)
	n := 0
	for i, e := range s.srcs {
		if len(hits[i]) == 0 {
			continue
		}
		n++
		mine, next := turn, make(chan struct{})
		if split {
			turn = next
		}
		go func() {
			defer close(next)
			<-mine
			start := time.Now()
			f, state, detail := s.resolve(rctx, e, hits[i])
			outs[i].State, outs[i].Detail = state, detail
			outs[i].MS += time.Since(start).Milliseconds()
			if split && f != nil {
				outs[i].Detail = "没说歌手，原唱说法不一，先放这家的首选"
				stop()
			}
			if f != nil || rctx.Err() == nil {
				tell(outs[i])
			}
			ch <- result{i, f}
		}()
	}
	var found *Found
	for range n {
		r := <-ch // every worker ends by the budget
		if r.f != nil && found == nil {
			found = r.f
			stop()
		} else if found != nil && outs[r.i].State != StateRestricted {
			outs[r.i].State, outs[r.i].Detail = StateSkipped, "另一家先给出了完整音频"
			tell(outs[r.i])
		}
	}
	return found, outs
}

func shareArtist(a, b string) bool {
	for _, x := range names.Artists(a) {
		for _, y := range names.Artists(b) {
			if x == y {
				return true
			}
		}
	}
	return false
}

// choose keeps the candidates that are the requested song, in the platform's order.
func choose(cands []Candidate, readings []Reading, request string) []hit {
	var named, plain []hit
	canon := ""
	for _, c := range cands {
		if names.VersionPenalty(c.Title, request) > 0 || strings.Contains(c.Title, "免费试听") {
			continue
		}
		t, _ := names.SplitTitle(c.Title)
		titles := []string{names.Norm(c.Title), names.Norm(t)}
		ok, withArtist := false, false
		for _, r := range readings {
			want := names.Norm(r.Title)
			for _, title := range titles {
				if title == "" {
					continue
				}
				if r.Artist != "" {
					withArtist = withArtist || title == want && names.ArtistMatch(c.Artist, r.Artist)
					continue
				}
				ok = ok || title == want
				for _, a := range names.Artists(c.Artist) {
					withArtist = withArtist || a+title == want || title+a == want
				}
			}
		}
		switch {
		case withArtist:
			named = append(named, hit{c, true})
		case ok && (canon == "" || shareArtist(canon, c.Artist)):
			canon = c.Artist
			plain = append(plain, hit{c, false})
		}
	}
	if len(named) > 0 {
		return named
	}
	return plain
}

// explain notes why shown candidates are not the song asked for.
func explain(shown []Candidate, hs []hit, readings []Reading, request string) {
next:
	for i := range shown {
		c := &shown[i]
		for _, h := range hs {
			if h.c.ID == c.ID {
				continue next
			}
		}
		t, _ := names.SplitTitle(c.Title)
		same := false
		for _, r := range readings {
			same = same || names.Norm(r.Title) == names.Norm(c.Title) || names.Norm(r.Title) == names.Norm(t)
		}
		switch {
		case names.VersionPenalty(c.Title, request) > 0 || strings.Contains(c.Title, "免费试听"):
			c.Why = "现场、翻唱、试听这类版本"
		case !same:
			c.Why = "歌名对不上"
		default:
			c.Why = "歌手对不上"
		}
	}
}

func (s *Service) resolve(ctx context.Context, e *entry, hs []hit) (*Found, string, string) {
	state, detail := StateRestricted, ErrRestricted.Error()
	for i, h := range hs {
		if i == maxPerSource {
			break
		}
		if ctx.Err() != nil {
			return nil, StateSkipped, ""
		}
		if h.c.Restricted {
			continue
		}
		st, err := s.resolveOne(ctx, e.src, h.c)
		if err == nil {
			return &Found{Candidate: h.c, Stream: st}, StateHit, ""
		}
		state, detail = StateError, describe(err)
		switch {
		case errors.Is(err, ErrRestricted), errors.Is(err, ErrTrial), errors.Is(err, ErrNoAudio):
			state = StateRestricted
			s.log.Info("音源　跳过一首", "音源", e.src.Name(), "原因", reason(err))
		case detail == "超时":
			state = StateTimeout
			s.record(e, searchTimeout, err)
		default:
			s.log.Warn("音源　取不到音频", "音源", e.src.Name(), "类型", detail)
		}
	}
	return nil, state, detail
}

// search and address ask a source. What it answers is not ours to trust: a fault while
// reading it is that source's error.
func (s *Service) search(ctx context.Context, src Source, q string) (c []Candidate, err error) {
	defer s.fault(&err)
	return src.Search(ctx, q)
}

func (s *Service) address(ctx context.Context, src Source, c Candidate) (u string, err error) {
	defer s.fault(&err)
	return src.Resolve(ctx, c)
}

func (s *Service) fault(err *error) {
	if p := recover(); p != nil {
		s.log.Error("在线　程序出错", "原因", fmt.Sprint(p), "位置", string(debug.Stack()))
		*err = errors.New("返回的内容读不懂")
	}
}

func (s *Service) resolveOne(ctx context.Context, src Source, c Candidate) (Stream, error) {
	st, err := s.tryOne(ctx, src, c)
	free, ok := src.(interface {
		ResolveFree(context.Context, Candidate) (string, error)
	})
	var he httpError
	if !ok || ctx.Err() != nil || !errors.Is(err, ErrNoAudio) && !errors.Is(err, ErrRestricted) && !errors.As(err, &he) {
		return st, err
	}
	again, aerr := s.tryOne(ctx, freely{src, free.ResolveFree}, c)
	if aerr != nil {
		return Stream{}, err
	}
	return again, nil
}

// freely is a source asked without its session.
type freely struct {
	Source
	resolve func(context.Context, Candidate) (string, error)
}

func (f freely) Resolve(ctx context.Context, c Candidate) (string, error) { return f.resolve(ctx, c) }

// ServesLocally carries the source's own answer across the wrapper.
func (f freely) ServesLocally() bool { return servesLocally(f.Source) }

func (s *Service) tryOne(ctx context.Context, src Source, c Candidate) (Stream, error) {
	u, err := s.address(ctx, src, c)
	if err != nil {
		return Stream{}, err
	}
	return s.check(ctx, u, c.Duration, servesLocally(src))
}

// check probes an address: it must answer with audio about as long as the song.
func (s *Service) check(ctx context.Context, u string, want time.Duration, local bool) (Stream, error) {
	body, total, ctype, err := s.fetch(ctx, u, 0, local)
	if err != nil {
		return Stream{}, err
	}
	off := int64(0)
	if n := audio.ID3Size(body); n > 0 {
		off = n
		if n+4096 > int64(len(body)) { // a big cover picture before the audio
			if n >= total {
				return Stream{}, ErrNoAudio
			}
			if body, _, _, err = s.fetch(ctx, u, n, local); err != nil {
				return Stream{}, err
			}
		} else {
			body = body[n:]
		}
	}
	d := audio.MP3Duration(body, total-off)
	if d == 0 {
		return Stream{}, fmt.Errorf("%w（不是 MP3，类型 %.24q，%d 字节）", ErrNoAudio, ctype, total)
	}
	if d < shortestSong && (want == 0 || want >= shortestSong) || want > 0 && float64(d) < minFullShare*float64(want) {
		return Stream{}, ErrTrial
	}
	return Stream{URL: u, Duration: d, Type: ctype}, nil
}

func (s *Service) fetch(ctx context.Context, u string, from int64, local bool) (b []byte, total int64, ctype string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, "", ErrNoAudio
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, from+32<<10-1))
	hc := s.probe
	if local {
		hc = s.local
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	ctype = resp.Header.Get("Content-Type")
	if resp.StatusCode >= 400 {
		return nil, 0, "", httpError(resp.StatusCode)
	}
	if strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "json") {
		return nil, 0, "", fmt.Errorf("%w（给的是网页，HTTP %d）", ErrNoAudio, resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		_, t, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if _, err := fmt.Sscan(t, &total); err != nil {
			return nil, 0, "", ErrNoAudio
		}
	case http.StatusOK:
		if from > 0 {
			return nil, 0, "", ErrNoAudio
		}
		total = resp.ContentLength
	default:
		return nil, 0, "", httpError(resp.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	return b, total, ctype, err
}

// Opener returns a function that opens the song's bytes, asking the platform for a fresh
// address when the current one has expired.
func (s *Service) Opener(f *Found) func(ctx context.Context, rng string) (*http.Response, error) {
	var mu sync.Mutex
	u := f.Stream.URL
	var src Source
	for _, e := range s.srcs {
		if e.src.Name() == f.Candidate.Source {
			src = e.src
		}
	}
	hc := s.probe
	if servesLocally(src) {
		hc = s.local
	}
	return func(ctx context.Context, rng string) (*http.Response, error) {
		for try := 0; ; try++ {
			mu.Lock()
			cur := u
			mu.Unlock()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("User-Agent", userAgent)
			if rng != "" {
				req.Header.Set("Range", rng)
			}
			resp, err := hc.Do(req)
			if err == nil && (resp.StatusCode < 400 || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable) {
				return resp, nil
			}
			if err == nil {
				resp.Body.Close()
				err = httpError(resp.StatusCode)
			}
			if try > 0 || ctx.Err() != nil || src == nil {
				return nil, err
			}
			rctx, cancel := context.WithTimeout(ctx, totalBudget)
			st, rerr := s.resolveOne(rctx, src, f.Candidate)
			cancel()
			if rerr != nil {
				return nil, rerr
			}
			mu.Lock()
			u = st.URL
			mu.Unlock()
		}
	}
}

type httpError int

func (e httpError) Error() string { return fmt.Sprintf("平台返回 HTTP %d", int(e)) }

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// hosts are the only domains TrackFerry talks to, directly or through redirects.
var hosts = []string{"kuwo.cn", "kugou.com", "migu.cn", "music.163.com", "music.126.net"}

func allowedHost(h string) bool {
	for _, d := range hosts {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// Client returns the HTTP client for the platforms: it only reaches their own domains and
// never a private address, whatever an answer or redirect says.
// localClient may reach this machine. The private-address guard exists for
// public platforms, and would also refuse the loopback address a local source
// hands out, so this one has no such guard.
func localClient() *http.Client {
	d := &net.Dialer{Timeout: 3 * time.Second}
	tr := &http.Transport{DialContext: d.DialContext, ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}
	return &http.Client{Transport: tr}
}

func Client() *http.Client {
	d := &net.Dialer{Timeout: 3 * time.Second, Control: func(_, addr string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return err
		}
		if ip := ap.Addr().Unmap(); ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("refusing a private address")
		}
		return nil
	}}
	tr := &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 4 * time.Second,
		MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute, ForceAttemptHTTP2: true}
	return &http.Client{Transport: guard{tr}, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > 4 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

type guard struct{ next http.RoundTripper }

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if !allowedHost(r.URL.Hostname()) || r.URL.Scheme != "http" && r.URL.Scheme != "https" {
		return nil, errors.New("host not allowed")
	}
	return g.next.RoundTrip(r)
}

// maxOffered bounds the songs shown that can still be picked.
const maxOffered = 600

// Search lists what the enabled platforms offer for a free text, interleaved, and the
// platforms that did not answer.
func (s *Service) Search(ctx context.Context, q string) ([]Candidate, []Outcome) {
	lists := make([][]Candidate, len(s.srcs))
	outs := make([]Outcome, len(s.srcs))
	var wg sync.WaitGroup
	for i, e := range s.srcs {
		outs[i] = Outcome{Source: e.src.Name(), Candidates: []Candidate{}}
		s.mu.Lock()
		on, open := e.enabled, time.Now().Before(e.open)
		s.mu.Unlock()
		switch {
		case !on:
			outs[i].State = StateDisabled
			continue
		case open:
			outs[i].State, outs[i].Detail = StateOpen, "连续失败，暂停询问一会儿"
			continue
		}
		wg.Go(func() {
			start := time.Now()
			c, done := context.WithTimeout(ctx, 4*time.Second)
			defer done()
			cands, err := s.search(c, e.src, q)
			if ctx.Err() == nil {
				s.record(e, time.Since(start), err)
			}
			outs[i].MS = time.Since(start).Milliseconds()
			switch {
			case err != nil:
				outs[i].State, outs[i].Detail = StateError, describe(err)
			case len(cands) == 0:
				outs[i].State = StateMiss
			default:
				outs[i].State, lists[i] = StateHit, cands
			}
		})
	}
	wg.Wait()
	out := []Candidate{}
	for n := 0; ; n++ {
		more := false
		for _, l := range lists {
			if n < len(l) {
				out, more = append(out, l[n]), true
			}
		}
		if !more || len(out) >= 60 {
			break
		}
	}
	s.offMu.Lock()
	defer s.offMu.Unlock()
	if s.offered == nil {
		s.offered = map[string]Candidate{}
	}
	for i := range out {
		c := &out[i]
		c.Key = c.Source + ":" + c.ID
		if _, ok := s.offered[c.Key]; !ok {
			s.order = append(s.order, c.Key)
		}
		s.offered[c.Key] = *c
	}
	for len(s.order) > maxOffered {
		delete(s.offered, s.order[0])
		s.order = s.order[1:]
	}
	return out, outs
}

// ErrGone is a song that was not shown, or too long ago.
var ErrGone = errors.New("搜索结果过期了，请重新搜索")

// Take gets a song ready that Search has shown. The error is for people.
func (s *Service) Take(ctx context.Context, key string) (*Found, error) {
	s.offMu.Lock()
	c, ok := s.offered[key]
	s.offMu.Unlock()
	if !ok {
		return nil, ErrGone
	}
	for _, e := range s.srcs {
		if e.src.Name() != c.Source {
			continue
		}
		s.mu.Lock()
		on := e.enabled
		s.mu.Unlock()
		if !on {
			return nil, errors.New("这个音源关着")
		}
		ctx, cancel := context.WithTimeout(ctx, 2*totalBudget)
		defer cancel()
		st, err := s.resolveOne(ctx, e.src, c)
		if err != nil {
			s.log.Warn("音源　点的歌放不了", "音源", c.Source, "原因", reason(err))
			return nil, errors.New(describe(err))
		}
		return &Found{Candidate: c, Stream: st}, nil
	}
	return nil, ErrGone
}
