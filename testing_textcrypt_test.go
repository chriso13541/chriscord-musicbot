//go:build integration

package main

// testing_textcrypt_test.go — for the integration test's pretend member,
// alice: her own copy of encrypted chat, as the app has it. (The bot gets
// this from the SDK now.)

import (
	"bytes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	textPrefix      = "e2ee:1:"
	textRefetchGap  = 5 * time.Second
	textPendingGap  = 1500 * time.Millisecond
	textPendingTick = 60 * time.Second
)

type textMember struct {
	Username  string  `json:"username"`
	PublicKey string  `json:"public_key"`
	EncKey    *string `json:"enc_key"`
	Sig       *string `json:"sig"`
}

type boardKeys struct {
	keys      map[int][]byte
	latest    int
	rotate    bool
	members   []textMember
	fetchedAt time.Time
}

type textIdentity struct {
	idHex   string
	idPriv  ed25519.PrivateKey
	encPriv [32]byte
	encPub  [32]byte
	me      string
}

type textCrypto struct {
	api *api

	mu        sync.Mutex
	id        textIdentity
	boards    map[string]*boardKeys
	triedAt   map[string]time.Time
	boardLock map[string]*sync.Mutex
	published bool
	pendingT  *time.Timer
}

func newTextCrypto(a *api, priv ed25519.PrivateKey) *textCrypto {
	t := &textCrypto{api: a, boards: map[string]*boardKeys{}, triedAt: map[string]time.Time{}, boardLock: map[string]*sync.Mutex{}}
	t.id.idPriv = priv
	t.id.idHex = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	t.id.encPriv, t.id.encPub = encKeyFromSeed(priv.Seed())
	return t
}

func (t *textCrypto) setUsername(u string) { t.mu.Lock(); t.id.me = u; t.mu.Unlock() }

func (t *textCrypto) identity() textIdentity { t.mu.Lock(); defer t.mu.Unlock(); return t.id }

func encKeyFromSeed(seed []byte) (priv, pub [32]byte) {
	io.ReadFull(hkdf.New(sha256.New, seed, nil, []byte("chriscord-e2ee-v1/x25519")), priv[:])
	p, _ := curve25519.X25519(priv[:], curve25519.Basepoint)
	copy(pub[:], p)
	return
}

func encKeyMessage(encPub []byte) []byte {
	return append([]byte("chriscord-e2ee-v1/enc-key\x00"), encPub...)
}

func memberEncKey(m textMember) ([32]byte, bool) {
	var enc [32]byte
	if m.EncKey == nil || m.Sig == nil {
		return enc, false
	}
	id, err1 := hex.DecodeString(m.PublicKey)
	e, err2 := hex.DecodeString(*m.EncKey)
	sig, err3 := hex.DecodeString(*m.Sig)
	if err1 != nil || err2 != nil || err3 != nil || len(id) != ed25519.PublicKeySize || len(e) != 32 ||
		!ed25519.Verify(id, encKeyMessage(e), sig) {
		return enc, false
	}
	copy(enc[:], e)
	return enc, true
}

func boxCipher(myPriv, peerPub [32]byte, board string, gen int, senderID, recipientID string, senderEnc, recipientEnc [32]byte) (cipher.AEAD, error) {
	shared, err := curve25519.X25519(myPriv[:], peerPub[:])
	if err != nil {
		return nil, err
	}
	var info bytes.Buffer
	fmt.Fprintf(&info, "chriscord-e2ee-v1/channel-key\x00%s\x00%d\x00%s\x00%s\x00", board, gen,
		strings.ToLower(senderID), strings.ToLower(recipientID))
	info.Write(senderEnc[:])
	info.Write(recipientEnc[:])
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, info.Bytes()), k); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(k)
}

func sealBox(id textIdentity, recipientID string, recipientEnc [32]byte, board string, gen int, key []byte) (string, error) {
	c, err := boxCipher(id.encPriv, recipientEnc, board, gen, id.idHex, recipientID, id.encPub, recipientEnc)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.NonceSize(), c.NonceSize()+len(key)+c.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(c.Seal(nonce, nonce, key, nil)), nil
}

func openBox(id textIdentity, senderID string, senderEnc [32]byte, board string, gen int, box string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(box)
	if err != nil {
		return nil, err
	}
	c, err := boxCipher(id.encPriv, senderEnc, board, gen, senderID, id.idHex, senderEnc, id.encPub)
	if err != nil {
		return nil, err
	}
	if len(raw) < c.NonceSize() {
		return nil, errors.New("short box")
	}
	key, err := c.Open(nil, raw[:c.NonceSize()], raw[c.NonceSize():], nil)
	if err == nil && len(key) != chacha20poly1305.KeySize {
		err = errors.New("wrong key size")
	}
	return key, err
}

// textPlain is what's inside an encrypted message (the app's format; "a"
// and "i" are kept as raw JSON since the bot doesn't use them).
type textPlain struct {
	U string          `json:"u"`
	T string          `json:"t"`
	A json.RawMessage `json:"a,omitempty"`
	I json.RawMessage `json:"i,omitempty"`
}

func textAAD(board string, gen int) []byte {
	return []byte(fmt.Sprintf("chriscord-e2ee-v1/msg\x00%s\x00%d", board, gen))
}

func sealPlain(key []byte, board string, gen int, p textPlain) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	pt, _ := json.Marshal(p)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(pt)+aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, pt, textAAD(board, gen))
	return textPrefix + strconv.Itoa(gen) + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

func parseText(content string) (gen int, blob []byte, ok bool) {
	rest, found := strings.CutPrefix(content, textPrefix)
	if !found {
		return 0, nil, false
	}
	g, b64, found := strings.Cut(rest, ":")
	if !found {
		return 0, nil, false
	}
	gen, err := strconv.Atoi(g)
	if err != nil || gen < 1 {
		return 0, nil, false
	}
	blob, err = base64.StdEncoding.DecodeString(b64)
	return gen, blob, err == nil
}

func openText(key []byte, board string, gen int, blob []byte) (textPlain, error) {
	var p textPlain
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return p, err
	}
	if len(blob) < aead.NonceSize() {
		return p, errors.New("short message")
	}
	pt, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], textAAD(board, gen))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(pt, &p)
}

type keysResp struct {
	Latest int  `json:"latest"`
	Rotate bool `json:"rotate"`
	Boxes  []struct {
		Generation int        `json:"generation"`
		Box        string     `json:"box"`
		Sender     textMember `json:"sender"`
	} `json:"boxes"`
	Members []textMember `json:"members"`
}

// begin publishes the bot's encryption key and shares any keys others are
// waiting on (after each sign-in).
func (t *textCrypto) begin() {
	t.mu.Lock()
	t.published = false
	t.mu.Unlock()
	t.ensurePublished()
	t.runPending()
}

// pendingLoop shares keys every textPendingTick, like the app.
func (t *textCrypto) pendingLoop(stop <-chan struct{}) {
	tick := time.NewTicker(textPendingTick)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			t.runPending()
		}
	}
}

func (t *textCrypto) ensurePublished() {
	t.mu.Lock()
	done := t.published
	id := t.id
	t.mu.Unlock()
	if done {
		return
	}
	sig := ed25519.Sign(id.idPriv, encKeyMessage(id.encPub[:]))
	err := t.api.call("PUT", "/api/e2ee/enc-key", map[string]string{
		"enc_key": hex.EncodeToString(id.encPub[:]), "sig": hex.EncodeToString(sig),
	}, nil)
	if err != nil {
		logf("e2ee: publishing the bot's encryption key: %v", err)
		return
	}
	t.mu.Lock()
	t.published = true
	t.mu.Unlock()
}

func (t *textCrypto) fetch(board string) (*boardKeys, error) {
	id := t.identity()
	var r keysResp
	if err := t.api.call("GET", "/api/boards/"+url.PathEscape(board)+"/keys", nil, &r); err != nil {
		return nil, err
	}
	bk := &boardKeys{keys: map[int][]byte{}, latest: r.Latest, rotate: r.Rotate, members: r.Members, fetchedAt: time.Now()}
	for _, b := range r.Boxes {
		senderEnc, ok := memberEncKey(b.Sender)
		if !ok {
			continue
		}
		key, err := openBox(id, b.Sender.PublicKey, senderEnc, board, b.Generation, b.Box)
		if err != nil {
			logf("e2ee: a key for %s (generation %d) didn't open: %v", board, b.Generation, err)
			continue
		}
		bk.keys[b.Generation] = key
	}
	t.mu.Lock()
	if old := t.boards[board]; old != nil {
		for g, k := range old.keys {
			if bk.keys[g] == nil {
				bk.keys[g] = k
			}
		}
	}
	t.boards[board] = bk
	t.mu.Unlock()
	return bk, nil
}

func (t *textCrypto) key(board string, gen int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bk := t.boards[board]; bk != nil {
		return bk.keys[gen]
	}
	return nil
}

func (t *textCrypto) lockFor(board string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.boardLock[board]
	if l == nil {
		l = &sync.Mutex{}
		t.boardLock[board] = l
	}
	return l
}

func (t *textCrypto) newGeneration(board string, bk *boardKeys) error {
	id := t.identity()
	gen := bk.latest + 1
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	boxes := []map[string]string{}
	haveSelf := false
	for _, m := range bk.members {
		enc, ok := memberEncKey(m)
		if !ok {
			continue
		}
		box, err := sealBox(id, m.PublicKey, enc, board, gen, key)
		if err != nil {
			continue
		}
		haveSelf = haveSelf || strings.EqualFold(m.PublicKey, id.idHex)
		boxes = append(boxes, map[string]string{"recipient": strings.ToLower(m.PublicKey), "box": box})
	}
	if !haveSelf {
		box, err := sealBox(id, id.idHex, id.encPub, board, gen, key)
		if err != nil {
			return err
		}
		boxes = append(boxes, map[string]string{"recipient": id.idHex, "box": box})
	}
	if err := t.api.call("POST", "/api/boards/"+url.PathEscape(board)+"/keys", map[string]interface{}{
		"generation": gen, "boxes": boxes,
	}, nil); err != nil {
		return err
	}
	t.mu.Lock()
	if cur := t.boards[board]; cur != nil {
		cur.keys[gen] = key
		cur.latest, cur.rotate = gen, false
	}
	t.mu.Unlock()
	logf("e2ee: new key (generation %d) for %s, for %d member(s)", gen, board, len(boxes))
	return nil
}

// seal encrypts a message the bot sends in board.
func (t *textCrypto) seal(board, text string) (string, error) {
	t.ensurePublished()
	id := t.identity()
	l := t.lockFor(board)
	l.Lock()
	defer l.Unlock()
	for attempt := 0; ; attempt++ {
		bk, err := t.fetch(board)
		if err != nil {
			return "", err
		}
		if bk.latest == 0 || bk.rotate || t.key(board, bk.latest) == nil {
			err := t.newGeneration(board, bk)
			if isConflict(err) && attempt < 2 {
				continue
			}
			if err != nil {
				return "", err
			}
		}
		t.mu.Lock()
		gen := t.boards[board].latest
		key := t.boards[board].keys[gen]
		t.mu.Unlock()
		return sealPlain(key, board, gen, textPlain{U: id.me, T: text})
	}
}

// open decrypts a message's content. ok is false when it can't be read
// (no key yet, broken, or forged); plain (unencrypted) messages come back
// as they are.
func (t *textCrypto) open(board, author, content string) (string, bool) {
	gen, blob, isEnc := parseText(content)
	if !isEnc {
		return content, true
	}
	key := t.key(board, gen)
	if key == nil {
		t.mu.Lock()
		newer := t.boards[board] == nil || gen > t.boards[board].latest
		try := newer || time.Since(t.triedAt[board]) > textRefetchGap
		if try {
			t.triedAt[board] = time.Now()
		}
		t.mu.Unlock()
		if try {
			if _, err := t.fetch(board); err != nil {
				logf("e2ee: fetching keys for %s: %v", board, err)
			}
			key = t.key(board, gen)
		}
	}
	if key == nil {
		return "", false
	}
	p, err := openText(key, board, gen, blob)
	if err != nil || len(p.I) > 0 || (author != "" && p.U != author) {
		return "", false
	}
	return p.T, true
}

// changed: keys moved on the server — share what others need (debounced).
func (t *textCrypto) changed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingT != nil {
		t.pendingT.Stop()
	}
	t.pendingT = time.AfterFunc(textPendingGap, t.runPending)
}

type pendingResp struct {
	Pending []struct {
		BoardID    string     `json:"board_id"`
		Generation int        `json:"generation"`
		Recipient  textMember `json:"recipient"`
	} `json:"pending"`
}

func (t *textCrypto) runPending() {
	id := t.identity()
	var r pendingResp
	if err := t.api.call("GET", "/api/e2ee/pending", nil, &r); err != nil {
		return
	}
	fetched := map[string]bool{}
	var boxes []map[string]interface{}
	for _, p := range r.Pending {
		enc, ok := memberEncKey(p.Recipient)
		if !ok {
			continue
		}
		key := t.key(p.BoardID, p.Generation)
		if key == nil && !fetched[p.BoardID] {
			fetched[p.BoardID] = true
			t.fetch(p.BoardID)
			key = t.key(p.BoardID, p.Generation)
		}
		if key == nil {
			continue
		}
		box, err := sealBox(id, p.Recipient.PublicKey, enc, p.BoardID, p.Generation, key)
		if err != nil {
			continue
		}
		boxes = append(boxes, map[string]interface{}{
			"board_id": p.BoardID, "generation": p.Generation,
			"recipient": strings.ToLower(p.Recipient.PublicKey), "box": box,
		})
	}
	if len(boxes) == 0 {
		return
	}
	if err := t.api.call("POST", "/api/e2ee/boxes", map[string]interface{}{"boxes": boxes}, nil); err != nil {
		logf("e2ee: sharing keys: %v", err)
		return
	}
	logf("e2ee: shared %d channel key(s) with members who needed them", len(boxes))
}
