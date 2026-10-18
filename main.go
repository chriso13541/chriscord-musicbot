package main

// chriscord-musicbot — a music bot for a Chriscord server.
//
// It runs next to the server as its own service and joins as a bot: with
// the token made for it in the app (Settings → Bots), once someone with
// Manage Bots has added it to the server. Chat with it and its music are
// end-to-end encrypted like everything else. See README.md.

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
)

// version is bumped with each change worth telling apart in the logs.
const version = "0.5.1"

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
	if cfg.Token == "" {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, "identity.key")); err == nil {
			fatal(fmt.Errorf("CHRISCORD_BOT_TOKEN isn't set. Since 0.5 the bot joins as a bot: make one in the app " +
				"(Settings → Bots → New bot), add it to your server from its Add to server link, and put its token " +
				"in the settings file. See README.md, \"Moving from 0.4\""))
		}
		fatal(fmt.Errorf("CHRISCORD_BOT_TOKEN isn't set — make the bot in the app (Settings → Bots → New bot) and put its token in the settings file"))
	}
	c, err := chriscordbot.New(chriscordbot.Options{
		Server: cfg.ServerURL, Token: cfg.Token, Client: "chriscord-musicbot/" + version, Logf: logf,
	})
	if err != nil {
		fatal(fmt.Errorf("CHRISCORD_BOT_TOKEN: %w", err))
	}
	y := newYtdlp(cfg)
	go y.updateLoop()

	logf("chriscord-musicbot %s starting: %s as the bot %q (commands start with %q)", version, cfg.ServerURL, c.Name(), cfg.Prefix)
	b := newBot(cfg, c, y)
	go b.pl.loop()
	c.Run(context.Background())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chriscord-musicbot:", err)
	os.Exit(1)
}

func randRead(b []byte) (int, error) { return io.ReadFull(rand.Reader, b) }

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", args...)
}
