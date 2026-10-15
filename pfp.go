package main

// pfp.go — the bot's profile picture: pfp.png or pfp.jpg (or .jpeg) in its
// data directory, read once when the bot starts. To change it, replace the
// file and restart the bot.
//
// Same handshake as the app: on every connect the bot says when its picture
// last changed (pfp_info); if the server's copy isn't that one, it asks
// (pfp_request) and the bot uploads it (pfp_upload). The file's modification
// time is that "last changed", so a new file means a new upload, and an
// unchanged one costs nothing on restart.
//
// The picture is made square (centre crop) and at most 512 px with ffmpeg,
// like the app's cropper does, so any photo works; if that fails the file
// is sent as it is.

import (
	"bytes"
	"context"
	"encoding/base64"
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

// announcePfp tells the server when the picture last changed (on connect).
func (b *Bot) announcePfp() {
	if b.pfp == nil {
		return
	}
	_ = b.sendJSON(map[string]interface{}{"type": "pfp_info", "pfp_updated_at": b.pfp.updatedAt})
}

// uploadPfp answers the server's pfp_request.
func (b *Bot) uploadPfp() {
	if b.pfp == nil {
		return
	}
	if err := b.sendJSON(map[string]interface{}{
		"type": "pfp_upload", "pfp_data": base64.StdEncoding.EncodeToString(b.pfp.data), "pfp_updated_at": b.pfp.updatedAt,
	}); err == nil {
		logf("pfp: uploaded")
	}
}
