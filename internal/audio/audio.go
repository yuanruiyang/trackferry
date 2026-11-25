// Package audio reads tags and play length from audio files without third-party decoders.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Meta is what a file says about itself.
type Meta struct {
	Title, Artist, Album string
	Duration             time.Duration
	SampleRate, Bits     int
}

// maxTag bounds how much tag data is read into memory (embedded covers can be large).
const maxTag = 8 << 20

var errFormat = errors.New("unrecognised file contents")

// ReadMeta reads a file's tags and audio properties. ext is lower case with the dot.
func ReadMeta(f io.ReadSeeker, ext string, size int64) (Meta, error) {
	switch ext {
	case ".flac":
		return readFLAC(f)
	case ".mp3":
		return readMP3(f, size)
	case ".m4a":
		return readMP4(f, size)
	case ".wav":
		return readWAV(f, size)
	}
	return Meta{}, nil // playable, indexed by file name
}

// ReadFile reads a file's metadata with its text cleaned up.
func ReadFile(p, ext string) (Meta, error) {
	f, err := os.Open(p)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Meta{}, err
	}
	m, err := ReadMeta(f, ext, fi.Size())
	m.Title, m.Artist, m.Album = clean(m.Title), clean(m.Artist), clean(m.Album)
	return m, err
}

func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == 0 || r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if rs := []rune(s); len(rs) > 300 {
		s = string(rs[:300])
	}
	return s
}

// --- FLAC ---

func readFLAC(f io.ReadSeeker) (Meta, error) {
	var m Meta
	var head [10]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return m, errFormat
	}
	start := int64(0)
	if string(head[:3]) == "ID3" { // some taggers prepend ID3 to FLAC
		start = 10 + int64(syncsafe(head[6:10]))
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return m, err
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return m, errFormat
	}
	for range 64 {
		var h [4]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return m, errFormat
		}
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		switch h[0] & 0x7f {
		case 0:
			b := make([]byte, n)
			if _, err := io.ReadFull(f, b); err != nil || n < 18 {
				return m, errFormat
			}
			v := binary.BigEndian.Uint64(b[10:18])
			m.SampleRate = int(v >> 44)
			m.Bits = int(v>>36&0x1f) + 1
			if samples := v & (1<<36 - 1); m.SampleRate > 0 {
				m.Duration = time.Duration(float64(samples) / float64(m.SampleRate) * float64(time.Second))
			}
		case 4:
			if n > maxTag {
				return m, errFormat
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(f, b); err != nil {
				return m, errFormat
			}
			vorbis(b, &m)
		default:
			if _, err := f.Seek(n, io.SeekCurrent); err != nil {
				return m, err
			}
		}
		if h[0]&0x80 != 0 {
			break
		}
	}
	return m, nil
}

func vorbis(b []byte, m *Meta) {
	le := binary.LittleEndian
	next := func() ([]byte, bool) {
		if len(b) < 4 {
			return nil, false
		}
		n := uint64(le.Uint32(b))
		if n > uint64(len(b)-4) {
			return nil, false
		}
		v := b[4 : 4+n]
		b = b[4+n:]
		return v, true
	}
	if _, ok := next(); !ok || len(b) < 4 {
		return
	}
	count := le.Uint32(b)
	b = b[4:]
	var artists []string
	for range min(count, 1000) {
		c, ok := next()
		if !ok {
			break
		}
		k, v, _ := strings.Cut(string(c), "=")
		switch strings.ToUpper(k) {
		case "TITLE":
			if m.Title == "" {
				m.Title = v
			}
		case "ARTIST":
			artists = append(artists, v)
		case "ALBUM":
			if m.Album == "" {
				m.Album = v
			}
		}
	}
	if len(artists) > 0 {
		m.Artist = strings.Join(artists, "/")
	}
}

// --- MP3 ---

func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

func readMP3(f io.ReadSeeker, size int64) (Meta, error) {
	var m Meta
	var head [10]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return m, errFormat
	}
	audio := int64(0)
	if string(head[:3]) == "ID3" {
		n := syncsafe(head[6:10])
		audio = 10 + int64(n)
		b := make([]byte, min(n, maxTag))
		k, _ := io.ReadFull(f, b)
		id3v2(head[3], head[5], b[:k], &m)
	}
	if m.Title == "" && size > 128 {
		var v1 [128]byte
		if _, err := f.Seek(size-128, io.SeekStart); err == nil {
			if _, err := io.ReadFull(f, v1[:]); err == nil && string(v1[:3]) == "TAG" {
				m.Title = legacyText(cstr(v1[3:33]))
				if m.Artist == "" {
					m.Artist = legacyText(cstr(v1[33:63]))
				}
				if m.Album == "" {
					m.Album = legacyText(cstr(v1[63:93]))
				}
			}
		}
	}
	mp3Frames(f, audio, size, &m)
	return m, nil
}

func cstr(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return bytes.TrimRight(b, " ")
}

func id3v2(ver, flags byte, b []byte, m *Meta) {
	if flags&0x80 != 0 && ver < 4 {
		b = bytes.ReplaceAll(b, []byte{0xff, 0x00}, []byte{0xff})
	}
	if flags&0x40 != 0 && ver >= 3 && len(b) > 4 { // extended header
		n := int(binary.BigEndian.Uint32(b)) + 4
		if ver == 4 {
			n = syncsafe(b)
		}
		if n < 0 || n > len(b) {
			return
		}
		b = b[n:]
	}
	idLen, hdr := 4, 10
	if ver == 2 {
		idLen, hdr = 3, 6
	}
	for len(b) >= hdr && b[0] != 0 {
		id := string(b[:idLen])
		var n int
		switch ver {
		case 2:
			n = int(b[3])<<16 | int(b[4])<<8 | int(b[5])
		case 4:
			n = syncsafe(b[4:8])
		default:
			n = int(binary.BigEndian.Uint32(b[4:8]))
		}
		if n < 0 || n > len(b)-hdr {
			return
		}
		body := b[hdr : hdr+n]
		b = b[hdr+n:]
		switch id {
		case "TIT2", "TT2":
			m.Title = id3text(body)
		case "TPE1", "TP1":
			m.Artist = id3text(body)
		case "TALB", "TAL":
			m.Album = id3text(body)
		}
	}
}

func id3text(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	enc, b := b[0], b[1:]
	var s string
	switch enc {
	case 1, 2:
		be := enc == 2
		if len(b) >= 2 {
			if b[0] == 0xff && b[1] == 0xfe {
				be, b = false, b[2:]
			} else if b[0] == 0xfe && b[1] == 0xff {
				be, b = true, b[2:]
			}
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			if be {
				u[i] = binary.BigEndian.Uint16(b[2*i:])
			} else {
				u[i] = binary.LittleEndian.Uint16(b[2*i:])
			}
		}
		s = string(utf16.Decode(u))
	default: // 0 says latin1, 3 says UTF-8; neither is reliable in Chinese files
		s = legacyText(b)
	}
	s = strings.Trim(s, "\x00\ufeff ")
	// Several values are separated by NUL.
	return strings.ReplaceAll(s, "\x00", "/")
}

// legacyText decodes text of unknown 8-bit encoding: UTF-8, then GBK, then latin1.
func legacyText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil && !bytes.ContainsRune(s, utf8.RuneError) {
		for _, r := range string(s) {
			if unicode.Is(unicode.Han, r) {
				return string(s)
			}
		}
	}
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

var (
	mp3Rates   = [2][15]int{{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}, {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}}
	mp3Samples = [3]int{44100, 48000, 32000}
)

// mp3Frames estimates the duration from the first frame (exactly, when it carries a Xing
// header). Only layer III is recognised.
func mp3Frames(f io.ReadSeeker, start, size int64, m *Meta) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	b := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, b)
	frames(b[:n], size-start, m)
}

// ID3Size is the length of the ID3v2 tag at the start of b (0: none).
func ID3Size(b []byte) int64 {
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return 0
	}
	return 10 + int64(syncsafe(b[6:10]))
}

// MP3Duration estimates the play length of an MP3 whose frames start in b and continue for
// size bytes; 0 when b holds no MP3 frames.
// FLACDuration reads the play length of a FLAC stream from its first bytes.
// The STREAMINFO block carries sample rate and total samples; 0 means unknown.
func FLACDuration(b []byte) time.Duration {
	if len(b) < 42 || string(b[:4]) != "fLaC" {
		return 0
	}
	for p, n := 4, 0; p+4 <= len(b) && n < 16; n++ {
		typ := b[p] & 0x7f
		size := int(b[p+1])<<16 | int(b[p+2])<<8 | int(b[p+3])
		p += 4
		if typ == 0 {
			if p+18 > len(b) {
				return 0
			}
			v := binary.BigEndian.Uint64(b[p+10 : p+18])
			rate := v >> 44
			samples := v & 0xfffffffff
			if rate == 0 || samples == 0 {
				return 0
			}
			return time.Duration(samples / rate * uint64(time.Second))
		}
		p += size
	}
	return 0
}

func MP3Duration(b []byte, size int64) time.Duration {
	var m Meta
	frames(b, size, &m)
	return m.Duration
}

func frames(b []byte, size int64, m *Meta) {
	for i := 0; i+200 < len(b); i++ {
		if b[i] != 0xff || b[i+1]&0xe6 != 0xe2 {
			continue
		}
		ver := b[i+1] >> 3 & 3 // 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5
		bi, si := int(b[i+2]>>4), int(b[i+2]>>2&3)
		if ver == 1 || bi == 0 || bi == 15 || si == 3 {
			continue
		}
		rate, kbps, per, side := mp3Samples[si], mp3Rates[0][bi], 1152, 32
		if b[i+3]>>6 == 3 {
			side = 17
		}
		if ver != 3 {
			rate, kbps, per, side = rate/2, mp3Rates[1][bi], 576, 17
			if b[i+3]>>6 == 3 {
				side = 9
			}
			if ver == 0 {
				rate /= 2
			}
		}
		flen := per/8*kbps*1000/rate + int(b[i+2]>>1&1)
		if j := i + flen; j+1 < len(b) && (b[j] != 0xff || b[j+1]&0xe6 != 0xe2) {
			continue // a lone sync pattern, not a frame
		}
		m.SampleRate, m.Bits = rate, 16
		if x := i + 4 + side; x+12 <= len(b) && (string(b[x:x+4]) == "Xing" || string(b[x:x+4]) == "Info") && b[x+7]&1 != 0 {
			frames := binary.BigEndian.Uint32(b[x+8:])
			m.Duration = time.Duration(float64(frames) * float64(per) / float64(rate) * float64(time.Second))
			return
		}
		m.Duration = time.Duration(float64(size-int64(i)) * 8 / float64(kbps*1000) * float64(time.Second))
		return
	}
}

// --- MP4 ---

func readMP4(f io.ReadSeeker, size int64) (Meta, error) {
	var m Meta
	found := false
	err := mp4Boxes(f, 0, size, func(kind string, off, n int64) error {
		if kind == "ftyp" {
			found = true
		}
		if kind != "moov" {
			return nil
		}
		found = true
		return mp4Walk(f, off, n, &m, 0)
	})
	if !found {
		return m, errFormat
	}
	return m, err
}

func mp4Boxes(f io.ReadSeeker, off, n int64, fn func(kind string, off, n int64) error) error {
	end := off + n
	for range 10000 {
		if off+8 > end {
			return nil
		}
		var h [16]byte
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(f, h[:8]); err != nil {
			return errFormat
		}
		l, hl := int64(binary.BigEndian.Uint32(h[:])), int64(8)
		if l == 1 {
			if _, err := io.ReadFull(f, h[8:]); err != nil {
				return errFormat
			}
			l, hl = int64(binary.BigEndian.Uint64(h[8:])), 16
		} else if l == 0 {
			l = end - off
		}
		if l < hl || off+l > end {
			return errFormat
		}
		if err := fn(string(h[4:8]), off+hl, l-hl); err != nil {
			return err
		}
		off += l
	}
	return nil
}

func mp4Walk(f io.ReadSeeker, off, n int64, m *Meta, depth int) error {
	if depth > 6 {
		return nil
	}
	return mp4Boxes(f, off, n, func(kind string, off, n int64) error {
		switch kind {
		case "udta", "ilst":
			return mp4Walk(f, off, n, m, depth+1)
		case "meta":
			if n < 4 {
				return nil
			}
			return mp4Walk(f, off+4, n-4, m, depth+1)
		case "mvhd":
			b, err := readAt(f, off, n, 4096)
			if err != nil || len(b) < 20 {
				return err
			}
			var scale uint32
			var dur uint64
			if b[0] == 1 && len(b) >= 32 {
				scale, dur = binary.BigEndian.Uint32(b[20:]), binary.BigEndian.Uint64(b[24:])
			} else {
				scale, dur = binary.BigEndian.Uint32(b[12:]), uint64(binary.BigEndian.Uint32(b[16:]))
			}
			if scale > 0 {
				m.Duration = time.Duration(float64(dur) / float64(scale) * float64(time.Second))
			}
		case "\xa9nam", "\xa9ART", "\xa9alb":
			b, err := readAt(f, off, n, 64<<10)
			if err != nil || len(b) < 16 || string(b[4:8]) != "data" {
				return nil
			}
			v := string(b[16:])
			switch kind {
			case "\xa9nam":
				m.Title = v
			case "\xa9ART":
				m.Artist = v
			default:
				m.Album = v
			}
		}
		return nil
	})
}

func readAt(f io.ReadSeeker, off, n, limit int64) ([]byte, error) {
	if n > limit {
		return nil, errFormat
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	b := make([]byte, n)
	_, err := io.ReadFull(f, b)
	return b, err
}

// --- WAV ---

func readWAV(f io.ReadSeeker, size int64) (Meta, error) {
	var m Meta
	var h [12]byte
	if _, err := io.ReadFull(f, h[:]); err != nil || string(h[:4]) != "RIFF" || string(h[8:]) != "WAVE" {
		return m, errFormat
	}
	off := int64(12)
	var perSec uint32
	for range 100 {
		var c [8]byte
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			break
		}
		if _, err := io.ReadFull(f, c[:]); err != nil {
			break
		}
		n := int64(binary.LittleEndian.Uint32(c[4:]))
		switch string(c[:4]) {
		case "fmt ":
			b, err := readAt(f, off+8, n, 4096)
			if err != nil || len(b) < 16 {
				return m, errFormat
			}
			m.SampleRate = int(binary.LittleEndian.Uint32(b[4:]))
			perSec = binary.LittleEndian.Uint32(b[8:])
			m.Bits = int(binary.LittleEndian.Uint16(b[14:]))
		case "data":
			n = min(n, size-off-8)
			if perSec > 0 {
				m.Duration = time.Duration(float64(n) / float64(perSec) * float64(time.Second))
			}
		}
		off += 8 + n + n&1
	}
	return m, nil
}

// --- file names ---

var trackNo = regexp.MustCompile(`^(?:CD\s*\d+\s*)?\d{1,3}(?:\s*[-._、]\s*|\s+)`)

// FromName guesses title and artist from "歌手 - 歌名.ext", "01. 歌名.ext" and the like. The
// artist may be empty.
func FromName(rel string) (title, artist string) {
	base := path.Base(rel)
	base = strings.TrimSuffix(base, path.Ext(base))
	if loc := trackNo.FindStringIndex(base); loc != nil && loc[1] < len(base) {
		// "1874.mp3" or "7 Years.mp3" are titles, not numbers: only strip when a
		// separator other than a space follows, or the number has a leading zero.
		num := base[:loc[1]]
		if strings.ContainsAny(num, "-._、") || strings.HasPrefix(num, "0") {
			base = strings.TrimSpace(base[loc[1]:])
		}
	}
	for _, sep := range []string{" - ", " – ", " — ", "－", "-", "_"} {
		if a, b, ok := strings.Cut(base, sep); ok {
			a, b = strings.TrimSpace(a), strings.TrimSpace(b)
			if a != "" && b != "" {
				if _, err := strconv.Atoi(a); err == nil {
					return b, ""
				}
				return b, a
			}
		}
	}
	return strings.TrimSpace(base), ""
}
