package main

// commands.go — chat commands. Anyone who can see a text channel the bot
// is in can use them there; the ones that change what's playing need you
// to be in the bot's voice channel (like Discord music bots).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Symbols at the start of replies. Plain text characters, drawn in the
// chat's own font and colour: the emoji versions (⏭ ⏸ ⏹) come out as dark
// pictures that disappear on dark backgrounds.
const (
	iconPlay   = "▶"
	iconPause  = "||"
	iconSkip   = ">>"
	iconStop   = "■"
	iconQueued = "+"
)

const helpText = `Music bot commands:
{p}play <link> — join your voice channel and queue it (playlists and mixes too)
{p}play <words> — play the top YouTube result
{p}search <words> — list the top 10, then {p}play <number> to pick one
{p}pause / {p}resume — pause or carry on
{p}skip — next track
{p}stop — stop playing and empty the queue (stays in the call)
{p}clear — empty the queue (the current track keeps playing)
{p}queue — what's playing and what's next
{p}np — what's playing now
{p}disconnect — leave the call`

func (b *Bot) onChat(m chatMessage) {
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, b.cfg.Prefix) {
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(text, b.cfg.Prefix), " ")
	cmd, arg = strings.ToLower(strings.TrimSpace(cmd)), strings.TrimSpace(arg)
	reply := func(format string, a ...interface{}) { b.say(m.BoardID, fmt.Sprintf(format, a...)) }
	logf("chat: %s: %s%s %s", m.Username, b.cfg.Prefix, cmd, arg)

	switch cmd {
	case "play", "p":
		b.cmdPlay(m, arg, reply)
	case "search", "find", "yt":
		b.cmdSearch(m, arg, reply)
	case "pick":
		if b.pendingSearch(m.Username) == nil {
			reply("Search first: %ssearch <words>", b.cfg.Prefix)
		} else {
			b.cmdPlay(m, arg, reply)
		}
	case "stop":
		if b.mustShareCall(m.Username, reply) {
			n := b.pl.clear()
			if t := b.pl.skip(); t != nil {
				reply(iconStop+" Stopped%s.", clearedNote(n))
			} else if n > 0 {
				reply(iconStop+" Cleared the queue (%d track%s).", n, plural(n))
			} else {
				reply("Nothing's playing.")
			}
		}
	case "pause":
		if !b.mustShareCall(m.Username, reply) {
			break
		}
		if b.pl.setPaused(true) {
			reply(iconPause + " Paused.")
		} else if b.pl.view().current == nil {
			reply("Nothing's playing.")
		} else {
			reply("Already paused.")
		}
	case "resume", "unpause", "continue":
		if b.mustShareCall(m.Username, reply) {
			if b.pl.setPaused(false) {
				reply(iconPlay + " Resumed.")
			} else {
				reply("Not paused.")
			}
		}
	case "skip", "s", "next":
		if b.mustShareCall(m.Username, reply) {
			if t := b.pl.skip(); t != nil {
				reply(iconSkip+" Skipped %s.", t.Title)
			} else {
				reply("Nothing's playing.")
			}
		}
	case "clear":
		if b.mustShareCall(m.Username, reply) {
			n := b.pl.clear()
			reply("🧹 Cleared the queue (%d track%s).", n, plural(n))
		}
	case "disconnect", "dc", "leave":
		if b.mustShareCall(m.Username, reply) {
			b.mu.Lock()
			b.noticeTo = m.BoardID
			b.mu.Unlock()
			b.leave("👋 Left the call.")
		}
	case "queue", "q":
		reply("%s", b.queueText(10))
	case "np", "nowplaying":
		v := b.pl.view()
		if v.current == nil {
			reply("Nothing's playing.")
		} else {
			reply("%s", nowLine(v))
		}
	case "version":
		reply("chriscord-musicbot %s", version)
	case "help", "commands", "music":
		reply("%s", strings.ReplaceAll(helpText, "{p}", b.cfg.Prefix))
	}
}

// mustShareCall: the command needs the person in the bot's call.
func (b *Bot) mustShareCall(user string, reply func(string, ...interface{})) bool {
	board, _ := b.inVoice()
	if board == "" {
		reply("I'm not in a call.")
		return false
	}
	if b.voiceChannelOf(user) != board {
		reply("You need to be in my voice channel for that.")
		return false
	}
	return true
}

func (b *Bot) cmdPlay(m chatMessage, arg string, reply func(string, ...interface{})) {
	if arg == "" {
		if b.pl.view().paused {
			if b.mustShareCall(m.Username, reply) && b.pl.setPaused(false) {
				reply(iconPlay + " Resumed.")
			}
		} else {
			reply("Usage: %splay <link or search words>", b.cfg.Prefix)
		}
		return
	}
	theirs := b.voiceChannelOf(m.Username)
	if theirs == "" {
		// (The bot only sees the voice channels it was given when it was
		// added, so someone in another one looks like they're in none.)
		reply("Join a voice channel first, then ask again. (If you're in one, I haven't been given access to it — someone with Manage Bots can add it for me.)")
		return
	}
	mine, _ := b.inVoice()
	if mine != "" && mine != theirs {
		v := b.pl.view()
		if len(b.othersIn(mine)) > 0 && (v.current != nil || len(v.queue) > 0) {
			reply("I'm busy in another voice channel right now.")
			return
		}
	}
	// Only joins a channel with someone in it (they're in it, so: unless
	// they've just left).
	if mine != theirs && len(b.othersIn(theirs)) == 0 {
		reply("Nobody's in that voice channel.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var found resolved
	switch {
	case isPick(arg) && b.pendingSearch(m.Username) != nil:
		// A number after !search: that result.
		res := b.pendingSearch(m.Username)
		n, _ := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if n < 1 || n > len(res) {
			reply("Pick 1–%d from the search.", len(res))
			return
		}
		found = resolved{tracks: []trackInfo{res[n-1]}, kind: linkVideo}
		b.forgetSearch(m.Username)
	case strings.HasPrefix(strings.ToLower(arg), "search "):
		b.cmdSearch(m, arg[len("search "):], reply)
		return
	case looksLikeURL(strings.Trim(arg, "<>")):
		var err error
		if found, err = b.ytdlp.resolve(ctx, arg, b.cfg.MaxPlaylist); err != nil {
			reply("Couldn't play that — %v", friendlyError(err))
			return
		}
	default:
		// Words: the top YouTube result.
		res, err := b.ytdlp.search(ctx, arg, 1)
		if err != nil {
			reply("Couldn't find anything for %q — %v", arg, err)
			return
		}
		found = resolved{tracks: res, kind: linkVideo}
	}
	infos := found.tracks
	playlist := found.kind == linkPlaylist || found.kind == linkRadio
	var ts []*track
	skipped := found.skipped
	tooLong := 0
	for _, in := range infos {
		if !in.Live && b.cfg.MaxLength > 0 && in.Duration > b.cfg.MaxLength {
			tooLong++
			continue
		}
		ts = append(ts, &track{trackInfo: in, requester: m.Username, board: m.BoardID})
	}
	if len(ts) == 0 {
		reply("That's longer than the %s limit.", clockTime(b.cfg.MaxLength))
		return
	}

	// Earlier !plays queue first, even if this lookup was quicker.
	b.waitTurn(m)

	// Into the call, if not there already (one !play at a time decides
	// that, so two at once don't both join).
	b.joinMu.Lock()
	mine, _ = b.inVoice()
	if mine != theirs {
		if len(b.othersIn(theirs)) == 0 {
			b.joinMu.Unlock()
			reply("Nobody's in that voice channel any more.")
			return
		}
		if mine != "" {
			b.pl.stopAll()
		}
		if err := b.joinVoice(theirs); err != nil {
			b.joinMu.Unlock()
			reply("Couldn't join the call — %v", err)
			return
		}
	}
	b.joinMu.Unlock()
	b.mu.Lock()
	b.noticeTo = m.BoardID
	b.mu.Unlock()

	pos, added := b.pl.enqueue(ts)
	switch {
	case added == 0:
		reply("The queue's full (%d tracks).", b.cfg.MaxQueue)
	case playlist:
		what := "the playlist"
		if found.kind == linkRadio {
			what = "the mix"
		}
		if found.title != "" {
			what += " “" + found.title + "”"
		}
		var notes []string
		if skipped > 0 {
			notes = append(notes, fmt.Sprintf("%d unavailable", skipped))
		}
		if tooLong > 0 {
			notes = append(notes, fmt.Sprintf("%d too long", tooLong))
		}
		if added < len(ts) {
			notes = append(notes, fmt.Sprintf("only %d fit in the queue", added))
		}
		note := ""
		if len(notes) > 0 {
			note = " (skipped " + strings.Join(notes, ", ") + ")"
			note = strings.Replace(note, "skipped only", "only", 1)
		}
		reply(iconQueued+" Queued %d track%s from %s%s.", added, plural(added), what, note)
	case pos == 0:
		// Starting now: "Now playing" follows when it does.
	default:
		reply(iconQueued+" Queued %s%s — #%d in line.", ts[0].Title, lengthNote(ts[0].trackInfo), pos)
	}
}

func (b *Bot) queueText(limit int) string {
	v := b.pl.view()
	if v.current == nil && len(v.queue) == 0 {
		return "The queue's empty."
	}
	var sb strings.Builder
	if v.current != nil {
		sb.WriteString(nowLine(v))
	}
	if len(v.queue) == 0 {
		sb.WriteString("\nNothing up next.")
		return sb.String()
	}
	sb.WriteString("\nUp next:")
	var total time.Duration
	for i, t := range v.queue {
		total += t.Duration
		if i < limit {
			fmt.Fprintf(&sb, "\n%d. %s%s — %s", i+1, t.Title, lengthNote(t.trackInfo), t.requester)
		}
	}
	if len(v.queue) > limit {
		fmt.Fprintf(&sb, "\n…and %d more", len(v.queue)-limit)
	}
	fmt.Fprintf(&sb, "\n%d track%s", len(v.queue), plural(len(v.queue)))
	if total > 0 {
		fmt.Fprintf(&sb, ", %s in all", clockTime(total))
	}
	return sb.String()
}

func nowLine(v queueView) string {
	state := iconPlay + " Now playing"
	if v.paused {
		state = iconPause + " Paused"
	}
	pos := clockTime(v.elapsed)
	if v.current.Live {
		pos = "🔴 live, " + pos
	} else if v.current.Duration > 0 {
		pos += " / " + clockTime(v.current.Duration)
	}
	return fmt.Sprintf("%s: %s [%s] — asked for by %s", state, v.current.Title, pos, v.current.requester)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clearedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" and cleared the queue (%d track%s)", n, plural(n))
}

// ── Search ────────────────────────────────────────────────────────────────────

const (
	searchResults = 10
	searchKeep    = 10 * time.Minute // how long "!play <number>" picks from a search
)

type searchResult struct {
	tracks []trackInfo
	at     time.Time
}

func isPick(s string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	return err == nil && n >= 0 && n < 100
}

func (b *Bot) pendingSearch(user string) []trackInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.searches[user]; ok && time.Since(r.at) < searchKeep {
		return r.tracks
	}
	return nil
}

func (b *Bot) forgetSearch(user string) {
	b.mu.Lock()
	delete(b.searches, user)
	b.mu.Unlock()
}

func (b *Bot) cmdSearch(m chatMessage, words string, reply func(string, ...interface{})) {
	words = strings.TrimSpace(words)
	if words == "" {
		reply("Usage: %ssearch <words>", b.cfg.Prefix)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := b.ytdlp.search(ctx, words, searchResults)
	if err != nil {
		logf("search: %q: %v", words, err)
		reply("Couldn't search for %q — %v", words, err)
		return
	}
	logf("search: %q: %d results", words, len(res))
	b.mu.Lock()
	b.searches[m.Username] = searchResult{tracks: res, at: time.Now()}
	b.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔎 Results for %q — %splay <number> to pick one:", words, b.cfg.Prefix)
	for i, t := range res {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, t.Title)
		if t.Uploader != "" {
			sb.WriteString(" — " + t.Uploader)
		}
		sb.WriteString(lengthNote(t))
	}
	reply("%s", sb.String())
}
