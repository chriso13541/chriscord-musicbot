package main

// chriscord-musicbot — a music bot for a Chriscord server.
//
// It runs next to the server as its own service and joins like any member:
// its own account (an identity key in its data directory), end-to-end
// encrypted chat for the commands, and an end-to-end encrypted call for
// the music. No changes to the server are needed. See README.md.

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// version is bumped with each change worth telling apart in the logs.
const version = "0.4.0"

const usage = `chriscord-musicbot — a music bot for a Chriscord server

USAGE:
    chriscord-musicbot            run the bot
    chriscord-musicbot update-ytdlp   fetch the latest yt-dlp now and exit
    chriscord-musicbot version
    chriscord-musicbot help

Settings come from the environment — see musicbot.env.example.
`

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Println("chriscord-musicbot", version)
			return
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		case "update-ytdlp":
			if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
				fatal(err)
			}
			if err := newYtdlp(cfg).update(); err != nil {
				fatal(err)
			}
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}

	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "downloads")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			fatal(err)
		}
	}
	// Downloads left over from before a restart.
	if old, _ := filepath.Glob(filepath.Join(cfg.DataDir, "downloads", "t-*")); len(old) > 0 {
		for _, o := range old {
			os.RemoveAll(o)
		}
	}
	if _, err := exec.LookPath(cfg.FFmpeg); err != nil {
		fatal(fmt.Errorf("ffmpeg not found (%s) — install it, or set BOT_FFMPEG", cfg.FFmpeg))
	}
	id, err := loadIdentity(cfg.DataDir)
	if err != nil {
		fatal(err)
	}
	y := newYtdlp(cfg)
	go y.updateLoop()

	logf("chriscord-musicbot %s starting: %s as %s (commands start with %q)", version, cfg.ServerURL, cfg.Username, cfg.Prefix)
	newBot(cfg, id, y).run()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chriscord-musicbot:", err)
	os.Exit(1)
}

func randRead(b []byte) (int, error) { return io.ReadFull(rand.Reader, b) }
