package online

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// FeiniuSong is one hit of 飞牛音乐's catalogue.
type FeiniuSong struct {
	GUID       string
	Title      string
	Artist     string
	DurationMS int64
}

// FeiniuCatalogue is what the 飞牛音乐 source needs from the rest of the app.
type FeiniuCatalogue interface {
	// SearchFeiniu asks 飞牛音乐 for songs matching q. An enhanced 飞牛音乐
	// answers with its library plus the online catalogue it adds, so a hit may
	// be a song that has no local file at all.
	SearchFeiniu(ctx context.Context, q string) ([]FeiniuSong, error)
	// FeiniuLink returns an address this process may fetch to read the audio
	// of guid. It carries the 飞牛音乐 login, which must not leave the server.
	FeiniuLink(guid string) string
}

// guidRe accepts both library guids (32 hex) and the ones an enhanced
// 飞牛音乐 gives its online songs (online:track:..., online/kg/...).
var guidRe = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,96}$`)

// feiniuPerSearch bounds what one search returns to the page.
const feiniuPerSearch = 30

// Feiniu plays what 飞牛音乐 can find itself: its own library and the online
// catalogue added by 飞牛音乐增强. The audio is read through 飞牛音乐 over a
// signed loopback address, so whatever 飞牛音乐 can play, a speaker can play.
type Feiniu struct{ Cat FeiniuCatalogue }

// Name is the source name shown in the UI and used by the switch.
func (f *Feiniu) Name() string { return "feiniu" }

// ServesLocally tells the service that this source hands out a loopback address
// which only this process can read: the SSRF guard on the probe must not apply.
func (f *Feiniu) ServesLocally() bool { return true }

// Search asks 飞牛音乐. It stays silent about what it cannot do: an error here
// only means this source missed, the other sources are still asked.
func (f *Feiniu) Search(ctx context.Context, q string) ([]Candidate, error) {
	if f.Cat == nil {
		return nil, ErrNoAudio
	}
	songs, err := f.Cat.SearchFeiniu(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(songs))
	for _, s := range songs {
		if s.Title == "" || !guidRe.MatchString(s.GUID) {
			continue
		}
		out = append(out, Candidate{
			Source:     "feiniu",
			ID:         s.GUID,
			Key:        "feiniu:" + s.GUID,
			Title:      s.Title,
			Artist:     s.Artist,
			Duration:   time.Duration(s.DurationMS) * time.Millisecond,
			DurationMS: s.DurationMS,
		})
		if len(out) >= feiniuPerSearch {
			break
		}
	}
	return out, nil
}

// Resolve names the audio of one song. 飞牛音乐 serves it, so the address is
// the signed loopback one; nothing is downloaded here.
func (f *Feiniu) Resolve(ctx context.Context, c Candidate) (string, error) {
	guid := strings.TrimPrefix(c.Key, "feiniu:")
	if guid == "" {
		guid = c.ID
	}
	if f.Cat == nil || !guidRe.MatchString(guid) {
		return "", ErrNoAudio
	}
	u := f.Cat.FeiniuLink(guid)
	if u == "" {
		return "", ErrNoAudio
	}
	return u, nil
}
