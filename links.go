package main

// links.go — making sense of what people paste: the many shapes of YouTube
// link (youtu.be, shorts, live, music.youtube.com, links with tracking
// junk, a video inside a playlist, mixes/radio), plus everything else
// yt-dlp knows; and searching.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type linkKind int

const (
	linkOther    linkKind = iota // some other site (or a YouTube channel page): let yt-dlp work it out
	linkVideo                    // one video
	linkPlaylist                 // a playlist (maybe starting at a given video)
	linkRadio                    // a YouTube mix / radio (list=RD…)
)

type link struct {
	URL   string
	Kind  linkKind
	Start string // linkPlaylist: the video it was opened at, to start from
}

var ytIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// parseLink tidies a pasted link and says what kind it is.
func parseLink(raw string) (link, error) {
	raw = strings.TrimSpace(strings.Trim(raw, "<>"))
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return link{}, fmt.Errorf("that doesn't look like a link")
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "m.", "music.", "gaming."} {
		host = strings.TrimPrefix(host, p)
	}
	if host != "youtube.com" && host != "youtu.be" && host != "youtube-nocookie.com" {
		return link{URL: raw, Kind: linkOther}, nil
	}
	q := u.Query()
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")

	// The video, wherever this kind of link keeps it.
	id := ""
	switch {
	case host == "youtu.be":
		id = parts[0]
	case path == "watch":
		id = q.Get("v")
	case len(parts) >= 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed" || parts[0] == "v" || parts[0] == "e"):
		id = parts[1]
	case path == "results":
		return link{}, errSearchLink{q.Get("search_query")}
	}
	if id != "" && !ytIDPattern.MatchString(id) {
		id = ""
	}

	list := q.Get("list")
	switch {
	case list == "WL" || list == "LL" || list == "LM":
		// Watch later / liked: private to whoever pasted it. The video alone.
		if id == "" {
			return link{}, errors.New("that's a private playlist (Watch later / Liked) — the bot can't see it")
		}
		list = ""
	case strings.HasPrefix(list, "RD") && !strings.HasPrefix(list, "RDCLAK"):
		// A mix ("radio"): made around a video, so it needs one.
		if id == "" {
			if v := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(list, "RDAMVM"), "RDMM"), "RD"); ytIDPattern.MatchString(v) {
				id = v
			}
		}
		if id != "" {
			return link{URL: "https://www.youtube.com/watch?v=" + id + "&list=" + url.QueryEscape(list), Kind: linkRadio}, nil
		}
	}
	if list != "" {
		return link{URL: "https://www.youtube.com/playlist?list=" + url.QueryEscape(list), Kind: linkPlaylist, Start: id}, nil
	}
	if id != "" {
		return link{URL: "https://www.youtube.com/watch?v=" + id, Kind: linkVideo}, nil
	}
	// A channel, a /playlist without a list, …: yt-dlp may know.
	return link{URL: raw, Kind: linkOther}, nil
}

// errSearchLink: a youtube.com/results link — treated as a search.
type errSearchLink struct{ query string }

func (e errSearchLink) Error() string { return "a search link" }

// trackInfo is one thing to play, as yt-dlp describes it.
type trackInfo struct {
	URL      string
	Title    string
	Uploader string
	Duration time.Duration
	Live     bool
}

type ytEntry struct {
	Type         string     `json:"_type"`
	IEKey        string     `json:"ie_key"`
	ID           string     `json:"id"`
	URL          string     `json:"url"`
	WebpageURL   string     `json:"webpage_url"`
	Title        string     `json:"title"`
	Channel      string     `json:"channel"`
	Uploader     string     `json:"uploader"`
	Duration     float64    `json:"duration"`
	IsLive       bool       `json:"is_live"`
	LiveStatus   string     `json:"live_status"`
	Availability string     `json:"availability"`
	Entries      []*ytEntry `json:"entries"`
}

// usable says whether a playlist entry is something to play (not a
// private or deleted video, a nested playlist or a channel tab).
func (x *ytEntry) usable() bool {
	if x == nil || x.Type == "playlist" || strings.HasSuffix(x.IEKey, "Tab") {
		return false
	}
	switch x.Title {
	case "[Private video]", "[Deleted video]", "[Unavailable video]":
		return false
	}
	switch x.Availability {
	case "private", "premium_only", "subscriber_only", "needs_auth":
		return false
	}
	return x.LiveStatus != "is_upcoming" && x.trackInfo().URL != ""
}

func (x *ytEntry) trackInfo() trackInfo {
	u := x.WebpageURL
	if u == "" {
		u = x.URL
	}
	// YouTube entries: always the plain watch link (flat entries can be
	// /shorts/… and the like).
	if (x.IEKey == "Youtube" || strings.Contains(u, "youtube.com/") || strings.Contains(u, "youtu.be/")) && ytIDPattern.MatchString(x.ID) {
		u = "https://www.youtube.com/watch?v=" + x.ID
	}
	title := x.Title
	if title == "" {
		title = u
	}
	up := x.Channel
	if up == "" {
		up = x.Uploader
	}
	return trackInfo{URL: u, Title: title, Uploader: up, Duration: time.Duration(x.Duration * float64(time.Second)),
		Live: x.IsLive || x.LiveStatus == "is_live"}
}

type resolved struct {
	tracks  []trackInfo
	kind    linkKind
	skipped int // private, deleted or otherwise unplayable entries left out
	title   string
}

// resolve looks a link up without downloading anything: one video, or up
// to limit tracks of a playlist or mix.
func (y *ytdlp) resolve(ctx context.Context, raw string, limit int) (resolved, error) {
	l, err := parseLink(raw)
	var s errSearchLink
	if errors.As(err, &s) {
		res, err := y.search(ctx, s.query, 1)
		return resolved{tracks: res, kind: linkVideo}, err
	}
	if err != nil {
		return resolved{}, err
	}
	args := []string{"-J"}
	switch l.Kind {
	case linkVideo:
		args = append(args, "--no-playlist")
	default:
		end := limit*2 + 10 // room for private/deleted ones, which are left out
		if l.Start != "" {
			end = limit + 500 // to find where the video sits in it
		}
		args = append(args, "--flat-playlist", "--playlist-end", fmt.Sprint(end))
	}
	out, err := y.run(ctx, append(args, "--", l.URL)...)
	if err != nil {
		if l.Kind == linkRadio && strings.Contains(err.Error(), "not available") {
			// Some mixes only exist for signed-in users: play the video.
			return y.resolve(ctx, strings.SplitN(l.URL, "&list=", 2)[0], limit)
		}
		return resolved{}, err
	}
	var e ytEntry
	if err := json.Unmarshal(out, &e); err != nil {
		return resolved{}, fmt.Errorf("couldn't read yt-dlp's answer: %w", err)
	}
	if e.Type != "playlist" && len(e.Entries) == 0 {
		if e.LiveStatus == "is_upcoming" {
			return resolved{}, errors.New("that stream hasn't started yet")
		}
		return resolved{tracks: []trackInfo{e.trackInfo()}, kind: linkVideo}, nil
	}

	r := resolved{kind: l.Kind, title: e.Title}
	if r.kind == linkOther {
		r.kind = linkPlaylist
	}
	entries := e.Entries
	if l.Start != "" { // start where the link was opened
		for i, x := range entries {
			if x != nil && x.ID == l.Start {
				entries = entries[i:]
				break
			}
		}
	}
	for _, x := range entries {
		if !x.usable() {
			r.skipped++
			continue
		}
		r.tracks = append(r.tracks, x.trackInfo())
		if len(r.tracks) >= limit {
			break
		}
	}
	if len(r.tracks) == 0 {
		return r, errors.New("nothing playable in that playlist")
	}
	return r, nil
}

// search returns up to n YouTube results for words.
func (y *ytdlp) search(ctx context.Context, words string, n int) ([]trackInfo, error) {
	words = strings.TrimSpace(words)
	if words == "" {
		return nil, errors.New("search for what?")
	}
	out, err := y.run(ctx, "-J", "--flat-playlist", "--", fmt.Sprintf("ytsearch%d:%s", n+3, words))
	if err != nil {
		return nil, err
	}
	var e ytEntry
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, fmt.Errorf("couldn't read yt-dlp's answer: %w", err)
	}
	var res []trackInfo
	for _, x := range e.Entries {
		if x.usable() {
			res = append(res, x.trackInfo())
		}
		if len(res) >= n {
			break
		}
	}
	if len(res) == 0 {
		return nil, errors.New("no results")
	}
	return res, nil
}
