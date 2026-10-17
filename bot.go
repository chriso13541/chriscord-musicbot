package main

// bot.go — the bot's connection to the server, through the Chriscord bot
// SDK (github.com/chriso13541/chriscord-bot-sdk): it signs in with the
// bot's token, keeps connected, reads and sends encrypted chat, and does
// the call's encryption. What's here is the music bot's part: which call
// to be in, leaving when alone, and saying what's happening.

import (
	"fmt"
	"sync"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
)

type Bot struct {
	cfg   config
	c     *chriscordbot.Bot
	ytdlp *ytdlp
	pl    *player

	joinMu sync.Mutex // one "get into the right call" at a time (!play)

	mu       sync.Mutex
	aloneT   *time.Timer             // running while the bot sits alone in its channel
	noticeTo string                  // text channel for notices (where it was last told to play)
	searches map[string]searchResult // each person's last !search, for "!play <number>"
}

// chatMessage is a chat message for the commands (already decrypted).
type chatMessage struct {
	BoardID  string
	Username string
	Text     string
	msg      *chriscordbot.Message
}

func newBot(cfg config, c *chriscordbot.Bot, y *ytdlp) *Bot {
	b := &Bot{cfg: cfg, c: c, ytdlp: y, searches: map[string]searchResult{}}
	b.pl = newPlayer(b)
	if p := loadPfp(cfg); p != nil {
		if err := c.SetPicture(p.data, p.updatedAt); err != nil {
			logf("pfp: %v", err)
		}
	}
	c.OnMessage = func(m *chriscordbot.Message) {
		b.onChat(chatMessage{BoardID: m.Channel, Username: m.Author, Text: m.Text, msg: m})
	}
	c.OnVoiceState = func(map[string][]string) { b.checkAlone() }
	c.OnCallReady = func(string) { b.pl.kick() }
	c.OnCallEnded = func(_, reason string) {
		b.stopAlone()
		b.pl.stopAll()
		b.notice(reason + ".")
	}
	c.OnDenied = func(msg string) { b.notice("⚠ " + msg) }
	return b
}

func (b *Bot) username() string { return b.c.Username() }

// waitTurn waits until every chat message that arrived before this one has
// been handled (two quick !plays queue in the order they were sent,
// whichever lookup finishes first).
func (b *Bot) waitTurn(m chatMessage) { b.c.WaitTurn(m.msg) }

func (b *Bot) othersIn(board string) []string    { return b.c.OthersIn(board) }
func (b *Bot) voiceChannelOf(user string) string { return b.c.VoiceChannelOf(user) }
func (b *Bot) inVoice() (string, bool)           { return b.c.InVoice() }
func (b *Bot) speaking(on bool)                  { b.c.SetSpeaking(on) }

// checkAlone starts the countdown when the bot's on its own in its
// channel, and stops it when someone's back.
func (b *Bot) checkAlone() {
	board, _ := b.inVoice()
	alone := board != "" && len(b.othersIn(board)) == 0
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case alone && b.aloneT == nil:
		logf("voice: alone in %s — leaving in %v unless someone comes back", board, b.cfg.AloneAfter)
		b.aloneT = time.AfterFunc(b.cfg.AloneAfter, func() {
			b.mu.Lock()
			b.aloneT = nil
			b.mu.Unlock()
			if now, _ := b.inVoice(); now == board && len(b.othersIn(board)) == 0 {
				b.leave(fmt.Sprintf("Left the call — I'd been on my own for %s.", humanDuration(b.cfg.AloneAfter)))
			}
		})
	case !alone && b.aloneT != nil:
		b.aloneT.Stop()
		b.aloneT = nil
	}
}

func (b *Bot) stopAlone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aloneT != nil {
		b.aloneT.Stop()
		b.aloneT = nil
	}
}

// joinVoice puts the bot in a voice channel.
func (b *Bot) joinVoice(board string) error {
	if err := b.c.JoinVoice(board); err != nil {
		return err
	}
	b.checkAlone()
	b.pl.kick()
	return nil
}

// leave hangs up, empties the queue, and says why in the notice channel
// (if there's a reason to give).
func (b *Bot) leave(reason string) {
	b.stopAlone()
	b.pl.stopAll()
	b.c.LeaveVoice()
	if reason != "" {
		b.notice(reason)
	}
}

// say posts a message in a text channel, encrypted like the app's.
func (b *Bot) say(board, text string) {
	if err := b.c.Send(board, text); err != nil {
		logf("chat: sending to %s: %v", board, err)
	}
}

// notice posts in the channel the bot was last told to play in.
func (b *Bot) notice(text string) {
	b.mu.Lock()
	board := b.noticeTo
	b.mu.Unlock()
	if board != "" {
		go b.say(board, text)
	}
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d%time.Minute == 0 && d < time.Hour {
		n := int(d / time.Minute)
		if n == 1 {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", n)
	}
	return d.String()
}
