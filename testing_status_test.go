//go:build integration

package main

// testing_status_test.go — for the integration test's pretend member,
// alice: her own copy of the status format, as the app has it. (The bot gets
// this from the SDK now.)

import "time"

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
