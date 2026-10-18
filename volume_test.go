package main

import "testing"

func TestParseVolume(t *testing.T) {
	for in, want := range map[string]float64{"100": 0.25, "100%": 0.25, "50": 0.125, "200": 0.5, "0": 0, "0.5": 0.5, "1.0": 1} {
		got, err := parseVolume(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"loud", "-5", "500"} {
		if _, err := parseVolume(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
