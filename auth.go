package main

// auth.go — the bot's account: an Ed25519 identity key kept in the data
// directory, and signing in to the server with it exactly the way the app
// does (POST /api/challenge, sign the nonce, POST /api/join). The first
// sign-in registers the username to the key (with the server key or an
// invite code); after that the key alone is enough.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const identityFile = "identity.key"

// loadIdentity reads the bot's identity seed, making one the first time.
func loadIdentity(dataDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dataDir, identityFile)
	data, err := os.ReadFile(path)
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s is damaged (expected %d hex-encoded bytes)", path, ed25519.SeedSize)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0600); err != nil {
		return nil, err
	}
	logf("auth: made a new identity key (%s) — keep it: it *is* the bot's account", path)
	return ed25519.NewKeyFromSeed(seed), nil
}

// api talks to the server over HTTP with the session token.
type api struct {
	base   string
	client *http.Client

	mu    sync.Mutex
	token string
}

func newAPI(base string) *api {
	return &api{base: base, client: &http.Client{Timeout: 30 * time.Second}}
}

func (a *api) setToken(t string) { a.mu.Lock(); a.token = t; a.mu.Unlock() }
func (a *api) getToken() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.token }

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.msg, e.code) }

func isConflict(err error) bool {
	var h *httpError
	return errors.As(err, &h) && h.code == http.StatusConflict
}

func (a *api) call(method, path string, body, out interface{}) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t := a.getToken(); t != "" {
		req.Header.Set("X-Session-Token", t)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e["error"]
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &httpError{resp.StatusCode, msg}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// login signs in (registering on first use) and keeps the session token.
func (a *api) login(cfg config, id ed25519.PrivateKey) (username string, err error) {
	pub := hex.EncodeToString(id.Public().(ed25519.PublicKey))
	var ch struct {
		Nonce string `json:"nonce"`
	}
	a.setToken("")
	if err := a.call("POST", "/api/challenge", map[string]string{"public_key": pub}, &ch); err != nil {
		return "", fmt.Errorf("challenge: %w", err)
	}
	nonce, err := hex.DecodeString(ch.Nonce)
	if err != nil {
		return "", fmt.Errorf("challenge: bad nonce")
	}
	req := map[string]interface{}{
		"username":   cfg.Username,
		"public_key": pub,
		"nonce":      ch.Nonce,
		"signature":  hex.EncodeToString(ed25519.Sign(id, nonce)),
	}
	if cfg.ServerKey != "" {
		req["server_key"] = cfg.ServerKey
	}
	if cfg.Invite != "" {
		req["invite"] = cfg.Invite
	}
	var j struct {
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if err := a.call("POST", "/api/join", req, &j); err != nil {
		// The key already has a name on this server (it was set up with a
		// different BOT_USERNAME): a server keeps a key's first name, so
		// sign in with that one rather than retrying forever.
		var h *httpError
		if errors.As(err, &h) && h.code == http.StatusConflict {
			if _, rest, ok := strings.Cut(h.msg, "registered as '"); ok {
				if name, _, ok := strings.Cut(rest, "'"); ok && name != "" && name != cfg.Username {
					logf("auth: this bot's key is registered here as %q, not %q (BOT_USERNAME) — signing in as %q", name, cfg.Username, name)
					cfg.Username = name
					return a.login(cfg, id)
				}
			}
		}
		return "", fmt.Errorf("join: %w", err)
	}
	a.setToken(j.Token)
	return j.Username, nil
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", args...)
}
