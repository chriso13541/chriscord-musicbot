package main

// ytdlp.go — yt-dlp: keeping it current, looking links up, downloading.
//
// The bot keeps its own copy of yt-dlp in <data dir>/bin/yt-dlp — the
// official standalone build for this machine from GitHub releases (it
// includes yt-dlp-ejs, which YouTube needs alongside a JavaScript runtime
// such as Deno). It checks GitHub for a newer release every BOT_YTDLP_CHECK
// (24h by default) and swaps it in, checked against the release's SHA-256
// list. Set BOT_YTDLP to use a yt-dlp of your own instead (then it's left
// alone).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	ytdlpLatestAPI  = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"
	ytdlpLatestPage = "https://github.com/yt-dlp/yt-dlp/releases/latest"
)

type ytdlp struct {
	cfg     config
	path    string
	managed bool
	mu      sync.RWMutex // held for reading while yt-dlp runs, for writing while it's replaced
}

func newYtdlp(cfg config) *ytdlp {
	y := &ytdlp{cfg: cfg}
	if cfg.YtdlpPath != "" {
		y.path = cfg.YtdlpPath
	} else {
		y.path, y.managed = filepath.Join(cfg.DataDir, "bin", "yt-dlp"), true
	}
	return y
}

// releaseAsset is the standalone build for this machine.
func releaseAsset() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "yt-dlp_linux", nil
	case "linux/arm64":
		return "yt-dlp_linux_aarch64", nil
	case "linux/arm":
		return "yt-dlp_linux_armv7l", nil
	case "darwin/amd64", "darwin/arm64":
		return "yt-dlp_macos", nil
	}
	return "", fmt.Errorf("no standalone yt-dlp build for %s/%s — set BOT_YTDLP to one you've installed", runtime.GOOS, runtime.GOARCH)
}

// updateLoop checks for a new release now and then every YtdlpEvery.
func (y *ytdlp) updateLoop() {
	if !y.managed {
		logf("yt-dlp: using %s (BOT_YTDLP) — not updating it", y.path)
		return
	}
	for {
		wait := y.cfg.YtdlpEvery
		if err := y.update(); err != nil {
			logf("yt-dlp: update check failed: %v", err)
			wait = min(wait, time.Hour) // try again sooner
		}
		time.Sleep(wait)
	}
}

func (y *ytdlp) version() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, y.path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// update installs the newest yt-dlp release if this one isn't it. The
// release's SHA2-256SUMS says what the newest build is: when the copy here
// already matches it, there's nothing to do; otherwise the new build is
// downloaded, checked against it, and swapped in.
func (y *ytdlp) update() error {
	asset, err := releaseAsset()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	have := y.version()
	base := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	tag, tagErr := latestTag(client)
	if tagErr == nil {
		if have != "" && have == tag {
			logf("yt-dlp: %s is the latest", have)
			return nil
		}
		base = "https://github.com/yt-dlp/yt-dlp/releases/download/" + tag + "/"
	}
	sums, err := fetch(client, base+"SHA2-256SUMS", 1<<20)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("the latest release lists no checksum for %s", asset)
	}
	if cur, err := os.ReadFile(y.path); err == nil {
		sum := sha256.Sum256(cur)
		if hex.EncodeToString(sum[:]) == want {
			logf("yt-dlp: %s is the latest", have)
			return nil
		}
	}
	bin, err := fetch(client, base+asset, 200<<20)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("the downloaded %s doesn't match its checksum — not installed", asset)
	}
	if err := os.MkdirAll(filepath.Dir(y.path), 0755); err != nil {
		return err
	}
	tmp := y.path + ".new"
	if err := os.WriteFile(tmp, bin, 0755); err != nil {
		return err
	}
	y.mu.Lock()
	err = os.Rename(tmp, y.path)
	y.mu.Unlock()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	now := y.version()
	if have == "" {
		logf("yt-dlp: installed %s", now)
	} else {
		logf("yt-dlp: updated %s → %s", have, now)
	}
	return nil
}

// latestTag asks GitHub for the newest release: its API, or failing that
// (rate limits), where the releases/latest page redirects to.
func latestTag(client *http.Client) (string, error) {
	req, _ := http.NewRequest("GET", ytdlpLatestAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "chriscord-musicbot")
	if resp, err := client.Do(req); err == nil {
		var rel struct {
			Tag string `json:"tag_name"`
		}
		ok := resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&rel) == nil && rel.Tag != ""
		resp.Body.Close()
		if ok {
			return rel.Tag, nil
		}
	}
	noFollow := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ = http.NewRequest("HEAD", ytdlpLatestPage, nil)
	req.Header.Set("User-Agent", "chriscord-musicbot")
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	_, tag, found := strings.Cut(loc, "/releases/tag/")
	if !found || tag == "" || strings.ContainsAny(tag, "/?#") {
		return "", fmt.Errorf("couldn't tell the latest release from GitHub (%s)", resp.Status)
	}
	return tag, nil
}

func fetch(c *http.Client, u string, limit int64) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "chriscord-musicbot")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("too big")
	}
	return data, err
}

// waitReady waits (a while) for a first install to finish.
func (y *ytdlp) ready() bool {
	_, err := os.Stat(y.path)
	if err == nil {
		return true
	}
	if !y.managed {
		_, err = exec.LookPath(y.path)
		return err == nil
	}
	return false
}

// run runs yt-dlp and returns its standard output.
func (y *ytdlp) run(ctx context.Context, args ...string) ([]byte, error) {
	if !y.ready() {
		return nil, errors.New("yt-dlp isn't installed yet (the first download may still be running)")
	}
	y.mu.RLock()
	defer y.mu.RUnlock()
	full := append([]string{"--no-warnings", "--no-progress", "--no-color"}, y.cfg.YtdlpArgs...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, y.path, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s", lastLine(stderr.String(), err.Error()))
	}
	return out, nil
}

func lastLine(s, def string) string {
	var last string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if last == "" {
		return def
	}
	last = strings.TrimPrefix(last, "ERROR: ")
	if len(last) > 300 {
		last = last[:300] + "…"
	}
	return last
}

// trackInfo is one thing to play, as yt-dlp describes it.
type trackInfo struct {
	URL      string
	Title    string
	Duration time.Duration
	Live     bool
}

type ytEntry struct {
	Type       string     `json:"_type"`
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	WebpageURL string     `json:"webpage_url"`
	Title      string     `json:"title"`
	Duration   float64    `json:"duration"`
	IsLive     bool       `json:"is_live"`
	LiveStatus string     `json:"live_status"`
	Entries    []*ytEntry `json:"entries"`
}

// resolve looks up a link (or, for plain words, the top YouTube search
// result) without downloading anything. A playlist gives up to max tracks.
func (y *ytdlp) resolve(ctx context.Context, query string, limit int) ([]trackInfo, bool, error) {
	target := query
	if !looksLikeURL(query) {
		target = "ytsearch1:" + query
	}
	out, err := y.run(ctx, "-J", "--flat-playlist", "--playlist-end", fmt.Sprint(limit), "--", target)
	if err != nil {
		return nil, false, err
	}
	var e ytEntry
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, false, fmt.Errorf("couldn't read yt-dlp's answer: %w", err)
	}
	conv := func(x *ytEntry) trackInfo {
		u := x.WebpageURL
		if u == "" {
			u = x.URL
		}
		title := x.Title
		if title == "" {
			title = u
		}
		return trackInfo{URL: u, Title: title, Duration: time.Duration(x.Duration * float64(time.Second)),
			Live: x.IsLive || x.LiveStatus == "is_live"}
	}
	if e.Type == "playlist" || len(e.Entries) > 0 {
		var out []trackInfo
		for _, x := range e.Entries {
			if x == nil {
				continue
			}
			if t := conv(x); t.URL != "" {
				out = append(out, t)
			}
			if len(out) >= limit {
				break
			}
		}
		if len(out) == 0 {
			return nil, false, errors.New("nothing found")
		}
		return out, strings.HasPrefix(target, "ytsearch") == false, nil
	}
	return []trackInfo{conv(&e)}, false, nil
}

// download fetches a track's audio into dir and returns the file. With
// BOT_AUDIO_FORMAT=mp3 it's converted to MP3; otherwise it's kept as the
// site's best audio (usually Opus already) — one less lossy step.
func (y *ytdlp) download(ctx context.Context, u, dir string) (string, error) {
	args := []string{"--no-playlist", "-f", "bestaudio/best", "--max-filesize", "500M",
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"), "--print", "after_move:filepath"}
	if y.cfg.AudioFormat == "mp3" {
		args = append(args, "-x", "--audio-format", "mp3", "--audio-quality", "0")
	}
	if y.cfg.FFmpeg != "ffmpeg" {
		args = append(args, "--ffmpeg-location", y.cfg.FFmpeg)
	}
	out, err := y.run(ctx, append(args, "--", u)...)
	if err != nil {
		return "", err
	}
	path := lastLine(string(out), "")
	if path == "" {
		return "", errors.New("yt-dlp didn't say where it saved the file")
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("downloaded file missing: %w", err)
	}
	return path, nil
}

func looksLikeURL(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
