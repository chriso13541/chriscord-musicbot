package main

// status.go — the bot's status in the member list: a custom status saying
// what it's playing ("🎶 Song title"), cleared when nothing is.
//
// It goes the way the app's does (presence.go / customstatus.go there):
// one encrypted blob (ws "presence") sealed with the server-wide status key
// ("@presence", shared like a channel's keys), padded to a fixed size and
// sent about once a minute whether it changed or not, plus shortly after a
// change. Keep the format in step with the app's presencePlain.
//
// Servers from before encrypted statuses (no encrypted_presence when
// joining) get no status from the bot.

import (
	"encoding/json"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	presenceKeyID      = "@presence"
	presenceEvery      = 60 * time.Second
	presencePadTo      = 1800 // the app's: every sealed status is this size
	presenceKeyRecheck = 5 * time.Minute
	statusMaxChars     = 128 // the app shows at most this much
)

// presencePlain is what's inside a status blob (the app's format).
type presencePlain struct {
	S string `json:"s"`           // "online" | "idle" | "invisible"
	G string `json:"g,omitempty"` // game
	T string `json:"t,omitempty"` // playing since
	C string `json:"c,omitempty"` // custom status text
	E string `json:"e,omitempty"` // custom status emoji
	P string `json:"p"`           // padding
}

type botStatus struct {
	b       *Bot
	enabled atomic.Bool // the server takes encrypted statuses

	mu    sync.Mutex
	text  string
	emoji string
	kick  chan struct{}
}

func newBotStatus(b *Bot) *botStatus { return &botStatus{b: b, kick: make(chan struct{}, 1)} }

// set changes the custom status ("" and "" clears it); sent shortly.
func (s *botStatus) set(emoji, text string) {
	text = cleanStatusText(text)
	s.mu.Lock()
	changed := s.text != text || s.emoji != emoji
	s.text, s.emoji = text, emoji
	s.mu.Unlock()
	if changed {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// loop publishes while connected (stop closes with the connection).
func (s *botStatus) loop(stop <-chan struct{}) {
	if !s.enabled.Load() {
		return
	}
	s.publish()
	tick := time.NewTicker(presenceEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			s.publish()
		case <-s.kick:
			select { // a moment later, a little at random (as the app does)
			case <-stop:
				return
			case <-time.After(500*time.Millisecond + time.Duration(rand.Intn(1500))*time.Millisecond):
			}
			s.publish()
		}
	}
}

func (s *botStatus) publish() {
	s.mu.Lock()
	p := presencePlain{S: "online", C: s.text, E: s.emoji}
	s.mu.Unlock()
	data := presenceJSON(p)
	id := s.b.text.identity()
	outer, _ := json.Marshal(textPlain{U: id.me, T: string(data)})
	if n := presencePadTo - len(outer); n > 0 {
		p.P = strings.Repeat(" ", n)
		data = presenceJSON(p)
	}
	blob, ok := s.b.text.sealCached(presenceKeyID, string(data), presenceKeyRecheck)
	if !ok {
		var err error
		if blob, err = s.b.text.seal(presenceKeyID, string(data)); err != nil {
			logf("status: can't encrypt it yet: %v", err)
			return
		}
	}
	_ = s.b.sendJSON(map[string]interface{}{"type": "presence", "blob": blob})
}

// presenceJSON: no HTML escaping, so the size is predictable (as the app).
func presenceJSON(p presencePlain) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(p)
	return []byte(strings.TrimRight(b.String(), "\n"))
}

// cleanStatusText: no control characters, at most statusMaxChars (with
// "…" when cut).
func cleanStatusText(t string) string {
	var r []rune
	for _, c := range strings.TrimSpace(t) {
		if !unicode.IsControl(c) {
			r = append(r, c)
		}
	}
	if len(r) > statusMaxChars {
		r = append(r[:statusMaxChars-1], '…')
	}
	return strings.TrimSpace(string(r))
}

// sealCached seals with the board's newest key as last fetched, if that was
// within maxAge and it didn't need replacing then.
func (t *textCrypto) sealCached(board, text string, maxAge time.Duration) (string, bool) {
	id := t.identity()
	t.mu.Lock()
	bk := t.boards[board]
	var key []byte
	gen := 0
	if bk != nil && !bk.rotate && bk.latest > 0 && time.Since(bk.fetchedAt) < maxAge {
		gen, key = bk.latest, bk.keys[bk.latest]
	}
	t.mu.Unlock()
	if key == nil {
		return "", false
	}
	blob, err := sealPlain(key, board, gen, textPlain{U: id.me, T: text})
	return blob, err == nil
}
