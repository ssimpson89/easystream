// Package preview serves a low-latency WebRTC preview of the capture source.
//
// Architecture:
//
//	FFmpeg --H.264 RTP--> 127.0.0.1:UDP --> pion PeerConnection --> browser <video>
//
// The browser sends an SDP offer to POST /api/preview/webrtc/offer. We:
//  1. Tear down any previous session
//  2. Spin up FFmpeg encoding H.264 to a localhost UDP port
//  3. Listen on that UDP port and forward RTP packets to a pion video track
//  4. Negotiate the answer SDP and return it
//
// Browser then plays the track in a <video> element. End-to-end latency
// is well under a second since everything is on localhost (no STUN/TURN,
// no ICE candidate gathering over the public internet).
package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// rtpPort is the local UDP port FFmpeg sends RTP to. Picked from the IANA
// ephemeral range so it won't collide with common services.
const rtpPort = 52001

// Server manages the WebRTC preview pipeline. One active session at a time
// (the operator's browser tab). macOS avfoundation locks a capture device
// to one process, so Block() / Unblock() let the main stream claim the
// device by tearing down the preview's FFmpeg.
type Server struct {
	mu      sync.Mutex
	logger  *log.Logger
	config  ffmpeg.Config
	blocked bool

	// Current session, if any.
	session *session
}

type session struct {
	pc      *webrtc.PeerConnection
	cmd     *exec.Cmd
	udp     *net.UDPConn
	cancel  context.CancelFunc
	closed  bool
	closeMu sync.Mutex
}

func NewServer(logger *log.Logger) *Server {
	return &Server{logger: logger}
}

func (s *Server) UpdateConfig(config ffmpeg.Config) {
	s.mu.Lock()
	changed := s.config.Input != config.Input
	s.config = config
	sess := s.session
	s.mu.Unlock()
	// Source changed mid-preview — tear down so the next offer spins up
	// FFmpeg with the new input.
	if changed && sess != nil {
		s.closeSession(sess, "source changed")
	}
}

// Block tears down any active preview and prevents new sessions from
// starting so the main stream can claim the capture device.
func (s *Server) Block() {
	s.mu.Lock()
	s.blocked = true
	sess := s.session
	s.mu.Unlock()
	if sess != nil {
		s.closeSession(sess, "blocked by main stream")
	}
}

// Unblock allows new preview sessions to start.
func (s *Server) Unblock() {
	s.mu.Lock()
	s.blocked = false
	s.mu.Unlock()
}

// IsBlocked reports whether previews are currently disallowed.
func (s *Server) IsBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

// ServeHTTP accepts the browser's WebRTC SDP offer and returns our answer.
// One session at a time — any existing session is torn down first.
//
// Browser-side flow:
//
//	const pc = new RTCPeerConnection();
//	pc.addTransceiver('video', { direction: 'recvonly' });
//	pc.ontrack = e => { videoEl.srcObject = e.streams[0]; };
//	const offer = await pc.createOffer();
//	await pc.setLocalDescription(offer);
//	const r = await fetch('/api/preview/webrtc/offer', {
//	    method: 'POST', headers: {'Content-Type': 'application/json'},
//	    body: JSON.stringify(pc.localDescription),
//	});
//	await pc.setRemoteDescription(await r.json());
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	if s.blocked {
		s.mu.Unlock()
		http.Error(w, "preview paused while stream is live", http.StatusServiceUnavailable)
		return
	}
	config := s.config
	prev := s.session
	s.session = nil
	s.mu.Unlock()
	if prev != nil {
		s.closeSession(prev, "replaced by new session")
	}

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	sess, answer, err := s.startSession(config, offer)
	if err != nil {
		s.logger.Printf("preview: session start failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// startSession spins up FFmpeg, the UDP listener, and the PeerConnection,
// negotiates with the browser's offer, and returns our answer.
func (s *Server) startSession(config ffmpeg.Config, offer webrtc.SessionDescription) (*session, *webrtc.SessionDescription, error) {
	// PeerConnection with no ICE servers — localhost only.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, fmt.Errorf("create peer connection: %w", err)
	}

	// Video track that the browser will receive.
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "easystream-preview",
	)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("create video track: %w", err)
	}
	rtpSender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("add track: %w", err)
	}

	// Drain RTCP (otherwise browser feedback fills up the buffer).
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Listen for RTP from FFmpeg.
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpPort})
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("rtp listen: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		pc:     pc,
		udp:    listener,
		cancel: cancel,
	}

	// Forward RTP packets from FFmpeg to the WebRTC track.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := listener.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := track.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	// Start FFmpeg. Encoding to H.264 RTP at 640x360, low-latency tuning.
	args := previewArgs(config, rtpPort)
	binary := config.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if err := cmd.Start(); err != nil {
		_ = pc.Close()
		_ = listener.Close()
		cancel()
		return nil, nil, fmt.Errorf("ffmpeg start: %w", err)
	}
	sess.cmd = cmd
	s.logger.Printf("preview: ffmpeg started (pid %d)", cmd.Process.Pid)

	// Reap FFmpeg if it exits on its own (capture error, etc.) so we tear
	// down the session cleanly.
	go func() {
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			s.logger.Printf("preview: ffmpeg exited: %v", err)
			s.closeSession(sess, "ffmpeg exited")
		}
	}()

	// Tear down when the peer connection fails/closes — e.g., browser
	// tab closed, network changed, etc.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed || state == webrtc.ICEConnectionStateDisconnected {
			s.closeSession(sess, fmt.Sprintf("ice %s", state.String()))
		}
	})

	// Negotiate.
	if err := pc.SetRemoteDescription(offer); err != nil {
		s.closeSession(sess, "set remote")
		return nil, nil, fmt.Errorf("set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.closeSession(sess, "create answer")
		return nil, nil, fmt.Errorf("create answer: %w", err)
	}

	// Wait for ICE gathering to complete so we return all candidates inline
	// (avoids trickle ICE complexity for a localhost-only flow).
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.closeSession(sess, "set local")
		return nil, nil, fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-time.After(2 * time.Second):
		// Localhost ICE shouldn't take 2s; proceed with what we have.
		s.logger.Printf("preview: ICE gathering took >2s, proceeding anyway")
	}

	return sess, pc.LocalDescription(), nil
}

// closeSession tears down a preview session and clears it from Server state.
// Safe to call multiple times.
func (s *Server) closeSession(sess *session, reason string) {
	sess.closeMu.Lock()
	already := sess.closed
	sess.closed = true
	sess.closeMu.Unlock()
	if already {
		return
	}
	s.logger.Printf("preview: closing session (%s)", reason)
	sess.cancel()
	if sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	if sess.udp != nil {
		_ = sess.udp.Close()
	}
	if sess.pc != nil {
		_ = sess.pc.Close()
	}
	s.mu.Lock()
	if s.session == sess {
		s.session = nil
	}
	s.mu.Unlock()
}

// previewArgs builds ffmpeg arguments for low-latency H.264 RTP output.
// No framerate/resolution forced on the capture device; FFmpeg negotiates
// with the hardware. Output is scaled to 640x360 for the preview track.
func previewArgs(config ffmpeg.Config, rtpPort int) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
	}

	switch config.Input.Kind {
	case ffmpeg.InputTestVideo:
		args = append(args,
			"-re",
			"-f", "lavfi",
			"-i", "testsrc2=size=640x360:rate=15",
		)
	default:
		backend := config.Input.Backend
		if backend == "" {
			backend = ffmpeg.PlatformBackend()
		}
		device := config.Input.VideoDevice
		switch backend {
		case "avfoundation":
			if config.Input.AudioDevice != "" {
				device = device + ":" + config.Input.AudioDevice
			} else {
				device = device + ":none"
			}
			args = append(args, "-f", "avfoundation", "-i", device)
		case "dshow":
			args = append(args, "-f", "dshow", "-i", "video="+device)
		case "v4l2":
			args = append(args, "-f", "v4l2", "-i", device)
		case "decklink":
			args = append(args, "-f", "decklink", "-i", device)
		default:
			args = append(args, "-f", backend, "-i", device)
		}
	}

	// Encode H.264 at low resolution with ultrafast/zerolatency tuning.
	// -an drops audio (no need for the confidence preview).
	// payload_type 96 matches the default WebRTC dynamic PT for H.264.
	args = append(args,
		"-an",
		"-vf", "scale=640:360:force_original_aspect_ratio=decrease",
		"-r", "15",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-profile:v", "baseline",
		"-level:v", "3.1",
		"-pix_fmt", "yuv420p",
		"-g", "30",
		"-keyint_min", "30",
		"-bf", "0",
		"-b:v", "800k",
		"-payload_type", "96",
		"-f", "rtp",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", rtpPort),
	)
	return args
}
