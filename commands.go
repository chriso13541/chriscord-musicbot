package main

// commands.go — chat commands. Anyone who can see a text channel the bot
// is in can use them there; the ones that change what's playing need you
// to be in the bot's voice channel (like Discord music bots).

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const helpText = `Music bot commands:
{p}play <link or search words> — join your voice channel and queue it (playlists work too)
{p}pause / {p}resume — pause or carry on
{p}skip — next track
{p}queue — what's playing and what's next
{p}np — what's playing now
{p}clear — empty the queue (the current track keeps playing)
{p}disconnect — stop and leave the call`

func (b *Bot) onChat(m chatMessage, seq uint64) {
	defer b.doneTurn(seq)
	text, ok := b.text.open(m.BoardID, m.Username, m.Content)
	if !ok {
		return
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, b.cfg.Prefix) {
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(text, b.cfg.Prefix), " ")
	cmd, arg = strings.ToLower(strings.TrimSpace(cmd)), strings.TrimSpace(arg)
	reply := func(format string, a ...interface{}) { b.say(m.BoardID, fmt.Sprintf(format, a...)) }

	switch cmd {
	case "play", "p":
		b.cmdPlay(m, arg, seq, reply)
	case "pause":
		if !b.mustShareCall(m.Username, reply) {
			break
		}
		if b.pl.setPaused(true) {
			reply("⏸ Paused.")
		} else if b.pl.view().current == nil {
			reply("Nothing's playing.")
		} else {
			reply("Already paused.")
		}
	case "resume", "unpause", "continue":
		if b.mustShareCall(m.Username, reply) {
			if b.pl.setPaused(false) {
				reply("▶ Resumed.")
			} else {
				reply("Not paused.")
			}
		}
	case "skip", "s", "next":
		if b.mustShareCall(m.Username, reply) {
			if t := b.pl.skip(); t != nil {
				reply("⏭ Skipped %s.", t.Title)
			} else {
				reply("Nothing's playing.")
			}
		}
	case "clear":
		if b.mustShareCall(m.Username, reply) {
			n := b.pl.clear()
			reply("🧹 Cleared the queue (%d track%s).", n, plural(n))
		}
	case "disconnect", "dc", "leave", "stop":
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

func (b *Bot) cmdPlay(m chatMessage, arg string, seq uint64, reply func(string, ...interface{})) {
	if arg == "" {
		if b.pl.view().paused {
			if b.mustShareCall(m.Username, reply) && b.pl.setPaused(false) {
				reply("▶ Resumed.")
			}
		} else {
			reply("Usage: %splay <link or search words>", b.cfg.Prefix)
		}
		return
	}
	theirs := b.voiceChannelOf(m.Username)
	if theirs == "" {
		reply("Join a voice channel first, then ask again.")
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
	infos, playlist, err := b.ytdlp.resolve(ctx, arg, b.cfg.MaxPlaylist)
	if err != nil {
		reply("Couldn't find that — %v", err)
		return
	}
	var ts []*track
	skipped := 0
	for _, in := range infos {
		if in.Live {
			skipped++
			continue
		}
		if b.cfg.MaxLength > 0 && in.Duration > b.cfg.MaxLength {
			skipped++
			continue
		}
		ts = append(ts, &track{trackInfo: in, requester: m.Username, board: m.BoardID})
	}
	if len(ts) == 0 {
		if len(infos) == 1 && infos[0].Live {
			reply("Live streams aren't supported.")
		} else {
			reply("That's longer than the %s limit.", clockTime(b.cfg.MaxLength))
		}
		return
	}

	// Earlier !plays queue first, even if this lookup was quicker.
	b.waitTurn(seq)

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
			b.e2ee.end()
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
		note := ""
		if skipped > 0 {
			note = fmt.Sprintf(" (skipped %d: live or too long)", skipped)
		}
		if added < len(ts) {
			note += fmt.Sprintf(" — only %d fit in the queue", added)
		}
		reply("➕ Queued %d track%s from the playlist%s.", added, plural(added), note)
	case pos == 0:
		// Starting now: "Now playing" follows when it does.
	default:
		reply("➕ Queued %s%s — #%d in line.", ts[0].Title, lengthNote(ts[0].Duration), pos)
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
			fmt.Fprintf(&sb, "\n%d. %s%s — %s", i+1, t.Title, lengthNote(t.Duration), t.requester)
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
	state := "▶ Now playing"
	if v.paused {
		state = "⏸ Paused"
	}
	pos := clockTime(v.elapsed)
	if v.current.Duration > 0 {
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
