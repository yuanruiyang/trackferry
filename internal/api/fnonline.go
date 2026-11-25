package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FeiniuOnline lets the online search use 飞牛音乐's own catalogue. Its audio
// needs the 飞牛音乐 login cookie, and that cookie stays in this process: what
// is handed out is a signed loopback address that only this server answers, so
// neither a speaker nor a browser ever holds the login.
type FeiniuOnline struct {
	open func(ctx context.Context, guid, rng string) (*http.Response, error)
	key  []byte
	port string
	ttl  time.Duration
}

// NewFeiniuOnline builds the loopback signer. listen is this server's own
// listen address, either ":18561" or "192.168.1.10:18561".
func NewFeiniuOnline(open func(ctx context.Context, guid, rng string) (*http.Response, error), listen string, key []byte) *FeiniuOnline {
	port := "18561"
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		port = p
	}
	return &FeiniuOnline{open: open, key: key, port: port, ttl: 10 * time.Minute}
}

// Link signs the address of one song's audio.
func (f *FeiniuOnline) Link(guid string) string {
	exp := time.Now().Add(f.ttl).Unix()
	return "http://127.0.0.1:" + f.port + "/internal/fn/" + url.PathEscape(guid) +
		"?e=" + strconv.FormatInt(exp, 10) + "&s=" + f.sign(guid, exp)
}

func (f *FeiniuOnline) sign(guid string, exp int64) string {
	m := hmac.New(sha256.New, f.key)
	m.Write([]byte(guid))
	m.Write([]byte("|"))
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// serve answers a signed loopback address: the speaker's Range is forwarded to
// 飞牛音乐 and the audio is copied back untouched.
func (f *FeiniuOnline) serve(w http.ResponseWriter, r *http.Request) {
	if f == nil || f.open == nil || !loopback(r.RemoteAddr) {
		http.NotFound(w, r)
		return
	}
	guid := r.PathValue("guid")
	exp, err := strconv.ParseInt(r.URL.Query().Get("e"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		http.NotFound(w, r)
		return
	}
	if !hmac.Equal([]byte(f.sign(guid, exp)), []byte(r.URL.Query().Get("s"))) {
		http.NotFound(w, r)
		return
	}
	resp, err := f.open(r.Context(), guid, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, "飞牛音乐读不到这首歌", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// loopback keeps the signed address from being read by anything but this host.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
