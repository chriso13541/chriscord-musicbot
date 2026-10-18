# chriscord-musicbot

A music bot for a Chriscord server, in the spirit of the Discord ones: type
`!play <link>` in chat and it joins your voice channel and plays it.

It's its own small service that runs next to the server and joins as a
**bot**: it has a token made in the app, someone with **Manage Bots** adds it
to the server, and it shows with a BOT tag and who made it. Chat with it and
the music are end-to-end encrypted like everything else. It's built on the
[Chriscord bot SDK](https://github.com/chriso13541/chriscord-bot-sdk), which
does the signing in, encryption and calls.

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
straight from YouTube until skipped. Hours-long
videos (over `BOT_STREAM_OVER`, 1 hour) are streamed the same way rather
than downloaded first, so there's no wait and no length limit.

- It only joins a voice channel someone's in — you have to be in one to
  `!play`, and it comes to yours. It sees only the channels it was given
  when it was added, so give it the voice channels it should play in and a
  text channel for commands.
- After **5 minutes alone** in its channel it leaves (and empties the queue).
- `pause`, `skip`, `clear` and `disconnect` need you in the bot's channel.
- If it's busy playing for people in another channel, it says so instead of
  leaving them.
- Moved or disconnected by a moderator, it goes along with it. If its
  connection to the server drops, it rejoins the same call when it's back.

## Status

While it plays, the bot's custom status in the member list says what:
`🎶 Song title` (`🔴` for a live stream, `Paused: …` while paused), and it's
cleared when nothing's playing. It's sent the way the app sends yours —
encrypted with the server's status key, padded and sent about once a
minute — so the server can't see what's playing either. (Servers from
before encrypted statuses get no status from the bot.)

## Setting it up on a server

1. **Make the bot** in the app: Settings → Bots → New bot. Call it what you
   like ("Music"), and say what it keeps in the description ("Keeps the
   song queue in memory; downloads are deleted once played"). The app shows
   its **token** once: copy it into the bot's settings file
   (`CHRISCORD_BOT_TOKEN`). It also gives an **Add to server** link.
2. **Add it to the server**: someone with Manage Bots (the owner always has
   it) clicks the link in a chat, or pastes it into the server's Manage Bots
   window (click the server's name), ticks what it may do (it asks for Send
   Messages, Connect and Speak; Listen isn't needed, since it never plays
   anyone's voice) and picks the channels it sees: the voice channels to
   play in and a text channel for commands.
3. Start it (below). It signs in with the token; no join key, invite or
   owner key is involved.

To retire it, revoke it in the app (Settings → Bots). Servers that have it
drop it, and the token stops working. To just take it off one server,
remove it there (click the server's name, then Manage Bots).

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
  each channel's keys with it the same way they do with any newcomer (the
  SDK does this, the way the app does). Its replies are encrypted too.
- **yt-dlp updates:** the bot keeps its own copy of the official standalone
  yt-dlp in `<data dir>/bin/yt-dlp`. Every 24 hours it checks the latest
  GitHub release (`SHA2-256SUMS`) and, if its copy doesn't match, downloads
  the new one, checks the hash, and swaps it in. To check right now:
  `chriscord-musicbot update-ytdlp`.

The encryption, signing in and calls are in the SDK now, which is what has
to keep in step with the app. The `testing_*_test.go` files are the test's
pretend member's own copies (she's an app, not a bot).

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
go mod tidy                     # fetches the SDK and the module checksums
go build -o chriscord-musicbot .
sudo deploy/install.sh          # → chriscord-musicbot@main
sudoedit /etc/chriscord/musicbot-main.env   # CHRISCORD_URL and CHRISCORD_BOT_TOKEN
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

The token is the bot's whole identity: keep the settings file private (the
install does) and with your backups. If it might have leaked, revoke the
bot in the app and make a new one.

### Moving from 0.4

Before 0.5 the bot joined as an ordinary member, with an `identity.key` in
its data directory and `BOT_USERNAME` plus a join key or invite. Now:

1. Kick the old "Music" member in the admin panel (that frees the name).
2. Make the bot in the app and add it to the server, as above.
3. In the settings file, set `CHRISCORD_BOT_TOKEN` and remove
   `BOT_USERNAME`, `CHRISCORD_SERVER_KEY` and `CHRISCORD_INVITE`. Then
   `deploy/update.sh`. The old `identity.key` isn't used any more; delete it
   whenever you like.

The server has to be one with bots (from the same update).

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
pretend member who types the commands and listens: it checks the bot signs
in with its certificate, the audio reaching the call decrypts with the key
the bot gave her, at real-time pace, and decodes to the tone that was
queued; plus pause, resume, skip, clear, disconnect and leaving when alone.

```sh
go test -tags integration -v -run TestBot .
```

## Worth knowing

- The bot holds the channel keys for the text channels it was given, so
  whoever has its token can read those channels. Keep its settings file
  private (the install does), and give it only the channels it needs.
- YouTube sometimes refuses a download with **403 Forbidden**, more often
  when many are fetched in a row (a mix). The bot tries each download three
  times a few seconds apart, which usually gets past it; if a track still
  fails it says so and moves on. Keeping yt-dlp current (it updates itself
  daily) and having Deno installed are the real fixes; for persistent
  trouble, cookies from a signed-in account help (`BOT_YTDLP_ARGS`).
- Volume is one setting for everyone (`BOT_VOLUME`, a percent: 100 sits
  about level with voices); each listener can also
  turn the bot down in their app like any other person in the call.
