package feiniu

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Playlist is one of the account's playlists.
type Playlist struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

const (
	listPage = 300
	listMax  = 6000 // songs read of one list
)

// Playlists lists the account's playlists.
func (m *Manager) Playlists(ctx context.Context) ([]Playlist, error) {
	c, err := m.client()
	if err != nil {
		return nil, err
	}
	var out struct {
		List []Playlist `json:"list"`
	}
	err = c.call(ctx, http.MethodGet, "/playlist/list", nil, nil, &out)
	m.note(c, err)
	return out.List, err
}

// Search asks 飞牛音乐 for songs matching kw. An enhanced 飞牛音乐 answers with
// its library plus the online catalogue it adds (paths under online/), so a hit
// may be a song that has no local file at all.
func (m *Manager) Search(ctx context.Context, kw string) ([]Song, error) {
	c, err := m.client()
	if err != nil {
		return nil, err
	}
	var out struct {
		List []Song `json:"list"`
	}
	q := url.Values{
		"keyword":  {kw},
		"q":        {kw},
		"page":     {"1"},
		"pageSize": {"30"},
		"lan":      {"zh-CN"},
	}
	err = c.call(ctx, http.MethodGet, "/search/track", q, nil, &out)
	m.note(c, err)
	return out.List, err
}

// PlaylistSongs reads a playlist.
func (m *Manager) PlaylistSongs(ctx context.Context, guid string) ([]Song, error) {
	if !idRe.MatchString(guid) {
		return nil, errors.New("bad playlist id")
	}
	return m.all(ctx, "/track/playlist-detail/list", url.Values{"playlistGUID": {guid}})
}

// Favorites reads the account's favourites.
func (m *Manager) Favorites(ctx context.Context) ([]Song, error) {
	return m.all(ctx, "/favorite-track/list", url.Values{"sort": {"favoriteAt,desc"}})
}

func (m *Manager) all(ctx context.Context, path string, q url.Values) ([]Song, error) {
	c, err := m.client()
	if err != nil {
		return nil, err
	}
	var out []Song
	q.Set("size", strconv.Itoa(listPage))
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		var p songPage
		err := c.call(ctx, http.MethodGet, path, q, nil, &p)
		var refused *APIError
		if !errors.As(err, &refused) {
			m.note(c, err)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p.List...)
		if len(p.List) == 0 || len(out) >= p.Total || len(out) >= listMax {
			return out, nil
		}
	}
}

// PutInPlaylist adds a song to a playlist, or takes it out. changed is false when it was
// that way already.
func (m *Manager) PutInPlaylist(ctx context.Context, playlist, guid string, on bool) (changed bool, err error) {
	songs, err := m.PlaylistSongs(ctx, playlist)
	if err != nil {
		return false, err
	}
	in := false
	for _, s := range songs {
		in = in || s.GUID == guid
	}
	if in == on {
		return false, nil
	}
	c, err := m.client()
	if err != nil {
		return false, err
	}
	path := "/playlist/remove-track"
	if on {
		path = "/playlist/add-track"
	}
	err = c.call(ctx, http.MethodPost, path, nil, map[string]any{"guid": playlist, "trackGUIDs": []string{guid}}, nil)
	var refused *APIError
	if !errors.As(err, &refused) {
		m.note(c, err)
	}
	return err == nil, err
}
