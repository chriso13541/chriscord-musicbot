#!/bin/sh
# Installs chriscord-musicbot as a systemd service.
#
#   go mod tidy && go build -o chriscord-musicbot .
#   sudo deploy/install.sh            # the bot for your main server ("main")
#   sudo deploy/install.sh gaming     # another bot, for another server on this box
#
# Run it again after pulling and rebuilding to update the binary; settings
# and the bot's identity are kept.
#
# What it does:
#   - creates a "chriscord-musicbot" system user
#   - installs the binary to /usr/local/bin/chriscord-musicbot
#   - makes /var/lib/chriscord-musicbot/<name> (yt-dlp, downloads, its picture)
#   - copies settings to /etc/chriscord/musicbot-<name>.env (first time only)
#   - fetches yt-dlp, then enables and starts chriscord-musicbot@<name>
set -eu

NAME=${1:-main}
DATA=/var/lib/chriscord-musicbot/$NAME
ENV=/etc/chriscord/musicbot-$NAME.env
BIN=/usr/local/bin/chriscord-musicbot
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(dirname "$HERE")

[ "$(id -u)" -eq 0 ] || { echo "Run this with sudo." >&2; exit 1; }
[ -x "$REPO/chriscord-musicbot" ] || {
  echo "Build it first:  go mod tidy && go build -o chriscord-musicbot ." >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd (systemctl) wasn't found on this machine." >&2; exit 1; }
command -v ffmpeg >/dev/null || echo "Note: ffmpeg isn't installed — the bot needs it (e.g. sudo apt install ffmpeg)." >&2
command -v deno >/dev/null || echo "Note: Deno isn't installed — yt-dlp needs it for YouTube (see README.md)." >&2

if ! id chriscord-musicbot >/dev/null 2>&1; then
  NOLOGIN=$(command -v nologin || echo /sbin/nologin)
  useradd --system --home-dir /var/lib/chriscord-musicbot --no-create-home --shell "$NOLOGIN" chriscord-musicbot
  echo "Created system user: chriscord-musicbot"
fi

systemctl stop "chriscord-musicbot@$NAME" 2>/dev/null || true
install -m 0755 "$REPO/chriscord-musicbot" "$BIN"
echo "Installed $BIN"

install -d -m 0750 -o chriscord-musicbot -g chriscord-musicbot /var/lib/chriscord-musicbot "$DATA"
install -d -m 0755 /etc/chriscord
FRESH=no
if [ ! -f "$ENV" ]; then
  install -m 0640 -g chriscord-musicbot "$HERE/musicbot.env.example" "$ENV"
  FRESH=yes
fi
install -m 0644 "$HERE/chriscord-musicbot@.service" /etc/systemd/system/chriscord-musicbot@.service
systemctl daemon-reload

echo "Fetching yt-dlp…"
runuser -u chriscord-musicbot -- env BOT_DATA_DIR="$DATA" HOME="$DATA" "$BIN" update-ytdlp || \
  echo "Couldn't fetch yt-dlp now; the bot tries again when it starts." >&2

if [ "$FRESH" = yes ]; then
  echo
  echo "Settings are in $ENV — set CHRISCORD_URL and CHRISCORD_BOT_TOKEN"
  echo "(make the bot in the app: Settings → Bots → New bot, and add it to"
  echo "your server from its Add to server link), then start it:"
  echo "  sudo systemctl enable --now chriscord-musicbot@$NAME"
else
  systemctl enable --now "chriscord-musicbot@$NAME"
  sleep 2
  systemctl --no-pager --lines=5 status "chriscord-musicbot@$NAME" || true
fi
