// Package preview serves a low-latency WebRTC preview of the capture source.
//
// Architecture:
//
//	FFmpeg --H.264/Opus RTP--> 127.0.0.1:UDP --> pion PeerConnection --> browser <video>
//
// When the main stream is idle, the preview Server runs its own FFmpeg that
// reads the capture device and writes H.264 RTP to a localhost UDP port.
// When the main stream is live, that FFmpeg is torn down (only one process
// can hold the camera on macOS), but the WebRTC session stays alive — the
// main stream's FFmpeg writes its own low-res RTP preview to the same UDP
// port, so packets keep flowing to the browser. The browser sees a brief
// pause during the transition then keeps playing.
package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// PreviewRTPPort is the UDP port that BOTH the preview's own FFmpeg AND the
// main stream's FFmpeg send H.264 RTP packets to. The preview's UDP listener
// reads from this port and forwards to the WebRTC track.
const PreviewRTPPort = 52001

// PreviewAudioRTPPort carries Opus RTP for the browser-side audio meter.
const PreviewAudioRTPPort = 52002

// previewRTPSignature is the substring that uniquely identifies an EasyStream
// ffmpeg process (preview OR main, since both tee to this port). Used by the
// orphan reaper at startup to clean up after `go run` restarts and crashes.
const previewRTPSignature = "rtp://127.0.0.1:52001"

// ReapOrphans kills any ffmpeg processes left behind from a previous
// EasyStream session. We identify them by the unique RTP preview port in
// their argv. Without this, orphans accumulate on every restart and all
// write to the same UDP port, garbling the preview decoder.
//
// Safe to call before any preview session exists.
func ReapOrphans(logger *log.Logger) {
	out, err := exec.Command("pgrep", "-fl", previewRTPSignature).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches — that's fine.
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Format: "PID command line"
		fields := strings.SplitN(line, " ", 2)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		// Double-check it's an ffmpeg process before killing.
		if !strings.Contains(fields[1], "ffmpeg") {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil && proc != nil {
			_ = proc.Signal(syscall.SIGTERM)
			// Give it a moment, then force-kill.
			time.Sleep(150 * time.Millisecond)
			_ = proc.Kill()
			logger.Printf("preview: reaped orphan ffmpeg pid %d", pid)
		}
	}
}

// Server manages the WebRTC preview pipeline. One active session at a time
// (the operator's browser tab). The main stream owns the camera while live,
// so Block() tears down the preview's own FFmpeg but keeps the WebRTC track
// alive so the main stream's preview output can take over.
type Server struct {
	mu      sync.Mutex
	logger  *log.Logger
	config  ffmpeg.Config
	blocked bool

	// Current session, if any.
	session *session
}

type session struct {
	pc       *webrtc.PeerConnection
	videoUDP *net.UDPConn
	audioUDP *net.UDPConn
	cancel   context.CancelFunc

	// Own FFmpeg (running when not blocked). Killed on Block().
	cmdMu     sync.Mutex
	cmd       *exec.Cmd
	ffmpegCtx context.Context
	cmdCancel context.CancelFunc

	// closed is set by closeSession; reaper goroutines check it to avoid
	// double-closing or treating a deliberate kill as an error.
	closed   bool
	closedMu sync.Mutex
}

func NewServer(logger *log.Logger) *Server {
	return &Server{logger: logger}
}

func (s *Server) UpdateConfig(config ffmpeg.Config) {
	s.mu.Lock()
	changed := s.config.Input != config.Input
	s.config = config
	sess := s.session
	blocked := s.blocked
	s.mu.Unlock()
	// Source changed while we're idle (running our own ffmpeg) — restart
	// the ffmpeg with the new input. If blocked (main stream live), do
	// nothing; the main stream's ffmpeg is the source.
	if changed && sess != nil && !blocked {
		s.stopSessionFFmpeg(sess, "source changed")
		s.startSessionFFmpeg(sess, config)
	}
}

// Block tears down the preview's own FFmpeg so the main stream can claim
// the capture device. The WebRTC session stays alive — the main stream's
// FFmpeg will start writing preview RTP to the same UDP port shortly.
func (s *Server) Block() {
	s.mu.Lock()
	s.blocked = true
	sess := s.session
	s.mu.Unlock()
	if sess != nil {
		s.stopSessionFFmpeg(sess, "blocked by main stream")
	}
}

// Unblock resumes the preview's own FFmpeg.
func (s *Server) Unblock() {
	s.mu.Lock()
	s.blocked = false
	sess := s.session
	cfg := s.config
	s.mu.Unlock()
	if sess != nil {
		s.startSessionFFmpeg(sess, cfg)
	}
}

// IsBlocked reports whether the main stream currently owns the camera.
func (s *Server) IsBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

// ServeHTTP accepts the browser's WebRTC SDP offer and returns our answer.
// One session at a time — any existing session is torn down first.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	config := s.config
	blocked := s.blocked
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

	sess, answer, err := s.startSession(config, blocked, offer)
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

// startSession creates the WebRTC PeerConnection and UDP listener, then —
// if we're not blocked — spawns the preview's own FFmpeg. When blocked, the
// main stream's FFmpeg supplies the RTP packets.
func (s *Server) startSession(config ffmpeg.Config, blocked bool, offer webrtc.SessionDescription) (*session, *webrtc.SessionDescription, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, fmt.Errorf("create peer connection: %w", err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "easystream-preview",
	)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("create video track: %w", err)
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("add video track: %w", err)
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "easystream-preview",
	)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("create audio track: %w", err)
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("add audio track: %w", err)
	}

	// Drain RTCP feedback so the buffer doesn't fill up.
	drainRTCP := func(sender *webrtc.RTPSender) {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}
	go drainRTCP(videoSender)
	go drainRTCP(audioSender)

	videoListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: PreviewRTPPort})
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("video rtp listen: %w", err)
	}
	audioListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: PreviewAudioRTPPort})
	if err != nil {
		_ = videoListener.Close()
		_ = pc.Close()
		return nil, nil, fmt.Errorf("audio rtp listen: %w", err)
	}

	_, cancel := context.WithCancel(context.Background())
	sess := &session{
		pc:       pc,
		videoUDP: videoListener,
		audioUDP: audioListener,
		cancel:   cancel,
	}

	// Forward RTP packets from whichever FFmpeg is writing to the port.
	forwardRTP := func(listener *net.UDPConn, track *webrtc.TrackLocalStaticRTP) {
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
	}
	go forwardRTP(videoListener, videoTrack)
	go forwardRTP(audioListener, audioTrack)

	// Tear down the session only on hard failures. "disconnected" is
	// transient and recoverable — letting Pion handle it avoids spurious
	// reconnects that cause visible video stutter.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			s.closeSession(sess, fmt.Sprintf("ice %s", state.String()))
		}
	})

	// Start the preview's own FFmpeg only when the main stream isn't live.
	if !blocked {
		if err := s.startSessionFFmpeg(sess, config); err != nil {
			s.closeSession(sess, "ffmpeg start failed")
			return nil, nil, err
		}
	} else {
		s.logger.Printf("preview: session started in passive mode (main stream feeds RTP)")
	}

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
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.closeSession(sess, "set local")
		return nil, nil, fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-time.After(2 * time.Second):
		s.logger.Printf("preview: ICE gathering took >2s, proceeding anyway")
	}
	return sess, pc.LocalDescription(), nil
}

// startSessionFFmpeg spawns the preview FFmpeg child for an existing session.
// Safe to call when there's no existing child.
func (s *Server) startSessionFFmpeg(sess *session, config ffmpeg.Config) error {
	sess.cmdMu.Lock()
	if sess.cmd != nil {
		sess.cmdMu.Unlock()
		return nil // already running
	}
	args := previewArgs(config, PreviewRTPPort, PreviewAudioRTPPort)
	binary := config.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, args...)
	if err := cmd.Start(); err != nil {
		cancel()
		sess.cmdMu.Unlock()
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	sess.cmd = cmd
	sess.ffmpegCtx = ctx
	sess.cmdCancel = cancel
	sess.cmdMu.Unlock()
	s.logger.Printf("preview: ffmpeg started (pid %d)", cmd.Process.Pid)

	// Reaper. If the ffmpeg exits while we expected it to be running
	// (i.e., we didn't intentionally kill it), close the whole session.
	go func() {
		err := cmd.Wait()
		// If ctx was canceled, the kill was intentional (stopSessionFFmpeg).
		if ctx.Err() != nil {
			return
		}
		s.logger.Printf("preview: ffmpeg exited unexpectedly: %v", err)
		s.closeSession(sess, "ffmpeg exited")
	}()
	return nil
}

// stopSessionFFmpeg kills the preview's own FFmpeg without disturbing the
// WebRTC PeerConnection or UDP listener.
func (s *Server) stopSessionFFmpeg(sess *session, reason string) {
	sess.cmdMu.Lock()
	cmd := sess.cmd
	cancel := sess.cmdCancel
	sess.cmd = nil
	sess.cmdCancel = nil
	sess.ffmpegCtx = nil
	sess.cmdMu.Unlock()
	if cmd == nil {
		return
	}
	s.logger.Printf("preview: stopping own ffmpeg (%s)", reason)
	if cancel != nil {
		cancel()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// closeSession tears down a preview session completely.
func (s *Server) closeSession(sess *session, reason string) {
	sess.closedMu.Lock()
	already := sess.closed
	sess.closed = true
	sess.closedMu.Unlock()
	if already {
		return
	}
	s.logger.Printf("preview: closing session (%s)", reason)
	s.stopSessionFFmpeg(sess, reason)
	sess.cancel()
	if sess.videoUDP != nil {
		_ = sess.videoUDP.Close()
	}
	if sess.audioUDP != nil {
		_ = sess.audioUDP.Close()
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

// previewArgs builds ffmpeg arguments for low-latency H.264 + Opus RTP output.
// Video is scaled to 640x360; audio is only used by the browser-side meter.
func previewArgs(config ffmpeg.Config, videoRTPPort, audioRTPPort int) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
	}
	inputs := previewInputs(config)
	args = append(args, inputs.args...)

	// Use the same encoder as the main stream to avoid CPU-heavy
	// software re-encode when hardware encoding is available.
	encoder := config.EffectiveEncoder()
	previewScale := "scale=640:360:force_original_aspect_ratio=decrease"
	args = append(args,
		"-map", inputs.videoMap,
		"-an",
		"-vf", previewScale,
		"-r", "15",
	)
	switch encoder {
	case ffmpeg.EncoderVideoToolbox:
		args = append(args,
			"-c:v", "h264_videotoolbox",
			"-profile:v", "baseline", "-level:v", "3.1",
			"-allow_sw", "1", "-realtime", "1",
			"-pix_fmt", "yuv420p", "-bf", "0",
		)
	case ffmpeg.EncoderNVENC:
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "p1", "-profile:v", "baseline", "-level:v", "3.1",
			"-rc", "cbr", "-bf", "0", "-pix_fmt", "yuv420p",
		)
	default:
		args = append(args,
			"-c:v", "libx264",
			"-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "baseline", "-level:v", "3.1",
			"-pix_fmt", "yuv420p", "-bf", "0",
		)
	}
	args = append(args,
		"-g", "30",
		"-keyint_min", "15",
		"-b:v", "800k",
		"-flush_packets", "1",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-payload_type", "96",
		"-f", "rtp",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", videoRTPPort),
		"-map", inputs.audioMap,
		"-vn",
		"-c:a", "libopus",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", "64k",
		"-flush_packets", "1",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-payload_type", "111",
		"-f", "rtp",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", audioRTPPort),
	)
	return args
}

type previewInputBuild struct {
	args     []string
	videoMap string
	audioMap string
}

const previewSilentAudio = "anullsrc=channel_layout=stereo:sample_rate=48000"

func previewInputs(config ffmpeg.Config) previewInputBuild {
	switch config.Input.Kind {
	case ffmpeg.InputTestVideo:
		return previewInputBuild{
			args: []string{
				"-re",
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15",
				"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
			},
			videoMap: "0:v", audioMap: "1:a",
		}
	default:
		backend := config.Input.Backend
		if backend == "" {
			backend = ffmpeg.PlatformBackend()
		}
		device := config.Input.VideoDevice
		switch backend {
		case "avfoundation":
			device = ffmpeg.ResolveAVFoundationDeviceIndex(config.Binary, device, config.Input.VideoDeviceName, "video")
			audioDevice := ffmpeg.ResolveAVFoundationDeviceIndex(config.Binary, config.Input.AudioDevice, config.Input.AudioDeviceName, "audio")
			fps := ffmpeg.ProbeAVFoundationFramerate(config.Binary, device, config.Preset.FPS)
			if config.Input.AudioDevice != "" {
				device = device + ":" + audioDevice
				return previewInputBuild{
					args:     []string{"-f", "avfoundation", "-framerate", fps, "-i", device},
					videoMap: "0:v", audioMap: "0:a",
				}
			} else {
				device = device + ":none"
			}
			return previewInputBuild{
				args: []string{
					"-f", "avfoundation", "-framerate", fps, "-i", device,
					"-f", "lavfi", "-i", previewSilentAudio,
				},
				videoMap: "0:v", audioMap: "1:a",
			}
		case "dshow":
			device = "video=" + device
			if config.Input.AudioDevice != "" {
				return previewInputBuild{
					args:     []string{"-f", "dshow", "-i", device + ":audio=" + config.Input.AudioDevice},
					videoMap: "0:v", audioMap: "0:a",
				}
			}
			return previewInputBuild{
				args: []string{
					"-f", "dshow", "-i", device,
					"-f", "lavfi", "-i", previewSilentAudio,
				},
				videoMap: "0:v", audioMap: "1:a",
			}
		case "v4l2":
			if config.Input.AudioDevice != "" {
				return previewInputBuild{
					args: []string{
						"-f", "v4l2", "-i", device,
						"-f", "alsa", "-i", config.Input.AudioDevice,
					},
					videoMap: "0:v", audioMap: "1:a",
				}
			}
			return previewInputBuild{
				args: []string{
					"-f", "v4l2", "-i", device,
					"-f", "lavfi", "-i", previewSilentAudio,
				},
				videoMap: "0:v", audioMap: "1:a",
			}
		case "decklink":
			return previewInputBuild{
				args:     []string{"-f", "decklink", "-audio_input", "embedded", "-i", device},
				videoMap: "0:v", audioMap: "0:a",
			}
		default:
			return previewInputBuild{
				args: []string{
					"-f", backend, "-i", device,
					"-f", "lavfi", "-i", previewSilentAudio,
				},
				videoMap: "0:v", audioMap: "1:a",
			}
		}
	}
}
