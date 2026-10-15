package main

// bot.go — the bot's connection to the server: signing in, the WebSocket
// (reconnecting when it drops), and routing what arrives to chat commands,
// call signalling and call encryption.

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Bot struct {
	cfg   config
	api   *api
	id    ed25519.PrivateKey
	text  *textCrypto
	e2ee  *callCrypto
	ytdlp *ytdlp
	pl    *player
	pfp   *botPfp // its picture (pfp.go), read at startup; nil = none

	joinMu sync.Mutex // one "get into the right call" at a time (!play)

	// Chat messages are numbered as they arrive and handled side by side,
	// but queue in that order: two quick !plays queue in the order they
	// were sent, whichever lookup finishes first (see waitTurn).
	chatSeq  uint64
	turnMu   sync.Mutex
	turnCond *sync.Cond
	turnNext uint64
	turnDone map[uint64]bool

	wsMu sync.Mutex
	ws   *websocket.Conn

	mu         sync.Mutex
	me         string
	channels   map[string][]string // voice_state: board → who's in it
	voiceBoard string              // the voice channel the bot is in ("" = none)
	voice      *voiceSession
	rejoin     bool                    // after a reconnect: get back into voiceBoard
	aloneT     *time.Timer             // running while the bot sits alone in its channel
	noticeTo   string                  // text channel for notices (where it was last told to play)
	searches   map[string]searchResult // each person's last !search, for "!play <number>"
	restartAt  time.Time               // last call connection rebuild (rate limit)
}

func newBot(cfg config, id ed25519.PrivateKey, y *ytdlp) *Bot {
	b := &Bot{cfg: cfg, api: newAPI(cfg.ServerURL), id: id, ytdlp: y, channels: map[string][]string{}, searches: map[string]searchResult{}}
	b.text = newTextCrypto(b.api, id)
	b.e2ee = newCallCrypto(id, b.sendJSON)
	b.pl = newPlayer(b)
	b.pfp = loadPfp(cfg)
	b.turnCond = sync.NewCond(&b.turnMu)
	b.turnDone = map[uint64]bool{}
	return b
}

func (b *Bot) username() string { b.mu.Lock(); defer b.mu.Unlock(); return b.me }

// sendJSON writes one message on the WebSocket.
func (b *Bot) sendJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.wsMu.Lock()
	defer b.wsMu.Unlock()
	if b.ws == nil {
		return fmt.Errorf("not connected")
	}
	b.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return b.ws.WriteMessage(websocket.TextMessage, data)
}

// run keeps the bot connected forever.
func (b *Bot) run() {
	go b.pl.loop()
	backoff := time.Second
	for {
		started := time.Now()
		if err := b.session(); err != nil {
			logf("connection: %v", err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		logf("connection: reconnecting in %v", backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session is one connection: sign in if needed, open the WebSocket, and
// handle it until it ends.
func (b *Bot) session() error {
	if b.api.getToken() == "" {
		name, err := b.api.login(b.cfg, b.id)
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.me = name
		b.mu.Unlock()
		b.text.setUsername(name)
		logf("signed in to %s as %s", b.cfg.ServerURL, name)
	}

	u, err := url.Parse(b.cfg.ServerURL)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	u.RawQuery = url.Values{"token": {b.api.getToken()}, "status": {"online"}}.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("websocket: %w", err)
	}
	b.wsMu.Lock()
	b.ws = conn
	b.wsMu.Unlock()
	b.mu.Lock()
	b.rejoin = b.voiceBoard != ""
	b.mu.Unlock()

	stop := make(chan struct{})
	defer func() {
		close(stop)
		b.wsMu.Lock()
		b.ws = nil
		b.wsMu.Unlock()
		conn.Close()
		b.dropVoiceConnection() // the server takes us out of the call when this connection goes
	}()
	b.announcePfp()
	go b.text.begin()
	go b.text.pendingLoop(stop)
	go func() { // the app pings every 10 s; so does the bot
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.wsMu.Lock()
				if b.ws != nil {
					b.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				}
				b.wsMu.Unlock()
			}
		}
	}()

	// Alive while pings and pongs keep coming (the server pings every 20 s,
	// and answers the bot's), even when nothing else is said for a while.
	alive := func() { conn.SetReadDeadline(time.Now().Add(70 * time.Second)) }
	conn.SetPongHandler(func(string) error { alive(); return nil })
	conn.SetPingHandler(func(data string) error {
		alive()
		b.wsMu.Lock()
		defer b.wsMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	got := 0
	for {
		alive()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if got == 0 {
				// Closed straight away: the session token isn't valid any more.
				b.api.setToken("")
			}
			return fmt.Errorf("websocket closed: %w", err)
		}
		got++
		b.handle(raw)
	}
}

type serverMsg struct {
	Type          string              `json:"type"`
	Target        string              `json:"target"`
	BoardID       string              `json:"board_id"`
	Data          json.RawMessage     `json:"data"`
	Channels      map[string][]string `json:"channels"`
	SDP           string              `json:"sdp"`
	Candidate     string              `json:"candidate"`
	SDPMid        *string             `json:"sdp_mid"`
	SDPMLineIndex *uint16             `json:"sdp_mline_index"`
	Message       string              `json:"message"`
}

type chatMessage struct {
	ID       string  `json:"id"`
	BoardID  string  `json:"board_id"`
	Username string  `json:"username"`
	Content  string  `json:"content"`
	Kind     *string `json:"kind"`
}

func (b *Bot) handle(raw []byte) {
	var m serverMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	me := b.username()
	switch m.Type {
	case "message":
		var cm chatMessage
		if json.Unmarshal(m.Data, &cm) == nil && cm.Username != me && cm.Kind == nil {
			seq := b.chatSeq
			b.chatSeq++
			go b.onChat(cm, seq)
		}
	case "e2ee_changed":
		b.text.changed()
	case "voice_state":
		b.onVoiceState(m.Channels)
	case "voice_e2ee":
		b.e2ee.handleMessage(raw)
	case "voice_answer":
		if s := b.session_(); s != nil {
			s.onAnswer(m.SDP)
		}
	case "voice_ice":
		if s := b.session_(); s != nil {
			s.onICE(m.Candidate, m.SDPMid, m.SDPMLineIndex)
		}
	case "voice_renegotiate":
		if s := b.session_(); s != nil {
			go s.onRenegotiate(m.SDP)
		}
	case "voice_removed":
		if m.Target == me {
			logf("voice: removed from the call (%s)", m.Message)
			go b.leave("")
		}
	case "voice_move":
		if m.Target == me && m.BoardID != "" {
			logf("voice: moved to %s by a moderator", m.BoardID)
			go b.moveTo(m.BoardID)
		}
	case "pfp_request":
		if m.Target == me {
			go b.uploadPfp()
		}
	case "action_denied":
		if m.Target == me {
			logf("server: %s", m.Message)
			b.notice("⚠ " + m.Message)
		}
	case "kicked":
		if m.Target == me {
			logf("server: the bot was kicked")
		}
	}
}

// waitTurn waits until every chat message that arrived before seq has
// been handled (or two minutes, so one stuck lookup can't stall the rest).
func (b *Bot) waitTurn(seq uint64) {
	deadline := time.Now().Add(2 * time.Minute)
	t := time.AfterFunc(2*time.Minute, func() { b.turnMu.Lock(); b.turnCond.Broadcast(); b.turnMu.Unlock() })
	defer t.Stop()
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	for b.turnNext < seq && time.Now().Before(deadline) {
		b.turnCond.Wait()
	}
}

// doneTurn: chat message seq has been handled.
func (b *Bot) doneTurn(seq uint64) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnDone[seq] = true
	for b.turnDone[b.turnNext] {
		delete(b.turnDone, b.turnNext)
		b.turnNext++
	}
	b.turnCond.Broadcast()
}

func (b *Bot) session_() *voiceSession { b.mu.Lock(); defer b.mu.Unlock(); return b.voice }

// othersIn is who else is in a voice channel, by the latest voice_state.
func (b *Bot) othersIn(board string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.othersInLocked(board)
}

func (b *Bot) othersInLocked(board string) []string {
	var out []string
	for _, u := range b.channels[board] {
		if u != b.me {
			out = append(out, u)
		}
	}
	return out
}

// voiceChannelOf is the voice channel someone's in ("" if none).
func (b *Bot) voiceChannelOf(user string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for board, users := range b.channels {
		for _, u := range users {
			if u == user {
				return board
			}
		}
	}
	return ""
}

func (b *Bot) onVoiceState(channels map[string][]string) {
	if channels == nil {
		channels = map[string][]string{}
	}
	b.e2ee.updateRoster(channels)
	b.mu.Lock()
	b.channels = channels
	board := b.voiceBoard
	rejoin := b.rejoin && board != ""
	b.rejoin = false
	b.mu.Unlock()

	if rejoin {
		// Back after a dropped connection: into the same channel, if anyone's
		// still there.
		if len(b.othersIn(board)) == 0 {
			logf("voice: reconnected, but nobody's left in the channel — not rejoining")
			go b.leave("Left the call — everyone had gone while I was reconnecting.")
		} else {
			go func() {
				if err := b.joinVoice(board); err != nil {
					logf("voice: rejoining: %v", err)
				}
			}()
		}
		return
	}
	if board != "" {
		b.checkAlone()
	}
}

// checkAlone starts the countdown when the bot's on its own in its
// channel, and stops it when someone's back.
func (b *Bot) checkAlone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.voiceBoard == "" {
		if b.aloneT != nil {
			b.aloneT.Stop()
			b.aloneT = nil
		}
		return
	}
	alone := len(b.othersInLocked(b.voiceBoard)) == 0
	switch {
	case alone && b.aloneT == nil:
		board := b.voiceBoard
		logf("voice: alone in %s — leaving in %v unless someone comes back", board, b.cfg.AloneAfter)
		b.aloneT = time.AfterFunc(b.cfg.AloneAfter, func() {
			b.mu.Lock()
			still := b.voiceBoard == board && len(b.othersInLocked(board)) == 0
			b.aloneT = nil
			b.mu.Unlock()
			if still {
				b.leave(fmt.Sprintf("Left the call — I'd been on my own for %s.", humanDuration(b.cfg.AloneAfter)))
			}
		})
	case !alone && b.aloneT != nil:
		b.aloneT.Stop()
		b.aloneT = nil
	}
}

// joinVoice puts the bot in a voice channel (or back in it after a
// reconnect), the way the app does: join_voice first, then the offer.
func (b *Bot) joinVoice(board string) error {
	b.mu.Lock()
	old := b.voice
	b.voice = nil
	b.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err := b.sendJSON(map[string]interface{}{
		"type": "join_voice", "board_id": board, "muted": false, "deafened": false,
	}); err != nil {
		return err
	}
	b.mu.Lock()
	b.voiceBoard = board
	b.mu.Unlock()
	return b.startCall(board)
}

// startCall builds the call's connection and sends the offer.
func (b *Bot) startCall(board string) error {
	s, err := newVoiceSession(b, board, b.othersIn(board))
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.voiceBoard != board { // left meanwhile
		b.mu.Unlock()
		s.close()
		return nil
	}
	old := b.voice
	b.voice = s
	b.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err := s.offer(); err != nil {
		return err
	}
	b.checkAlone()
	b.pl.kick()
	return nil
}

// restartCall rebuilds a failed call connection in place (no join_voice:
// the server takes the fresh offer as a refresh).
func (b *Bot) restartCall(s *voiceSession) {
	b.mu.Lock()
	if b.voice != s || b.voiceBoard == "" {
		b.mu.Unlock()
		return
	}
	wait := time.Until(b.restartAt.Add(5 * time.Second))
	b.restartAt = time.Now().Add(max(wait, 0))
	board := b.voiceBoard
	b.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	logf("voice: call connection failed — rebuilding it")
	if err := b.startCall(board); err != nil {
		logf("voice: rebuilding: %v", err)
	}
}

// dropVoiceConnection closes the call's connection (the WebSocket went),
// keeping voiceBoard so the bot rejoins once it's back.
func (b *Bot) dropVoiceConnection() {
	b.mu.Lock()
	s := b.voice
	b.voice = nil
	b.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// leave hangs up, empties the queue, and says why in the notice channel
// (if there's a reason to give).
func (b *Bot) leave(reason string) {
	b.mu.Lock()
	s := b.voice
	was := b.voiceBoard
	b.voice, b.voiceBoard, b.rejoin = nil, "", false
	if b.aloneT != nil {
		b.aloneT.Stop()
		b.aloneT = nil
	}
	b.mu.Unlock()
	b.pl.stopAll()
	if s != nil {
		s.close()
	}
	b.e2ee.end()
	if was != "" {
		_ = b.sendJSON(map[string]string{"type": "leave_voice"})
		logf("voice: left %s", was)
	}
	if reason != "" {
		b.notice(reason)
	}
}

// moveTo follows a moderator's move to another channel.
func (b *Bot) moveTo(board string) {
	b.e2ee.end()
	if err := b.joinVoice(board); err != nil {
		logf("voice: following a move: %v", err)
	}
}

// inVoice is the bot's voice channel and whether its connection is up.
func (b *Bot) inVoice() (board string, ready bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.voiceBoard, b.voice != nil && b.voice.isReady()
}

// say posts a message in a text channel, encrypted like the app's.
func (b *Bot) say(board, text string) {
	content, err := b.text.seal(board, text)
	if err != nil {
		logf("chat: can't encrypt a reply for %s: %v", board, err)
		return
	}
	if err := b.sendJSON(map[string]interface{}{"type": "message", "board_id": board, "content": content}); err != nil {
		logf("chat: sending: %v", err)
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

func (b *Bot) speaking(on bool) {
	board, _ := b.inVoice()
	if board != "" {
		_ = b.sendJSON(map[string]interface{}{"type": "speaking", "board_id": board, "speaking": on})
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
