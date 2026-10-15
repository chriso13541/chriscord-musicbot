# chriscord-musicbot

A music bot for a Chriscord server, in the spirit of the Discord ones: type
`!play <link>` in chat and it joins your voice channel and plays it.

It's its own small service that runs next to the server and joins like any
member — its own account (an identity key it keeps), end-to-end encrypted
chat for reading commands and replying, and an end-to-end encrypted call for
the music. **Nothing changes on the server.**

## Commands

| Command | What it does |
|---|---|
| `!play <link>` | Joins your voice channel and queues it. Anything yt-dlp handles; playlists and mixes add up to 50 tracks. |
| `!play <words>` | Plays the top YouTube result. |
| `!search <words>` | Lists the top 10 results; then `!play <number>` (or `!pick <number>`) plays one. |
| `!pause` / `!resume` | Pause, carry on (`!play` with nothing after it resumes too). |
| `!skip` | Next track. |
| `!stop` | Stops what's playing and empties the queue; stays in the call. |
| `!clear` | Empties the queue — what's playing keeps going. |
| `!disconnect` | Leaves the call (also `!leave`, `!dc`). |
| `!queue` / `!np` | What's playing and what's next. |
| `!help` | The list. |

YouTube links in all their shapes work: `youtu.be`, `/shorts/`, `/live/`,
`music.youtube.com`, mobile links, and links full of `si=`/`pp=`/`t=`
tracking bits. A video opened inside a playlist queues the playlist from
that video on; a mix ("radio", `list=RD…`) queues the mix; Watch Later /
Liked links (private) play just the video. Private and deleted videos in a
playlist are skipped. **Live streams** (24/7 lofi and the like) play
straight from YouTube until skipped.

- It only joins a voice channel someone's in — you have to be in one to
  `!play`, and it comes to yours.
- After **5 minutes alone** in its channel it leaves (and empties the queue).
- `pause`, `skip`, `clear` and `disconnect` need you in the bot's channel.
- If it's busy playing for people in another channel, it says so instead of
  leaving them.
- Moved or disconnected by a moderator, it goes along with it. If its
  connection to the server drops, it rejoins the same call when it's back.

## How it works

- **Downloads:** each track is fetched with yt-dlp into the data directory
  (the next one in the background while the current one plays) and deleted
  once played. By default it keeps the site's own audio, which is usually
  Opus already — set `BOT_AUDIO_FORMAT=mp3` to have everything converted to
  MP3 first instead (one more lossy step, so it's not the default).
- **Playing:** ffmpeg turns the file into 20 ms Opus packets, which go to
  the call sealed with the bot's call key — the same frame format and key
  exchange as the app (`e2ee.go` / `e2ee_frame.go`), so the server only ever
  forwards ciphertext. It shows the speaking ring while playing.
- **Chat:** commands arrive encrypted like everything else. The bot
  publishes its encryption key when it signs in, and the members' apps share
  each channel's keys with it the same way they do with any newcomer
  (`textcrypt.go`, ported from the app). Its replies are encrypted too.
- **yt-dlp updates:** the bot keeps its own copy of the official standalone
  yt-dlp in `<data dir>/bin/yt-dlp`. Every 24 hours it checks the latest
  GitHub release (`SHA2-256SUMS`) and, if its copy doesn't match, downloads
  the new one, checks the hash, and swaps it in. To check right now:
  `chriscord-musicbot update-ytdlp`.

Files kept in sync with the app — if the app's encryption changes, these
need the same change:

| Here | In chriscord-client-go |
|---|---|
| `e2ee_frame.go` | `e2ee_frame.go` (copied unchanged) |
| `e2ee.go` | `e2ee.go` (key exchange, minus the UI and listening) |
| `textcrypt.go` | `textcrypt.go` (minus attachments and identity pinning) |
| `voice.go` (offer shape) | `voice.go` (`startVoiceSession`) |

## What it needs

- **Go 1.24+** to build.
- **ffmpeg** with libopus (Debian/Ubuntu's `ffmpeg` package has it).
- **Deno**, for YouTube. yt-dlp needs a JavaScript runtime to get YouTube
  audio now; Deno is the one it uses without extra settings. Install it
  system-wide so the service can find it:

  ```sh
  curl -fsSL https://deno.land/install.sh | sudo DENO_INSTALL=/usr/local sh
  ```

  (Other sites work without it.)

## Install (next to the server, e.g. on the Dell)

```sh
go mod tidy                     # fetches the module checksums
go build -o chriscord-musicbot .
sudo deploy/install.sh          # → chriscord-musicbot@main
sudoedit /etc/chriscord/musicbot-main.env   # CHRISCORD_URL, BOT_USERNAME, and the
                                            # server key or an invite code
sudo systemctl enable --now chriscord-musicbot@main
journalctl -u chriscord-musicbot@main -f
```

### Updating

```sh
deploy/update.sh          # as yourself, not sudo; add --pull to git pull first
```

It builds the new binary, installs it, and restarts every installed bot on
it (then checks they came back up). Settings and identities are kept.

One bot per server: for another server hosted on the same box, run
`sudo deploy/install.sh <name>` and point that one's `CHRISCORD_URL` at its
port.

The server key / invite code is only needed for the first sign-in, which
registers the name to the bot's key (`identity.key` in its data directory —
that file *is* the bot's account; keep it with your backups).

### Suggested role

Give the bot a role in the admin panel with **Listen turned off**: it never
plays anyone's voice anyway, and without Listen the server doesn't even send
it. It needs Connect, Speak, View Channels and Send Messages (all on for
@everyone by default). To keep it to certain text channels, use channel
access as for anyone.

## Profile picture

Put `pfp.png` or `pfp.jpg` in the bot's data directory and restart it:

```sh
sudo install -m 0644 my-picture.jpg /var/lib/chriscord-musicbot/second/pfp.jpg
sudo systemctl restart chriscord-musicbot@second
```

It's read once at startup, centre-cropped to a square and scaled to at most
512 px (any photo works), and uploaded the same way the app uploads yours.
Replacing the file and restarting is all it takes to change it. Without a
file, the bot keeps whatever picture the server already has for it.

## Settings

All in the env file — see `deploy/musicbot.env.example` for the full list
(prefix, alone timeout, update interval, bitrate, mono/stereo, volume,
queue and length limits, extra yt-dlp arguments such as cookies).

## Testing

`integration_test.go` runs the real bot against a stand-in server with a
pretend member who types the commands and listens: it checks the audio
reaching the call decrypts with the key the bot gave her, at real-time pace,
and decodes to the tone that was queued; plus pause, resume, skip, clear,
disconnect and leaving when alone.

```sh
go test -tags integration -v -run TestBot .
```

## Worth knowing

- The bot holds the channel keys for any text channel it can see, so
  whoever can read its `identity.key` can read those channels. Keep its
  data directory private (the install does), or limit which channels it can
  see.
- YouTube sometimes refuses a download with **403 Forbidden**, more often
  when many are fetched in a row (a mix). The bot tries each download three
  times a few seconds apart, which usually gets past it; if a track still
  fails it says so and moves on. Keeping yt-dlp current (it updates itself
  daily) and having Deno installed are the real fixes; for persistent
  trouble, cookies from a signed-in account help (`BOT_YTDLP_ARGS`).
- Volume is one setting for everyone (`BOT_VOLUME`); each listener can also
  turn the bot down in their app like any other person in the call.
