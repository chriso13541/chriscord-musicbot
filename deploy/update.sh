#!/bin/sh
# Builds the bot from this folder and puts it live: every installed bot
# (chriscord-musicbot@<name>) is restarted on the new binary. Settings,
# identities and downloads are untouched.
#
#   deploy/update.sh            # build what's here, install, restart
#   deploy/update.sh --pull     # git pull first
#
# Run it as yourself (not with sudo): Go builds as you, and it asks for
# sudo only to install and restart.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(dirname "$HERE")
BIN=/usr/local/bin/chriscord-musicbot
UNIT=/etc/systemd/system/chriscord-musicbot@.service
cd "$REPO"

if [ "$(id -u)" -eq 0 ]; then
  echo "Run this as yourself, not with sudo — it asks for sudo when it needs it." >&2
  exit 1
fi
command -v go >/dev/null || { echo "Go isn't on your PATH (is /usr/local/go/bin in it?)." >&2; exit 1; }

# 1. Latest code, if asked.
if [ "${1:-}" = "--pull" ]; then
  [ -d .git ] || { echo "--pull: this folder isn't a git checkout." >&2; exit 1; }
  echo "→ git pull"
  git pull --ff-only
fi

# 2. Build (into a temporary name, so a failed build leaves nothing half-done).
echo "→ building"
go mod tidy
go build -o chriscord-musicbot.new .
NEW=$(./chriscord-musicbot.new version)
OLD=$("$BIN" version 2>/dev/null || echo "none")
mv chriscord-musicbot.new chriscord-musicbot
echo "   built $NEW (installed: $OLD)"

# 3. Which bots are installed: one settings file per bot.
NAMES=""
for f in /etc/chriscord/musicbot-*.env; do
  [ -e "$f" ] || continue
  n=${f#/etc/chriscord/musicbot-}
  NAMES="$NAMES ${n%.env}"
done
[ -n "$NAMES" ] || echo "No bots installed yet — run: sudo deploy/install.sh <name>"

# 4. Install and restart.
echo "→ installing $BIN"
sudo install -m 0755 chriscord-musicbot "$BIN"
if ! cmp -s "$HERE/chriscord-musicbot@.service" "$UNIT"; then
  sudo install -m 0644 "$HERE/chriscord-musicbot@.service" "$UNIT"
  sudo systemctl daemon-reload
  echo "   updated the systemd unit"
fi
for n in $NAMES; do
  svc="chriscord-musicbot@$n"
  if systemctl is-enabled --quiet "$svc" 2>/dev/null || systemctl is-active --quiet "$svc"; then
    echo "→ restarting $svc"
    sudo systemctl restart "$svc"
  else
    echo "   $svc isn't enabled — left stopped (start it: sudo systemctl enable --now $svc)"
  fi
done

# 5. Did they come back up?
sleep 3
FAILED=0
for n in $NAMES; do
  svc="chriscord-musicbot@$n"
  systemctl is-enabled --quiet "$svc" 2>/dev/null || continue
  if systemctl is-active --quiet "$svc"; then
    echo "✓ $svc running $NEW"
  else
    echo "✗ $svc didn't start — see: journalctl -u $svc -n 50" >&2
    FAILED=1
  fi
done
exit $FAILED
