package main

// config.go — settings, from the environment (the systemd unit loads them
// from /etc/chriscord/musicbot-<name>.env; see musicbot.env.example).

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ServerURL   string        // CHRISCORD_URL — where the Chriscord server is, e.g. http://127.0.0.1:7070
	Token       string        // CHRISCORD_BOT_TOKEN — the bot's token from the app (Settings → Bots)
	DataDir     string        // BOT_DATA_DIR — yt-dlp, downloads, its picture
	Prefix      string        // BOT_PREFIX — what commands start with
	AloneAfter  time.Duration // BOT_ALONE_TIMEOUT — leave after sitting alone this long
	YtdlpEvery  time.Duration // BOT_YTDLP_CHECK — how often to check GitHub for a new yt-dlp
	YtdlpPath   string        // BOT_YTDLP — use this yt-dlp instead of the managed one (no auto-update)
	FFmpeg      string        // BOT_FFMPEG — the ffmpeg to use
	AudioFormat string        // BOT_AUDIO_FORMAT — "best" (no re-encode) or "mp3"
	Bitrate     string        // BOT_BITRATE — Opus bitrate sent to the call, e.g. 96k
	Channels    int           // BOT_CHANNELS — 1 (mono, like voices) or 2
	Volume      float64       // BOT_VOLUME — 1.0 = as downloaded
	MaxQueue    int           // BOT_MAX_QUEUE — most tracks waiting at once
	MaxPlaylist int           // BOT_MAX_PLAYLIST — most tracks one playlist link adds
	MaxLength   time.Duration // BOT_MAX_LENGTH — longest track it'll play (0 = no limit)
	YtdlpArgs   []string      // BOT_YTDLP_ARGS — extra yt-dlp arguments (e.g. --cookies /path)
}

func loadConfig() (config, error) {
	c := config{
		ServerURL:   env("CHRISCORD_URL", "http://127.0.0.1:7070"),
		Token:       env("CHRISCORD_BOT_TOKEN", ""),
		DataDir:     env("BOT_DATA_DIR", "."),
		Prefix:      env("BOT_PREFIX", "!"),
		YtdlpPath:   env("BOT_YTDLP", ""),
		FFmpeg:      env("BOT_FFMPEG", "ffmpeg"),
		AudioFormat: strings.ToLower(env("BOT_AUDIO_FORMAT", "best")),
		Bitrate:     env("BOT_BITRATE", "96k"),
	}
	c.YtdlpArgs = strings.Fields(env("BOT_YTDLP_ARGS", ""))
	var err error
	if c.AloneAfter, err = envDuration("BOT_ALONE_TIMEOUT", 5*time.Minute); err != nil {
		return c, err
	}
	if c.YtdlpEvery, err = envDuration("BOT_YTDLP_CHECK", 24*time.Hour); err != nil {
		return c, err
	}
	if c.MaxLength, err = envDuration("BOT_MAX_LENGTH", 3*time.Hour); err != nil {
		return c, err
	}
	if c.Channels, err = envInt("BOT_CHANNELS", 1); err != nil {
		return c, err
	}
	if c.MaxQueue, err = envInt("BOT_MAX_QUEUE", 200); err != nil {
		return c, err
	}
	if c.MaxPlaylist, err = envInt("BOT_MAX_PLAYLIST", 50); err != nil {
		return c, err
	}
	if c.Volume, err = strconv.ParseFloat(env("BOT_VOLUME", "0.5"), 64); err != nil {
		return c, fmt.Errorf("BOT_VOLUME: %w", err)
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	if !strings.Contains(c.ServerURL, "://") {
		c.ServerURL = "http://" + c.ServerURL
	}
	if c.Channels != 1 && c.Channels != 2 {
		return c, fmt.Errorf("BOT_CHANNELS must be 1 or 2")
	}
	if c.AudioFormat != "best" && c.AudioFormat != "mp3" {
		return c, fmt.Errorf("BOT_AUDIO_FORMAT must be best or mp3")
	}
	if c.Prefix == "" {
		c.Prefix = "!"
	}
	return c, nil
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w (use e.g. 5m, 24h)", name, err)
	}
	return d, nil
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}
