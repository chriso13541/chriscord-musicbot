//go:build integration

package main

// integration_test.go — runs the real bot against a stand-in for the
// Chriscord server (the parts the bot uses: sign-in, channel keys, the
// WebSocket, voice signalling with a real WebRTC peer, key relay), with a
// pretend member "alice" who types commands and listens. Checks that what
// reaches the call is the bot's audio, decryptable with the key it gave
// alice, at real-time pace; and the commands, the queue, and leaving when
// alone.
//
//	go test -tags integration -v -run TestBot
//
// Needs ffmpeg and yt-dlp (BOT_YTDLP, or it fetches one into a temp dir).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

// ── The stand-in server ───────────────────────────────────────────────────────

type mockUser struct {
	name, pub   string
	encKey, sig *string
	conn        *websocket.Conn
	wmu         sync.Mutex
	local       func(map[string]interface{}) // a pretend member's inbox (alice)
}

type boxRec struct {
	gen                    int
	recipient, sender, box string
}

type mock struct {
	t      *testing.T
	mu     sync.Mutex
	users  map[string]*mockUser // by name
	tokens map[string]string    // token → name
	nonces map[string]string
	voice  map[string]string // user → board
	latest map[string]int
	boxes  map[string][]boxRec // board → boxes
	pcs    map[string]*webrtc.PeerConnection
	tap    chan []byte // RTP payloads the bot sends
	spk    []bool
	pfp    []byte // the bot's uploaded picture
	pfpAt  float64
	srv    *httptest.Server
}

func newMock(t *testing.T) *mock {
	m := &mock{t: t, users: map[string]*mockUser{}, tokens: map[string]string{}, nonces: map[string]string{},
		voice: map[string]string{}, latest: map[string]int{}, boxes: map[string][]boxRec{},
		pcs: map[string]*webrtc.PeerConnection{}, tap: make(chan []byte, 100000)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/challenge", m.challenge)
	mux.HandleFunc("POST /api/join", m.join)
	mux.HandleFunc("PUT /api/e2ee/enc-key", m.encKey)
	mux.HandleFunc("GET /api/boards/{b}/keys", m.getKeys)
	mux.HandleFunc("POST /api/boards/{b}/keys", m.postKeys)
	mux.HandleFunc("GET /api/e2ee/pending", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"pending":[]}`)) })
	mux.HandleFunc("POST /api/e2ee/boxes", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("/ws", m.ws)
	m.srv = httptest.NewServer(mux)
	return m
}

func jsonOut(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (m *mock) me(r *http.Request) *mockUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users[m.tokens[r.Header.Get("X-Session-Token")]]
}

func (m *mock) challenge(w http.ResponseWriter, r *http.Request) {
	var b struct {
		PublicKey string `json:"public_key"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	n := make([]byte, 32)
	rand.Read(n)
	m.mu.Lock()
	m.nonces[hex.EncodeToString(n)] = b.PublicKey
	m.mu.Unlock()
	jsonOut(w, map[string]string{"nonce": hex.EncodeToString(n)})
}

func (m *mock) join(w http.ResponseWriter, r *http.Request) {
	var b struct{ Username, PublicKey, Nonce, Signature string }
	var raw map[string]string
	json.NewDecoder(r.Body).Decode(&raw)
	b.Username, b.PublicKey, b.Nonce, b.Signature = raw["username"], raw["public_key"], raw["nonce"], raw["signature"]
	pk, _ := hex.DecodeString(b.PublicKey)
	n, _ := hex.DecodeString(b.Nonce)
	sig, _ := hex.DecodeString(b.Signature)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nonces[b.Nonce] != b.PublicKey || !ed25519.Verify(pk, n, sig) {
		w.WriteHeader(401)
		jsonOut(w, map[string]string{"error": "bad signature"})
		return
	}
	delete(m.nonces, b.Nonce)
	if m.users[b.Username] == nil {
		m.users[b.Username] = &mockUser{name: b.Username, pub: b.PublicKey}
	}
	tok := hex.EncodeToString(n[:16])
	m.tokens[tok] = b.Username
	jsonOut(w, map[string]string{"token": tok, "username": b.Username})
}

func (m *mock) addLocal(name string, priv ed25519.PrivateKey, inbox func(map[string]interface{})) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[name] = &mockUser{name: name, pub: hex.EncodeToString(priv.Public().(ed25519.PublicKey)), local: inbox}
	tok := "tok-" + name
	m.tokens[tok] = name
	return tok
}

func (m *mock) encKey(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	var b map[string]string
	json.NewDecoder(r.Body).Decode(&b)
	k, s := b["enc_key"], b["sig"]
	m.mu.Lock()
	u.encKey, u.sig = &k, &s
	m.mu.Unlock()
	m.broadcast(map[string]interface{}{"type": "e2ee_changed"})
	w.Write([]byte(`{}`))
}

func (m *mock) memberLocked(name string) textMember {
	u := m.users[name]
	return textMember{Username: u.name, PublicKey: u.pub, EncKey: u.encKey, Sig: u.sig}
}

func (m *mock) getKeys(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	board := r.PathValue("b")
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := map[string]interface{}{"latest": m.latest[board], "rotate": false}
	var boxes []map[string]interface{}
	for _, b := range m.boxes[board] {
		if strings.EqualFold(b.recipient, u.pub) {
			boxes = append(boxes, map[string]interface{}{"generation": b.gen, "box": b.box, "sender": m.memberLocked(b.sender)})
		}
	}
	var members []textMember
	for n := range m.users {
		members = append(members, m.memberLocked(n))
	}
	resp["boxes"], resp["members"] = boxes, members
	jsonOut(w, resp)
}

func (m *mock) postKeys(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	board := r.PathValue("b")
	var b struct {
		Generation int                               `json:"generation"`
		Boxes      []struct{ Recipient, Box string } `json:"boxes"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	m.mu.Lock()
	if b.Generation != m.latest[board]+1 {
		m.mu.Unlock()
		w.WriteHeader(409)
		jsonOut(w, map[string]string{"error": "generation taken"})
		return
	}
	m.latest[board] = b.Generation
	for _, x := range b.Boxes {
		m.boxes[board] = append(m.boxes[board], boxRec{b.Generation, x.Recipient, u.name, x.Box})
	}
	m.mu.Unlock()
	w.Write([]byte(`{}`))
}

// send delivers to one user.
func (m *mock) send(u *mockUser, v map[string]interface{}) {
	if u.local != nil {
		go u.local(v)
		return
	}
	if u.conn == nil {
		return
	}
	data, _ := json.Marshal(v)
	u.wmu.Lock()
	u.conn.WriteMessage(websocket.TextMessage, data)
	u.wmu.Unlock()
}

func (m *mock) broadcast(v map[string]interface{}) {
	m.mu.Lock()
	us := make([]*mockUser, 0, len(m.users))
	for _, u := range m.users {
		us = append(us, u)
	}
	m.mu.Unlock()
	for _, u := range us {
		m.send(u, v)
	}
}

func (m *mock) to(name string, v map[string]interface{}) {
	m.mu.Lock()
	u := m.users[name]
	m.mu.Unlock()
	if u != nil {
		v["target"] = name
		m.send(u, v)
	}
}

func (m *mock) voiceState() {
	m.mu.Lock()
	ch := map[string][]string{}
	for u, b := range m.voice {
		ch[b] = append(ch[b], u)
	}
	m.mu.Unlock()
	m.broadcast(map[string]interface{}{"type": "voice_state", "channels": ch, "reconnecting": map[string]interface{}{}})
}

func (m *mock) ws(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	name := m.tokens[r.URL.Query().Get("token")]
	u := m.users[name]
	m.mu.Unlock()
	if u == nil {
		return
	}
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	m.mu.Lock()
	u.conn = c
	m.mu.Unlock()
	m.voiceState()
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			m.mu.Lock()
			delete(m.voice, name)
			u.conn = nil
			m.mu.Unlock()
			m.voiceState()
			return
		}
		var v map[string]interface{}
		json.Unmarshal(raw, &v)
		m.handle(name, v)
	}
}

// handle: one message from a member (the bot, or alice via sendAs).
func (m *mock) handle(name string, v map[string]interface{}) {
	str := func(k string) string { s, _ := v[k].(string); return s }
	switch str("type") {
	case "message":
		m.broadcast(map[string]interface{}{"type": "message", "data": map[string]interface{}{
			"id": fmt.Sprint(time.Now().UnixNano()), "board_id": str("board_id"), "username": name, "content": str("content"),
		}})
	case "join_voice":
		m.mu.Lock()
		m.voice[name] = str("board_id")
		m.mu.Unlock()
		m.voiceState()
	case "leave_voice":
		m.mu.Lock()
		delete(m.voice, name)
		pc := m.pcs[name]
		delete(m.pcs, name)
		m.mu.Unlock()
		if pc != nil {
			pc.Close()
		}
		m.voiceState()
	case "voice_e2ee":
		m.mu.Lock()
		board := m.voice[name]
		pub := m.users[name].pub
		var targets []string
		for u, b := range m.voice {
			if b == board && u != name && (str("to") == "" || str("to") == u) {
				targets = append(targets, u)
			}
		}
		m.mu.Unlock()
		for _, t := range targets {
			msg := map[string]interface{}{"type": "voice_e2ee", "board_id": board, "from": name, "from_key": pub, "data": v["data"]}
			if str("to") != "" {
				msg["to"] = str("to")
			}
			m.mu.Lock()
			u := m.users[t]
			m.mu.Unlock()
			m.send(u, msg)
		}
	case "voice_offer":
		go m.offer(name, str("sdp"))
	case "voice_ice":
		m.mu.Lock()
		pc := m.pcs[name]
		m.mu.Unlock()
		if pc != nil {
			idx := uint16(0)
			if f, ok := v["sdp_mline_index"].(float64); ok {
				idx = uint16(f)
			}
			mid := str("sdp_mid")
			pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: str("candidate"), SDPMid: &mid, SDPMLineIndex: &idx})
		}
	case "pfp_info":
		m.mu.Lock()
		have := m.pfpAt
		m.mu.Unlock()
		if at, _ := v["pfp_updated_at"].(float64); at > 0 && at != have {
			m.to(name, map[string]interface{}{"type": "pfp_request"})
		}
	case "pfp_upload":
		data, _ := base64.StdEncoding.DecodeString(str("pfp_data"))
		m.mu.Lock()
		m.pfp, m.pfpAt = data, v["pfp_updated_at"].(float64)
		m.mu.Unlock()
	case "speaking":
		m.mu.Lock()
		m.spk = append(m.spk, v["speaking"] == true)
		m.mu.Unlock()
	}
}

func (m *mock) offer(name, sdp string) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		m.t.Errorf("mock pc: %v", err)
		return
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			m.tap <- append([]byte(nil), p.Payload...)
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		m.to(name, map[string]interface{}{"type": "voice_ice", "candidate": j.Candidate, "sdp_mid": j.SDPMid, "sdp_mline_index": j.SDPMLineIndex})
	})
	m.mu.Lock()
	if old := m.pcs[name]; old != nil {
		old.Close()
	}
	m.pcs[name] = pc
	m.mu.Unlock()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		m.t.Errorf("mock remote: %v", err)
		return
	}
	ans, _ := pc.CreateAnswer(nil)
	pc.SetLocalDescription(ans)
	m.to(name, map[string]interface{}{"type": "voice_answer", "sdp": ans.SDP})
}

// ── Alice ─────────────────────────────────────────────────────────────────────

type alice struct {
	m     *mock
	text  *textCrypto
	call  *callCrypto
	inbox chan string // what the bot says, decrypted
	seen  []string    // said, but not looked for yet
}

func newAlice(t *testing.T, m *mock) *alice {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &alice{m: m, inbox: make(chan string, 100)}
	tok := m.addLocal("alice", priv, a.receive)
	ap := newAPI(m.srv.URL)
	ap.setToken(tok)
	a.text = newTextCrypto(ap, priv)
	a.text.setUsername("alice")
	a.call = newCallCrypto(priv, func(v interface{}) error {
		data, _ := json.Marshal(v)
		var mv map[string]interface{}
		json.Unmarshal(data, &mv)
		m.handle("alice", mv)
		return nil
	})
	a.text.ensurePublished()
	return a
}

func (a *alice) receive(v map[string]interface{}) {
	switch v["type"] {
	case "message":
		d := v["data"].(map[string]interface{})
		if d["username"] == "alice" {
			return
		}
		text, ok := a.text.open(d["board_id"].(string), d["username"].(string), d["content"].(string))
		if !ok {
			text = "<couldn't open>"
		}
		a.inbox <- text
	case "voice_state":
		ch := map[string][]string{}
		raw, _ := json.Marshal(v["channels"])
		json.Unmarshal(raw, &ch)
		a.call.updateRoster(ch)
	case "voice_e2ee":
		raw, _ := json.Marshal(v)
		a.call.handleMessage(raw)
	}
}

func (a *alice) say(t *testing.T, board, text string) {
	c, err := a.text.seal(board, text)
	if err != nil {
		t.Fatalf("alice can't encrypt: %v", err)
	}
	a.m.handle("alice", map[string]interface{}{"type": "message", "board_id": board, "content": c})
}

// expect waits for the bot to say something containing want.
func (a *alice) expect(t *testing.T, want string, within time.Duration) string {
	t.Helper()
	for i, s := range a.seen {
		if strings.Contains(s, want) {
			a.seen = append(a.seen[:i], a.seen[i+1:]...)
			return s
		}
	}
	deadline := time.After(within)
	for {
		select {
		case s := <-a.inbox:
			t.Logf("bot says: %q", s)
			if strings.Contains(s, want) {
				return s
			}
			a.seen = append(a.seen, s)
		case <-deadline:
			t.Fatalf("bot never said %q", want)
		}
	}
}

// open decrypts one of the bot's audio frames with the keys it gave alice.
func (a *alice) open(frame []byte) ([]byte, bool) {
	sealed, kid, _, err := frameHeader(kindAudio, frame)
	if err != nil {
		return nil, false
	}
	a.call.mu.Lock()
	p := a.call.peers["Music"]
	var keys []*peerKey
	if p != nil {
		keys = append(keys, p.keys...)
	}
	a.call.mu.Unlock()
	for _, k := range keys {
		if byte(k.kid) == kid {
			pt, err := openSealed(k.aead, kindAudio, sealed)
			return pt, err == nil
		}
	}
	return nil, false
}

// ── The test ──────────────────────────────────────────────────────────────────

func TestBot(t *testing.T) {
	work := t.TempDir()
	// Two short test tracks served over HTTP (yt-dlp's generic extractor).
	media := filepath.Join(work, "media")
	os.MkdirAll(media, 0755)
	for i, freq := range []int{440, 660} {
		out := filepath.Join(media, fmt.Sprintf("tone%d.mp3", i+1))
		if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i",
			fmt.Sprintf("sine=frequency=%d:duration=20", freq), "-c:a", "libmp3lame", out).CombinedOutput(); err != nil {
			t.Fatalf("making test audio: %v %s", err, b)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.FileServer(http.Dir(media)))
	mediaURL := "http://" + ln.Addr().String()

	m := newMock(t)
	defer m.srv.Close()
	const text, voiceA = "text-1", "voice-1"
	a := newAlice(t, m)

	cfg := config{ServerURL: m.srv.URL, Username: "Music", DataDir: filepath.Join(work, "bot"), Prefix: "!",
		AloneAfter: 3 * time.Second, YtdlpEvery: time.Hour, YtdlpPath: os.Getenv("BOT_YTDLP"), FFmpeg: "ffmpeg",
		AudioFormat: "mp3", Bitrate: "96k", Channels: 1, Volume: 0.5, MaxQueue: 50, MaxPlaylist: 10}
	os.MkdirAll(filepath.Join(cfg.DataDir, "downloads"), 0700)
	// A non-square picture: it should arrive as a 512 px square PNG.
	if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=1200x800",
		"-frames:v", "1", filepath.Join(cfg.DataDir, "pfp.jpg")).CombinedOutput(); err != nil {
		t.Fatalf("making a test picture: %v %s", err, b)
	}
	id, err := loadIdentity(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	y := newYtdlp(cfg)
	if !y.ready() {
		if err := y.update(); err != nil {
			t.Fatalf("no yt-dlp: %v", err)
		}
	}
	b := newBot(cfg, id, y)
	go b.run()
	// The bot publishes its key; then alice's first message makes the
	// channel's key, sealed for both of them.
	waitFor(t, "the bot to sign in and publish its key", 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		u := m.users["Music"]
		return u != nil && u.encKey != nil
	})

	waitFor(t, "the bot's picture", 10*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.pfp != nil })
	m.mu.Lock()
	img, format, err := image.Decode(bytes.NewReader(m.pfp))
	m.mu.Unlock()
	if err != nil || format != "png" || img.Bounds().Dx() != 512 || img.Bounds().Dy() != 512 {
		t.Fatalf("picture should be a 512x512 PNG, got %s %v (%v)", format, img.Bounds(), err)
	}

	// Not in a call yet: the bot won't come.
	a.say(t, text, "!play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Join a voice channel first", 10*time.Second)

	// Alice joins voice; then !play.
	a.call.begin(voiceA, "alice")
	m.handle("alice", map[string]interface{}{"type": "join_voice", "board_id": voiceA})
	a.say(t, text, "!play "+mediaURL+"/tone1.mp3")
	a.say(t, text, "!play "+mediaURL+"/tone2.mp3")
	a.expect(t, "Now playing: tone1", 30*time.Second)
	a.expect(t, "#1 in line", 10*time.Second)

	// Audio arrives, decrypts with alice's key from the bot, at 50 packets a second.
	drain(m.tap)
	start := time.Now()
	var got [][]byte
	bad := 0
	for time.Since(start) < 2*time.Second {
		select {
		case f := <-m.tap:
			if pt, ok := a.open(f); ok {
				got = append(got, pt)
			} else {
				bad++
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Logf("2 s of the call: %d frames opened, %d didn't", len(got), bad)
	if len(got) < 85 || len(got) > 115 {
		t.Fatalf("expected ~100 packets in 2 s (20 ms each), got %d", len(got))
	}
	// …and is real Opus that ffmpeg can decode into the tone.
	checkDecodes(t, got, work)

	// Pause: nothing sent; resume: it carries on.
	a.say(t, text, "!pause")
	a.expect(t, "Paused", 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	drain(m.tap)
	time.Sleep(time.Second)
	if n := len(m.tap); n > 2 {
		t.Fatalf("paused, but %d packets went out", n)
	}
	a.say(t, text, "!queue")
	a.expect(t, "Up next:\n1. tone2", 10*time.Second)
	a.say(t, text, "!resume")
	a.expect(t, "Resumed", 10*time.Second)
	time.Sleep(time.Second)
	if n := len(m.tap); n < 30 {
		t.Fatalf("resumed, but only %d packets in a second", n)
	}

	// Skip → the next track.
	a.say(t, text, "!skip")
	a.expect(t, "Skipped tone1", 10*time.Second)
	a.expect(t, "Now playing: tone2", 20*time.Second)

	// The connection drops mid-track: back in the same call, still playing.
	m.mu.Lock()
	m.users["Music"].conn.Close()
	m.mu.Unlock()
	waitFor(t, "the bot to drop out of the call", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })
	waitFor(t, "the bot to rejoin the call", 15*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == voiceA })
	time.Sleep(3 * time.Second)
	drain(m.tap)
	time.Sleep(time.Second)
	opened := 0
	for len(m.tap) > 0 {
		if _, ok := a.open(<-m.tap); ok {
			opened++
		}
	}
	t.Logf("after reconnecting: %d frames opened in 1 s", opened)
	if opened < 40 {
		t.Fatalf("not playing after reconnecting (%d frames in 1 s)", opened)
	}

	// Clear and disconnect.
	a.say(t, text, "!play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Queued tone1", 20*time.Second)
	a.say(t, text, "!clear")
	a.expect(t, "Cleared the queue (1 track)", 10*time.Second)
	a.say(t, text, "!disconnect")
	a.expect(t, "Left the call", 10*time.Second)
	waitFor(t, "the bot to leave", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })

	// Left alone: gone after BOT_ALONE_TIMEOUT (3 s here).
	a.say(t, text, "!play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Now playing: tone1", 30*time.Second)
	m.handle("alice", map[string]interface{}{"type": "leave_voice"})
	a.call.end()
	waitFor(t, "the bot to leave once alone", 8*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })
	a.expect(t, "on my own", 5*time.Second)
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func drain(c chan []byte) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// checkDecodes puts the decrypted packets in an Ogg file and has ffmpeg
// decode it, checking the length and that it's the 440 Hz tone (by
// counting zero crossings).
func checkDecodes(t *testing.T, pkts [][]byte, dir string) {
	path := filepath.Join(dir, "heard.ogg")
	w, err := oggwriter.New(path, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pkts {
		w.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i * 960)}, Payload: p})
	}
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", "-loglevel", "error", "-i", path, "-f", "s16le", "-ac", "1", "-ar", "48000", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg couldn't decode what the call carried: %v", err)
	}
	samples := len(out) / 2
	secs := float64(samples) / 48000
	cross := 0
	prev := int16(0)
	for i := 0; i+1 < len(out); i += 2 {
		s := int16(out[i]) | int16(out[i+1])<<8
		if (prev < 0) != (s < 0) {
			cross++
		}
		prev = s
	}
	freq := float64(cross) / 2 / secs
	t.Logf("decoded %.2f s of audio, ~%.0f Hz", secs, freq)
	if secs < 1.5 || freq < 400 || freq > 480 {
		t.Fatalf("that's not 2 s of the 440 Hz tone")
	}
}
