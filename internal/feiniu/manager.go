package feiniu

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Status is what the management page shows. It never contains the token.
type Status struct {
	Linked   bool      `json:"linked"`
	Base     string    `json:"base,omitempty"`
	User     string    `json:"user,omitempty"`
	Server   string    `json:"server,omitempty"`
	Version  string    `json:"version,omitempty"`
	LinkedAt time.Time `json:"linked_at,omitzero"`
	Expired  bool      `json:"expired"`
	Error    string    `json:"error,omitempty"`
	Code     bool      `json:"code"` // the NAS asks for an access code
}

// Manager holds the linked account. It is safe for concurrent use.
type Manager struct {
	path string

	mu      sync.RWMutex
	ses     *Session
	cl      *client
	expired bool
	lastErr string

	lyrMu  sync.Mutex
	lyrics map[string]string
}

// NewManager loads a previous session from path.
func NewManager(path string) (*Manager, error) {
	m := &Manager{path: path, lyrics: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil || s.Token == "" {
		return m, nil
	}
	m.ses, m.cl = &s, newClient(s.Base, s.Token, s.Cert)
	m.cl.gate = newGate(s.Code)
	return m, nil
}

// Base normalises what the user typed into a NAS address. Plain HTTP is refused except on
// loopback: the NAS redirects to HTTPS anyway.
func Base(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("请填飞牛地址")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return "", errors.New("飞牛地址不对，例如 https://192.168.1.10:5667")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	if u.Scheme == "http" && !local {
		// The desktop's plain port redirects to its HTTPS neighbour.
		if u.Port() == "5666" || u.Port() == "" {
			u.Host = u.Hostname() + ":5667"
		}
		u.Scheme = "https"
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("飞牛地址只支持 https")
	}
	if u.Scheme == "https" && u.Port() == "" {
		u.Host += ":5667"
	}
	return u.Scheme + "://" + u.Host, nil
}

// Login links an account. The password is used once and not kept; the access code, where
// 飞牛 asks for one, is.
func (m *Manager) Login(ctx context.Context, base, user, password, code string) error {
	base, err := Base(base)
	if err != nil {
		return err
	}
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return errors.New("请填飞牛音乐的用户名和密码")
	}
	m.mu.RLock()
	if code == "" && m.ses != nil && m.ses.Base == base {
		code = m.ses.Code
	}
	m.mu.RUnlock()
	probe := newClient(base, "", "")
	probe.seen = new(string)
	// The certificate is not known yet, so the code waits: whoever answers here could be anyone.
	name, version, err := probe.system(ctx)
	if errors.Is(err, ErrAuth) { // nobody has to sign in for this
		err = ErrGate
	}
	if errors.Is(err, ErrGate) && strings.TrimSpace(code) != "" {
		err = nil
	}
	if err != nil {
		return err
	}
	pin := *probe.seen
	// From here on only the certificate just seen is accepted.
	c := newClient(base, "", pin)
	c.gate = newGate(code)
	if name == "" && version == "" {
		if name, version, err = c.system(ctx); err != nil {
			return err
		}
	}
	b := make([]byte, 16)
	crand.Read(b)
	s := Session{Base: base, User: user, DeviceID: hex.EncodeToString(b), Cert: pin, LinkedAt: time.Now().UTC(), Server: name, Version: version}
	if c.gate.n > 0 { // it did ask
		s.Code = c.gate.code
	}
	if s.Token, err = c.login(ctx, user, password, s.DeviceID); err != nil {
		return err
	}
	cl := newClient(base, s.Token, pin)
	cl.gate = c.gate
	if _, err := cl.songs(ctx, 1, 1); err != nil {
		return err
	}
	raw, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.mu.Lock()
	m.ses, m.cl, m.expired, m.lastErr = &s, cl, false, ""
	m.mu.Unlock()
	return nil
}

// Logout forgets the session.
func (m *Manager) Logout(ctx context.Context) error {
	m.mu.Lock()
	cl := m.cl
	m.ses, m.cl, m.expired, m.lastErr = nil, nil, false, ""
	m.mu.Unlock()
	if cl != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = cl.call(ctx, http.MethodPost, "/user/logout", nil, map[string]string{}, nil)
	}
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Status reports the link.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ses == nil {
		return Status{}
	}
	s := m.ses
	return Status{Linked: true, Base: s.Base, User: s.User, Server: s.Server, Version: s.Version, LinkedAt: s.LinkedAt, Expired: m.expired, Error: m.lastErr, Code: s.Code != ""}
}

// Linked reports whether an account is linked (it may have expired).
func (m *Manager) Linked() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ses != nil
}

// Ready reports whether audio can be expected to open.
func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cl != nil && !m.expired
}

func (m *Manager) client() (*client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cl == nil {
		return nil, ErrNotLinked
	}
	return m.cl, nil
}

// note keeps the link's state up to date with the outcome of a call made on c.
func (m *Manager) note(c *client, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cl != c {
		return
	}
	if err == nil {
		m.expired, m.lastErr = false, ""
		return
	}
	m.lastErr = err.Error()
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrCert) || errors.Is(err, ErrGate) || errors.Is(err, ErrGateCode) {
		m.expired = true
	}
}

// Run looks again whether a login that was refused works after all: 飞牛音乐 also refuses
// while it starts.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st := m.Status(); st.Linked && st.Expired {
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			m.Songs(c, 1, 1)
			cancel()
		}
	}
}

// Songs reads one page of the library.
func (m *Manager) Songs(ctx context.Context, page, size int) ([]Song, int, error) {
	c, err := m.client()
	if err != nil {
		return nil, 0, err
	}
	p, err := c.songs(ctx, page, size)
	m.note(c, err)
	return p.List, p.Total, err
}

// Favorite adds a track to the account's favourites, or takes it out.
func (m *Manager) Favorite(ctx context.Context, guid string, on bool) error {
	c, err := m.client()
	if err != nil {
		return err
	}
	path := "/favorite-track/delete"
	if on {
		path = "/favorite-track/create"
	}
	err = c.call(ctx, http.MethodPost, path, nil, map[string]string{"trackGUID": guid}, nil)
	var refused *APIError
	if errors.As(err, &refused) {
		return err // about this song, not the link
	}
	m.note(c, err)
	return err
}

// 2026-11-25 patch: ':' is allowed because 飞牛音乐增强 lists its platform
// playlists with guids like "online:playlist:plt:kg:6666". The old pattern
// rejected them before the request was sent ("bad playlist id"), so every
// platform playlist failed with 502 even though 飞牛音乐 serves it fine.
var idRe = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,96}$`)

// Cover opens a cover image.
func (m *Manager) Cover(ctx context.Context, id string, size string) (*http.Response, error) {
	c, err := m.client()
	if err != nil {
		return nil, err
	}
	if !idRe.MatchString(id) {
		return nil, errors.New("bad cover id")
	}
	q := url.Values{"coverId": {id}}
	if size != "" {
		q.Set("size", size)
	}
	return c.media(ctx, c.http, "/static/cover", q, "")
}

// Lyrics returns a track's lyrics, usually LRC ("" = none).
func (m *Manager) Lyrics(ctx context.Context, guid string) (string, error) {
	m.lyrMu.Lock()
	if l, ok := m.lyrics[guid]; ok {
		m.lyrMu.Unlock()
		return l, nil
	}
	m.lyrMu.Unlock()
	c, err := m.client()
	if err != nil {
		return "", err
	}
	l, err := c.lyrics(ctx, guid)
	m.note(c, err)
	if err != nil {
		return "", err
	}
	m.lyrMu.Lock()
	if len(m.lyrics) > 200 {
		clear(m.lyrics)
	}
	m.lyrics[guid] = l
	m.lyrMu.Unlock()
	return l, nil
}

// Open opens a track's audio.
func (m *Manager) Open(ctx context.Context, guid, rng string) (*http.Response, error) {
	c, err := m.client()
	if err != nil {
		return nil, err
	}
	resp, err := c.media(ctx, c.audio, "/track/stream", url.Values{"guid": {guid}}, rng)
	m.note(c, err)
	return resp, err
}

// Playable checks that a track's audio opens, by its first byte.
func (m *Manager) Playable(ctx context.Context, guid string) bool {
	resp, err := m.Open(ctx, guid, "bytes=0-0")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	n, _ := io.CopyN(io.Discard, resp.Body, 1)
	return n == 1
}
