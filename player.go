package main

// player.go — the queue and playback. Each track is downloaded with yt-dlp
// (the next one in the background while the current one plays), then
// ffmpeg turns it into 20 ms Opus packets, which go to the call — sealed
// for its encryption — one every 20 ms.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type track struct {
	trackInfo
	requester string
	board     string // the text channel it was asked for in

	dlOnce sync.Once
	dlDone chan struct{}
	dlDir  string
	file   string
	dlErr  error
	cancel context.CancelFunc
}

type player struct {
	b *Bot

	mu      sync.Mutex
	queue   []*track
	current *track
	paused  bool
	started time.Time     // when the current track started (pauses excluded)
	played  time.Duration // how much of it has been sent
	stopCur context.CancelFunc
	wake    chan struct{}
	resume  chan struct{} // closed when unpaused
	dlSlot  chan struct{} // held while a download runs (one at a time)
}

func newPlayer(b *Bot) *player {
	return &player{b: b, wake: make(chan struct{}, 1), resume: make(chan struct{}), dlSlot: make(chan struct{}, 1)}
}

// kick wakes the play loop (something was queued, or the call came up).
func (p *player) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// enqueue adds tracks; returns the position of the first one (1 = playing
// next) and how many were added.
func (p *player) enqueue(ts []*track) (pos, added int) {
	p.mu.Lock()
	room := p.b.cfg.MaxQueue - len(p.queue)
	if room < len(ts) {
		ts = ts[:max(room, 0)]
	}
	pos = len(p.queue) + 1
	if p.current == nil {
		pos = len(p.queue)
	}
	p.queue = append(p.queue, ts...)
	p.mu.Unlock()
	p.prefetch()
	p.kick()
	return pos, len(ts)
}

// prefetch starts downloading the first track waiting (and the one
// playing, if it somehow isn't yet).
//
// The queue itself is just links. Downloads happen one at a time, on
// demand: the track playing (if it isn't downloaded yet), and then — only
// once that's done — the one up next, so it's ready when its turn comes.
// Nothing further down the queue is fetched until it's next in line.
func (p *player) prefetch() {
	p.mu.Lock()
	cur := p.current
	var next *track
	if len(p.queue) > 0 {
		next = p.queue[0]
	}
	p.mu.Unlock()
	if cur != nil {
		p.startDownload(cur)
		if !cur.downloaded() {
			return // the next one waits until this one's done
		}
	}
	if next != nil {
		p.startDownload(next)
	}
}

// downloaded: its download has finished (or failed, or it's a stream).
func (t *track) downloaded() bool {
	if t.dlDone == nil {
		return false
	}
	select {
	case <-t.dlDone:
		return true
	default:
		return false
	}
}

func (p *player) startDownload(t *track) {
	t.dlOnce.Do(func() {
		t.dlDone = make(chan struct{})
		if t.Live { // streamed when it plays, not downloaded
			t.cancel = func() {}
			t.dlErr = errLive
			close(t.dlDone)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		t.cancel = cancel
		go func() {
			defer p.prefetch() // done: the one up next can start
			defer close(t.dlDone)
			defer cancel()
			// A YouTube video with no known length (as search and playlist
			// results list live streams) may be live: check before
			// downloading, since a live stream is played, not downloaded.
			if t.Duration == 0 && isYouTube(t.URL) {
				if live, upcoming := p.b.ytdlp.liveStatus(ctx, t.URL); live {
					t.dlErr = errLive
					return
				} else if upcoming {
					t.dlErr = errors.New("that stream hasn't started yet")
					return
				}
			}
			// One download at a time, whatever's going on.
			select {
			case p.dlSlot <- struct{}{}:
				defer func() { <-p.dlSlot }()
			case <-ctx.Done():
				t.dlErr = ctx.Err()
				return
			}
			dir, err := os.MkdirTemp(filepath.Join(p.b.cfg.DataDir, "downloads"), "t-")
			if err != nil {
				t.dlErr = err
				return
			}
			t.dlDir = dir
			started := time.Now()
			t.file, t.dlErr = p.b.ytdlp.download(ctx, t.URL, dir)
			if t.dlErr == nil {
				logf("player: downloaded %q in %v", t.Title, time.Since(started).Round(100*time.Millisecond))
			}
		}()
	})
}

// discard cancels a track's download and deletes its file.
func (t *track) discard() {
	if t.dlDone == nil {
		return
	}
	t.cancel()
	go func() {
		<-t.dlDone
		if t.dlDir != "" {
			os.RemoveAll(t.dlDir)
		}
	}()
}

func (p *player) setPaused(on bool) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.paused == on {
		return false
	}
	p.paused = on
	if !on {
		close(p.resume)
	} else {
		p.resume = make(chan struct{})
	}
	go p.showStatus()
	return true
}

// showStatus puts what's playing in the bot's status (status.go): the
// track, "Paused: …" while paused, nothing when nothing's playing.
func (p *player) showStatus() {
	p.mu.Lock()
	cur, paused, waiting := p.current, p.paused, len(p.queue)
	p.mu.Unlock()
	switch {
	case cur == nil && waiting > 0:
		// Between tracks: the next one's about to start and replace it.
	case cur == nil:
		p.b.c.SetStatus("", "")
	case paused:
		p.b.c.SetStatus("🎶", "Paused: "+cur.Title)
	case cur.Live:
		p.b.c.SetStatus("🔴", cur.Title)
	default:
		p.b.c.SetStatus("🎶", cur.Title)
	}
}

// skip ends the current track; the next one starts.
func (p *player) skip() *track {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.current
	if p.stopCur != nil {
		p.stopCur()
	}
	return cur
}

// clear empties the queue (what's playing carries on).
func (p *player) clear() int {
	p.mu.Lock()
	q := p.queue
	p.queue = nil
	p.mu.Unlock()
	for _, t := range q {
		t.discard()
	}
	return len(q)
}

// stopAll empties the queue and stops what's playing.
func (p *player) stopAll() {
	p.clear()
	p.skip()
}

type queueView struct {
	current *track
	elapsed time.Duration
	paused  bool
	queue   []*track
}

func (p *player) view() queueView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return queueView{current: p.current, elapsed: p.played, paused: p.paused, queue: append([]*track(nil), p.queue...)}
}

// loop plays the queue, forever.
func (p *player) loop() {
	for {
		p.mu.Lock()
		var next *track
		board, _ := p.b.inVoice()
		if board != "" && len(p.queue) > 0 {
			next = p.queue[0]
			p.queue = p.queue[1:]
			ctx, cancel := context.WithCancel(context.Background())
			p.current, p.paused, p.played, p.stopCur = next, false, 0, cancel
			p.resume = make(chan struct{})
			p.mu.Unlock()
			p.prefetch()
			err := p.play(ctx, next)
			cancel()
			next.discard()
			p.mu.Lock()
			p.current, p.stopCur, p.paused = nil, nil, false
			p.mu.Unlock()
			p.b.speaking(false)
			p.showStatus()
			if err != nil && !errors.Is(err, context.Canceled) {
				logf("player: %q: %v", next.Title, err)
				go p.b.say(next.board, fmt.Sprintf("Couldn't play %s — %v", next.Title, err))
			}
			continue
		}
		p.mu.Unlock()
		select {
		case <-p.wake:
		case <-time.After(2 * time.Second):
		}
	}
}

// play waits for the download, then streams the track to the call.
func (p *player) play(ctx context.Context, t *track) error {
	p.startDownload(t)
	select {
	case <-t.dlDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	live := errors.Is(t.dlErr, errLive)
	if t.dlErr != nil && !live {
		return t.dlErr
	}
	if err := p.waitForCall(ctx); err != nil {
		return err
	}
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()

	input := t.file
	var src *exec.Cmd // a live stream: yt-dlp fetches it, ffmpeg reads it from yt-dlp
	var srcErr limitedBuffer
	if live {
		p.mu.Lock()
		t.Live = true
		p.mu.Unlock()
		src = p.b.ytdlp.streamCmd(ctx, t.URL)
		src.Stderr = &srcErr
		input = "pipe:0"
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	if !live {
		args = append(args, "-nostdin")
	}
	args = append(args, "-i", input, "-vn", "-map", "0:a:0")
	if v := p.b.cfg.Volume; v != 1 {
		args = append(args, "-af", "volume="+strconv.FormatFloat(v, 'f', 3, 64))
	}
	args = append(args, "-ac", strconv.Itoa(p.b.cfg.Channels), "-ar", "48000",
		"-c:a", "libopus", "-b:a", p.b.cfg.Bitrate, "-application", "audio",
		"-frame_duration", "20", "-page_duration", "20000", "-f", "ogg", "pipe:1")
	cmd := killTree(exec.CommandContext(ctx, p.b.cfg.FFmpeg, args...))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if src != nil {
		pipe, err := src.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stdin = pipe
		if err := src.Start(); err != nil {
			return fmt.Errorf("yt-dlp: %w", err)
		}
		defer func() { stopTree(src); _ = src.Wait() }()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	defer func() { stopTree(cmd); _ = cmd.Wait() }()

	// No sound at all after a while (a stream that never starts, a file
	// ffmpeg can't read): give up with whatever yt-dlp / ffmpeg said.
	firstAudio := make(chan struct{})
	var stalled atomicBool
	go func() {
		wait := 30 * time.Second
		if live {
			wait = 60 * time.Second
		}
		select {
		case <-firstAudio:
		case <-ctx.Done():
		case <-time.After(wait):
			stalled.set()
			stopAll()
		}
	}()
	stallErr := func() error {
		why := lastLine(srcErr.String(), "")
		if why == "" {
			why = lastLine(stderr.String(), "no sound came through")
		}
		return fmt.Errorf("no sound came through — %s", friendlyError(errors.New(why)))
	}

	if live {
		logf("player: streaming %q live (asked for by %s)", t.Title, t.requester)
	} else {
		logf("player: playing %q (asked for by %s)", t.Title, t.requester)
	}
	go p.b.say(t.board, iconPlay+" Now playing: "+t.Title+lengthNote(t.trackInfo))
	p.showStatus()
	p.b.speaking(true)

	r := newOggOpusReader(stdout)
	clock := time.Now()
	sent := 0
	for {
		// Paused: hold here (ffmpeg just waits on the pipe).
		p.mu.Lock()
		paused, resume := p.paused, p.resume
		p.mu.Unlock()
		if paused && live {
			// A live stream keeps flowing while paused (held, it would fall
			// behind and its address expire): read on, send nothing.
			p.b.speaking(false)
			for p.isPaused() && ctx.Err() == nil {
				if _, err := r.next(); err != nil {
					break
				}
			}
			p.b.speaking(true)
			clock, sent = time.Now(), 0
			continue
		}
		if paused {
			p.b.speaking(false)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := p.waitForCall(ctx); err != nil {
				return err
			}
			p.b.speaking(true)
			clock, sent = time.Now(), 0
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pkt, err := r.next()
		if stalled.get() {
			return stallErr()
		}
		if err != nil && ctx.Err() != nil {
			return ctx.Err() // skipped or stopped: not an error
		}
		if err == nil && sent == 0 {
			select {
			case <-firstAudio:
			default:
				close(firstAudio)
			}
		}
		if err == io.EOF {
			if live {
				return fmt.Errorf("the stream ended%s", errSuffix(srcErr.String()))
			}
			if waitErr := cmd.Wait(); waitErr != nil && stderr.String() != "" {
				return fmt.Errorf("ffmpeg: %s", lastLine(stderr.String(), waitErr.Error()))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading ffmpeg's output: %w", err)
		}
		// The call dropped (reconnecting): wait for it, then carry on.
		if _, ready := p.b.inVoice(); !ready {
			if err := p.waitForCall(ctx); err != nil {
				return err
			}
			clock, sent = time.Now(), 0
		}
		if err := p.b.c.WriteOpus(pkt); err != nil && sent == 0 {
			logf("player: sending audio: %v", err)
		}
		sent++
		p.mu.Lock()
		p.played += chriscordbot.OpusFrame
		p.mu.Unlock()
		// One packet per 20 ms, by the clock (not by sleeping 20 ms each
		// time, which drifts).
		if d := time.Until(clock.Add(time.Duration(sent) * chriscordbot.OpusFrame)); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		} else if d < -time.Second {
			clock, sent = time.Now(), 0 // fell far behind (machine stalled): don't burst
		}
	}
}

func (p *player) isPaused() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.paused }

func errSuffix(stderr string) string {
	if l := lastLine(stderr, ""); l != "" {
		return " — " + friendlyError(errors.New(l)).Error()
	}
	return ""
}

type atomicBool struct{ v atomic.Bool }

func (a *atomicBool) set()      { a.v.Store(true) }
func (a *atomicBool) get() bool { return a.v.Load() }

// waitForCall waits until the bot's call connection is up (and its key
// has had a moment to reach everyone).
func (p *player) waitForCall(ctx context.Context) error {
	waited := false
	for {
		board, ready := p.b.inVoice()
		if board == "" {
			return context.Canceled // left the call
		}
		if ready {
			if waited {
				select { // give the first sending key time to arrive
				case <-time.After(time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		waited = true
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func lengthNote(t trackInfo) string {
	if t.Live {
		return " (🔴 live)"
	}
	if t.Duration <= 0 {
		return ""
	}
	return " (" + clockTime(t.Duration) + ")"
}

func clockTime(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// ── Ogg ───────────────────────────────────────────────────────────────────────

// oggOpusReader splits an Ogg stream into its Opus packets (by the pages'
// lacing values), skipping the OpusHead and OpusTags headers.
type oggOpusReader struct {
	r       *bufio.Reader
	pending [][]byte
	partial []byte
}

func newOggOpusReader(r io.Reader) *oggOpusReader {
	return &oggOpusReader{r: bufio.NewReaderSize(r, 64<<10)}
}

func (o *oggOpusReader) next() ([]byte, error) {
	for len(o.pending) == 0 {
		if err := o.readPage(); err != nil {
			return nil, err
		}
	}
	pkt := o.pending[0]
	o.pending = o.pending[1:]
	return pkt, nil
}

func (o *oggOpusReader) readPage() error {
	var hdr [27]byte
	if _, err := io.ReadFull(o.r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return io.EOF
		}
		return err
	}
	if string(hdr[:4]) != "OggS" {
		return errors.New("not an Ogg stream")
	}
	_ = binary.LittleEndian // (granule etc. unused)
	segs := make([]byte, hdr[26])
	if _, err := io.ReadFull(o.r, segs); err != nil {
		return io.EOF
	}
	total := 0
	for _, s := range segs {
		total += int(s)
	}
	body := make([]byte, total)
	if _, err := io.ReadFull(o.r, body); err != nil {
		return io.EOF
	}
	off := 0
	for _, s := range segs {
		o.partial = append(o.partial, body[off:off+int(s)]...)
		off += int(s)
		if s < 255 { // a packet ends here
			pkt := o.partial
			o.partial = nil
			if len(pkt) == 0 || isOpusHeader(pkt) {
				continue
			}
			o.pending = append(o.pending, pkt)
		}
	}
	return nil
}

func isOpusHeader(p []byte) bool {
	return len(p) >= 8 && (string(p[:8]) == "OpusHead" || string(p[:8]) == "OpusTags")
}

// limitedBuffer keeps the last few KB written to it (ffmpeg's errors).
type limitedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > 8192 {
		l.buf = l.buf[len(l.buf)-8192:]
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return string(l.buf) }
