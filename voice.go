package main

// voice.go — the bot's WebRTC connection to the server's call, built the
// same way as the app's (voice.go there): its own audio on the first
// section, then one receive-only placeholder per person already in the
// call (in the order sent as expected_others — the server maps them by
// position), at least one. Everyone else's audio does arrive on those; the
// bot just throws it away (give the bot a role without Listen and the
// server won't even send it).

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

const (
	sampleRate = 48000
	frameDur   = 20 * time.Millisecond
)

type voiceSession struct {
	b     *Bot
	board string
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	// who the placeholders are for, in order (expected_others)
	expected []string

	iceMu      sync.Mutex
	offerSent  bool
	remoteSet  bool
	pendingOut []webrtc.ICECandidateInit
	pendingIn  []webrtc.ICECandidateInit

	stateMu   sync.Mutex
	connected bool
	closed    bool
}

func newVoiceSession(b *Bot, board string, others []string) (*voiceSession, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}
	var se webrtc.SettingEngine
	se.SetICETimeouts(4*time.Second, 6*time.Second, 2*time.Second)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: sampleRate, Channels: 1},
		"audio", "chriscord-musicbot",
	)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return nil, err
	}
	n := max(len(others), 1)
	for i := 0; i < n; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			logf("voice: placeholder %d: %v", i, err)
		}
	}
	s := &voiceSession{b: b, board: board, pc: pc, track: track}
	s.expected = others

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.iceMu.Lock()
		if !s.offerSent {
			s.pendingOut = append(s.pendingOut, init)
			s.iceMu.Unlock()
			return
		}
		s.iceMu.Unlock()
		s.sendCandidate(init)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		logf("voice: connection %s", st)
		s.stateMu.Lock()
		s.connected = st == webrtc.PeerConnectionStateConnected
		closed := s.closed
		s.stateMu.Unlock()
		if st == webrtc.PeerConnectionStateConnected {
			b.pl.kick()
		}
		if st == webrtc.PeerConnectionStateFailed && !closed {
			go b.restartCall(s)
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Others' voices (or cameras): read and dropped, never decoded.
		buf := make([]byte, 1500)
		for {
			if _, _, err := t.Read(buf); err != nil {
				return
			}
		}
	})
	return s, nil
}

func (s *voiceSession) offer() error {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	expected := s.expected
	if expected == nil {
		expected = []string{}
	}
	if err := s.b.sendJSON(map[string]interface{}{
		"type": "voice_offer", "board_id": s.board, "sdp": offer.SDP, "expected_others": expected,
	}); err != nil {
		return err
	}
	s.b.e2ee.begin(s.board, s.b.username())
	s.iceMu.Lock()
	s.offerSent = true
	held := s.pendingOut
	s.pendingOut = nil
	s.iceMu.Unlock()
	for _, c := range held {
		s.sendCandidate(c)
	}
	return nil
}

func (s *voiceSession) sendCandidate(init webrtc.ICECandidateInit) {
	_ = s.b.sendJSON(map[string]interface{}{
		"type": "voice_ice", "board_id": s.board,
		"candidate": init.Candidate, "sdp_mid": init.SDPMid, "sdp_mline_index": init.SDPMLineIndex,
	})
}

func (s *voiceSession) onAnswer(sdp string) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		logf("voice: applying the answer: %v", err)
		return
	}
	s.iceMu.Lock()
	s.remoteSet = true
	held := s.pendingIn
	s.pendingIn = nil
	s.iceMu.Unlock()
	for _, c := range held {
		s.addCandidate(c)
	}
}

func (s *voiceSession) onICE(candidate string, mid *string, idx *uint16) {
	init := webrtc.ICECandidateInit{Candidate: candidate, SDPMid: mid, SDPMLineIndex: idx}
	s.iceMu.Lock()
	if !s.remoteSet {
		s.pendingIn = append(s.pendingIn, init)
		s.iceMu.Unlock()
		return
	}
	s.iceMu.Unlock()
	s.addCandidate(init)
}

// addCandidate adds one of the server's candidates, plus (like the app) a
// copy at the address the bot reaches the server at, for when the server
// advertises a LAN address the bot can't reach.
func (s *voiceSession) addCandidate(init webrtc.ICECandidateInit) {
	if err := s.pc.AddICECandidate(init); err != nil {
		logf("voice: server candidate %q: %v", init.Candidate, err)
	}
	if c := serverAddressCopy(s.b.cfg.ServerURL, init.Candidate); c != "" {
		cp := init
		cp.Candidate = c
		_ = s.pc.AddICECandidate(cp)
	}
}

func (s *voiceSession) onRenegotiate(sdp string) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		logf("voice: renegotiate: %v", err)
		return
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		logf("voice: renegotiate answer: %v", err)
		return
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		logf("voice: renegotiate local description: %v", err)
		return
	}
	_ = s.b.sendJSON(map[string]string{"type": "voice_renegotiate_answer", "board_id": s.board, "sdp": answer.SDP})
}

func (s *voiceSession) isReady() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.connected && !s.closed
}

// write sends one Opus packet (20 ms), encrypted for the call.
func (s *voiceSession) write(packet []byte) error {
	sealed := s.b.e2ee.seal(kindAudio, packet)
	if sealed == nil {
		return fmt.Errorf("call encryption isn't running")
	}
	return s.track.WriteSample(media.Sample{Data: sealed, Duration: frameDur})
}

func (s *voiceSession) close() {
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	s.closed, s.connected = true, false
	s.stateMu.Unlock()
	_ = s.pc.Close()
}

// serverAddressCopy: the app's trick — a private host candidate copied to
// the server's own (public) address, when the bot reaches it by one.
func serverAddressCopy(serverURL, candidate string) string {
	parts := strings.Fields(candidate)
	if len(parts) < 8 || parts[6] != "typ" || parts[7] != "host" || !strings.EqualFold(parts[2], "udp") {
		return ""
	}
	lanIP := net.ParseIP(parts[4])
	if lanIP == nil || lanIP.To4() == nil || !lanIP.IsPrivate() {
		return ""
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	var serverIP net.IP
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		serverIP = ip.To4()
	} else if ips, err := net.LookupIP(u.Hostname()); err == nil {
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				serverIP = v4
				break
			}
		}
	}
	if serverIP == nil || serverIP.IsPrivate() || serverIP.IsLoopback() || serverIP.Equal(lanIP) {
		return ""
	}
	priority, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return ""
	}
	parts[0] += "d"
	if priority > 2 {
		parts[3] = strconv.FormatUint(priority-2, 10)
	}
	parts[4] = serverIP.String()
	return strings.Join(parts, " ")
}
