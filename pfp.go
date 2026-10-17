package main

// pfp.go — the bot's profile picture: pfp.png or pfp.jpg (or .jpeg) in its
// data directory, read once when the bot starts. To change it, replace the
// file and restart the bot.
//
// The SDK uploads it the way the app does yours: on every connect it says
// when the picture last changed, and the server asks for it only if its
// copy is older. The file's modification time is that "last changed", so a
// new file means a new upload, and an unchanged one costs nothing on
// restart.
//
// The picture is made square (centre crop) and at most 512 px with ffmpeg,
// like the app's cropper does, so any photo works; if that fails the file
// is sent as it is.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const maxPfpBytes = 8 << 20 // the server's limit

var pfpNames = []string{"pfp.png", "pfp.jpg", "pfp.jpeg"}

type botPfp struct {
	data      []byte
	updatedAt int64
}

// loadPfp reads the picture, if there is one (nil if not).
func loadPfp(cfg config) *botPfp {
	for _, name := range pfpNames {
		path := filepath.Join(cfg.DataDir, name)
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		data, err := squarePfp(cfg.FFmpeg, path)
		if err != nil {
			logf("pfp: couldn't resize %s (%v) — using it as it is", name, err)
			data, err = os.ReadFile(path)
			if err != nil {
				logf("pfp: reading %s: %v", name, err)
				return nil
			}
		}
		if len(data) > maxPfpBytes {
			logf("pfp: %s is too big (%d bytes; the server takes up to %d)", name, len(data), maxPfpBytes)
			return nil
		}
		logf("pfp: using %s (%d bytes)", name, len(data))
		return &botPfp{data: data, updatedAt: st.ModTime().Unix()}
	}
	return nil
}

// squarePfp centre-crops the image to a square, no bigger than 512 px, as PNG.
func squarePfp(ffmpeg, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", path,
		"-vf", "crop='min(iw,ih)':'min(iw,ih)',scale='min(512,iw)':-1",
		"-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s", lastLine(stderr.String(), err.Error()))
	}
	return out, nil
}
