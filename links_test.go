package main

import "testing"

func TestParseLink(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	cases := []struct {
		in    string
		url   string
		kind  linkKind
		start string
	}{
		{"https://www.youtube.com/watch?v=" + id, "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://youtu.be/" + id + "?si=AbCdEf123", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://youtube.com/shorts/" + id + "?feature=share", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://www.youtube.com/live/" + id + "?si=x", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://m.youtube.com/watch?v=" + id + "&t=42s&pp=ygUE", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://music.youtube.com/watch?v=" + id + "&feature=share", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"https://www.youtube-nocookie.com/embed/" + id, "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		{"<https://www.youtube.com/watch?v=" + id + ">", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		// Mixes / radio
		{"https://www.youtube.com/watch?v=" + id + "&list=RD" + id + "&start_radio=1&rv=" + id, "https://www.youtube.com/watch?v=" + id + "&list=RD" + id, linkRadio, ""},
		{"https://music.youtube.com/watch?v=" + id + "&list=RDAMVM" + id, "https://www.youtube.com/watch?v=" + id + "&list=RDAMVM" + id, linkRadio, ""},
		{"https://www.youtube.com/playlist?list=RD" + id, "https://www.youtube.com/watch?v=" + id + "&list=RD" + id, linkRadio, ""},
		{"https://www.youtube.com/watch?v=" + id + "&list=RDMM&index=2", "https://www.youtube.com/watch?v=" + id + "&list=RDMM", linkRadio, ""},
		// Playlists
		{"https://www.youtube.com/playlist?list=PLabc123&si=zzz", "https://www.youtube.com/playlist?list=PLabc123", linkPlaylist, ""},
		{"https://www.youtube.com/watch?v=" + id + "&list=PLabc123&index=7", "https://www.youtube.com/playlist?list=PLabc123", linkPlaylist, id},
		{"https://music.youtube.com/playlist?list=OLAK5uy_abc", "https://www.youtube.com/playlist?list=OLAK5uy_abc", linkPlaylist, ""},
		{"https://music.youtube.com/playlist?list=RDCLAK5uy_abc", "https://www.youtube.com/playlist?list=RDCLAK5uy_abc", linkPlaylist, ""},
		// Private lists: just the video
		{"https://www.youtube.com/watch?v=" + id + "&list=WL&index=3", "https://www.youtube.com/watch?v=" + id, linkVideo, ""},
		// Elsewhere
		{"https://soundcloud.com/artist/track", "https://soundcloud.com/artist/track", linkOther, ""},
		{"https://www.youtube.com/@SomeChannel/videos", "https://www.youtube.com/@SomeChannel/videos", linkOther, ""},
	}
	for _, c := range cases {
		l, err := parseLink(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if l.URL != c.url || l.Kind != c.kind || l.Start != c.start {
			t.Errorf("%s\n got  %s kind=%d start=%q\n want %s kind=%d start=%q", c.in, l.URL, l.Kind, l.Start, c.url, c.kind, c.start)
		}
	}
	if _, err := parseLink("https://www.youtube.com/playlist?list=WL"); err == nil {
		t.Error("Watch later on its own should be refused")
	}
	if _, err := parseLink("https://www.youtube.com/results?search_query=lofi+beats"); err == nil {
		t.Error("a search link should come back as a search")
	} else if s, ok := err.(errSearchLink); !ok || s.query != "lofi beats" {
		t.Errorf("search link: %v", err)
	}
}
