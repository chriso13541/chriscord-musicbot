//go:build integration

package main

// testing_e2ee_test.go — for the integration test's pretend member,
// alice: her own copy of the call key exchange, as the app has it. (The bot gets
// this from the SDK now.)

import (
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	rotateDebounce = 200 * time.Millisecond
	switchDelay    = 800 * time.Millisecond
	keysKept       = 4
	resyncEvery    = 2 * time.Second
)

type sendKey struct {
	kid     uint32
	raw     []byte
	aead    cipher.AEAD
	counter uint32
}

type peerKey struct {
	kid  uint32
	aead cipher.AEAD
}

type e2eePeer struct {
	identityHex string
	eph         [32]byte
	keys        []*peerKey
}

type callCrypto struct {
	priv ed25519.PrivateKey

	mu       sync.Mutex
	board    string
	me       string
	ephPriv  [32]byte
	ephPub   [32]byte
	cur      *sendKey
	next     *sendKey
	switchAt time.Time
	lastKID  uint32
	rotate   *time.Timer
	peers    map[string]*e2eePeer
	roster   map[string]time.Time
	channels map[string][]string
	resync   *time.Timer

	transport func(v interface{}) error
}

func newCallCrypto(priv ed25519.PrivateKey, transport func(v interface{}) error) *callCrypto {
	return &callCrypto{priv: priv, transport: transport}
}

type e2eeMsg struct {
	BoardID string `json:"board_id"`
	From    string `json:"from"`
	FromKey string `json:"from_key"`
	To      string `json:"to"`
	Data    struct {
		T     string `json:"t"`
		Eph   string `json:"eph"`
		Sig   string `json:"sig"`
		Reply bool   `json:"reply"`
		KID   uint32 `json:"kid"`
		Box   string `json:"box"`
	} `json:"data"`
}

type outMsg struct {
	to   string
	data map[string]interface{}
}

func newSendKey(kid uint32) (*sendKey, error) {
	raw := make([]byte, 32)
	if _, err := randRead(raw); err != nil {
		return nil, err
	}
	aead, err := newFrameAEAD(raw)
	if err != nil {
		return nil, err
	}
	return &sendKey{kid: kid, raw: raw, aead: aead}, nil
}

// begin starts (or for the same channel carries on) the call's encryption
// and says hello. Called after join_voice and the offer.
func (c *callCrypto) begin(board, me string) {
	c.mu.Lock()
	if c.board != board || c.me != me {
		c.resetLocked()
		var err error
		c.ephPriv, c.ephPub, err = newEphemeral()
		if err == nil {
			c.cur, err = newSendKey(1)
		}
		if err != nil {
			c.mu.Unlock()
			logf("e2ee: making call keys: %v", err)
			return
		}
		c.board, c.me, c.lastKID = board, me, 1
		c.peers, c.roster = map[string]*e2eePeer{}, map[string]time.Time{}
		c.resync = time.AfterFunc(resyncEvery, c.resyncLoop)
		logf("e2ee: call in %s is end-to-end encrypted", board)
	}
	if c.channels != nil {
		c.applyRosterLocked(c.channels)
	}
	hello := c.helloLocked(false)
	c.mu.Unlock()
	c.send([]outMsg{{data: hello}})
}

func (c *callCrypto) end() {
	c.mu.Lock()
	c.resetLocked()
	c.mu.Unlock()
}

func (c *callCrypto) resetLocked() {
	if c.rotate != nil {
		c.rotate.Stop()
		c.rotate = nil
	}
	if c.resync != nil {
		c.resync.Stop()
		c.resync = nil
	}
	for _, k := range []*sendKey{c.cur, c.next} {
		if k != nil {
			clear(k.raw)
		}
	}
	clear(c.ephPriv[:])
	c.board, c.cur, c.next, c.peers, c.roster = "", nil, nil, nil, nil
}

func (c *callCrypto) helloLocked(reply bool) map[string]interface{} {
	sig := ed25519.Sign(c.priv, helloMessage(c.board, c.me, c.ephPub[:]))
	return map[string]interface{}{
		"t": "hello", "v": 1, "reply": reply,
		"eph": base64.StdEncoding.EncodeToString(c.ephPub[:]),
		"sig": base64.StdEncoding.EncodeToString(sig),
	}
}

func (c *callCrypto) keyMsgLocked(user string, p *e2eePeer, k *sendKey) (outMsg, bool) {
	box, err := wrapKey(c.ephPriv, c.ephPub, p.eph, c.board, c.me, user, k.kid, k.raw)
	if err != nil {
		logf("e2ee: wrapping a key for %s: %v", user, err)
		return outMsg{}, false
	}
	return outMsg{to: user, data: map[string]interface{}{
		"t": "key", "v": 1, "kid": k.kid, "box": base64.StdEncoding.EncodeToString(box),
	}}, true
}

func (c *callCrypto) send(msgs []outMsg) {
	c.mu.Lock()
	board := c.board
	c.mu.Unlock()
	if board == "" {
		return
	}
	for _, m := range msgs {
		v := map[string]interface{}{"type": "voice_e2ee", "board_id": board, "data": m.data}
		if m.to != "" {
			v["to"] = m.to
		}
		if err := c.transport(v); err != nil {
			logf("e2ee: sending %v: %v", m.data["t"], err)
		}
	}
}

// seal encrypts one outgoing audio frame; nil means don't send (no call).
func (c *callCrypto) seal(kind byte, frame []byte) []byte {
	c.mu.Lock()
	if c.cur == nil {
		c.mu.Unlock()
		return nil
	}
	if c.next != nil && !time.Now().Before(c.switchAt) {
		clear(c.cur.raw)
		c.cur, c.next = c.next, nil
	}
	k := c.cur
	counter := k.counter
	k.counter++
	if k.counter == 0 {
		c.scheduleRotateLocked()
	}
	c.mu.Unlock()
	return sealFrameWith(k.aead, byte(k.kid), counter, kind, frame)
}

func (c *callCrypto) handleMessage(raw []byte) {
	var m e2eeMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	c.mu.Lock()
	if c.board == "" || m.BoardID != c.board || m.From == "" || m.From == c.me || (m.To != "" && m.To != c.me) {
		c.mu.Unlock()
		return
	}
	var out []outMsg
	switch m.Data.T {
	case "hello":
		out = c.onHelloLocked(&m)
	case "key":
		c.onKeyLocked(&m)
	}
	c.mu.Unlock()
	c.send(out)
}

func (c *callCrypto) onHelloLocked(m *e2eeMsg) []outMsg {
	identity, err1 := hex.DecodeString(m.FromKey)
	eph, err2 := base64.StdEncoding.DecodeString(m.Data.Eph)
	sig, err3 := base64.StdEncoding.DecodeString(m.Data.Sig)
	if err1 != nil || err2 != nil || err3 != nil || !verifyHello(identity, c.board, m.From, eph, sig) {
		logf("e2ee: %s's call key didn't check out — ignoring it", m.From)
		return nil
	}
	var ephArr [32]byte
	copy(ephArr[:], eph)
	idHex := strings.ToLower(m.FromKey)
	p := c.peers[m.From]
	isNew := p == nil || p.eph != ephArr || p.identityHex != idHex
	if isNew {
		p = &e2eePeer{identityHex: idHex, eph: ephArr}
		c.peers[m.From] = p
	}
	var out []outMsg
	if !m.Data.Reply {
		out = append(out, outMsg{to: m.From, data: c.helloLocked(true)})
	}
	if _, inCall := c.roster[m.From]; inCall {
		if isNew {
			c.scheduleRotateLocked()
		} else {
			for _, k := range []*sendKey{c.cur, c.next} {
				if k == nil {
					continue
				}
				if km, ok := c.keyMsgLocked(m.From, p, k); ok {
					out = append(out, km)
				}
			}
		}
	}
	return out
}

func (c *callCrypto) onKeyLocked(m *e2eeMsg) {
	p := c.peers[m.From]
	if p == nil {
		return
	}
	box, err := base64.StdEncoding.DecodeString(m.Data.Box)
	if err != nil {
		return
	}
	raw, err := unwrapKey(c.ephPriv, c.ephPub, p.eph, c.board, m.From, c.me, m.Data.KID, box)
	if err != nil {
		logf("e2ee: a key from %s didn't unwrap: %v", m.From, err)
		return
	}
	for _, k := range p.keys {
		if k.kid == m.Data.KID {
			return
		}
	}
	aead, err := newFrameAEAD(raw)
	clear(raw)
	if err != nil {
		return
	}
	p.keys = append(p.keys, &peerKey{kid: m.Data.KID, aead: aead})
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].kid < p.keys[j].kid })
	if len(p.keys) > keysKept {
		p.keys = p.keys[len(p.keys)-keysKept:]
	}
}

// updateRoster takes voice_state's channels.
func (c *callCrypto) updateRoster(channels map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels = channels
	if c.board == "" {
		return
	}
	c.applyRosterLocked(channels)
}

func (c *callCrypto) applyRosterLocked(channels map[string][]string) {
	now := map[string]bool{}
	for _, u := range channels[c.board] {
		if u != c.me {
			now[u] = true
		}
	}
	changed := false
	for u := range c.roster {
		if !now[u] {
			delete(c.roster, u)
			delete(c.peers, u)
			changed = true
		}
	}
	for u := range now {
		if _, ok := c.roster[u]; !ok {
			c.roster[u] = time.Now()
			if c.peers[u] != nil {
				changed = true
			}
		}
	}
	if changed {
		c.scheduleRotateLocked()
	}
}

func (c *callCrypto) resyncLoop() {
	c.mu.Lock()
	if c.board == "" {
		c.mu.Unlock()
		return
	}
	var out []outMsg
	for u, since := range c.roster {
		p := c.peers[u]
		if (p == nil || len(p.keys) == 0) && time.Since(since) >= resyncEvery {
			out = append(out, outMsg{to: u, data: c.helloLocked(false)})
		}
	}
	c.resync = time.AfterFunc(resyncEvery, c.resyncLoop)
	c.mu.Unlock()
	c.send(out)
}

func (c *callCrypto) scheduleRotateLocked() {
	if c.rotate == nil {
		c.rotate = time.AfterFunc(rotateDebounce, c.doRotate)
	}
}

func (c *callCrypto) doRotate() {
	c.mu.Lock()
	c.rotate = nil
	if c.board == "" {
		c.mu.Unlock()
		return
	}
	if c.next != nil {
		clear(c.cur.raw)
		c.cur = c.next
	}
	k, err := newSendKey(c.lastKID + 1)
	if err != nil {
		c.mu.Unlock()
		logf("e2ee: new key: %v", err)
		return
	}
	c.lastKID++
	c.next, c.switchAt = k, time.Now().Add(switchDelay)
	var out []outMsg
	for u := range c.roster {
		if p := c.peers[u]; p != nil {
			if km, ok := c.keyMsgLocked(u, p, k); ok {
				out = append(out, km)
			}
		}
	}
	c.mu.Unlock()
	c.send(out)
}

// ready reports how many people in the call can hear the bot (have its
// key) out of how many are in it — for !np / logs.
func (c *callCrypto) ready() (withKey, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for u := range c.roster {
		total++
		if c.peers[u] != nil {
			withKey++
		}
	}
	return
}
