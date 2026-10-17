//go:build integration

package main

// testing_api_test.go — for the integration test's pretend member,
// alice: her own copy of the HTTP client, as the app has it. (The bot gets
// this from the SDK now.)

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

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
