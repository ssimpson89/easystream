package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
	"github.com/ssimpson89/easystream/internal/quality"
	"github.com/ssimpson89/easystream/internal/version"
)

// --- Status ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusSnapshot())
}

// statusSnapshot builds the full state object returned by /api/status and
// pushed over SSE. Single source of truth so the polled and pushed views
// stay identical.
func (s *Server) statusSnapshot() map[string]any {
	s.mu.Lock()
	config := s.config
	health := s.streamHealth
	broadcastID := s.activeBroadcastID
	s.mu.Unlock()

	streamStatus := s.supervisor.Status()
	confidence := computeConfidence(streamStatus, health, broadcastID)

	result := map[string]any{
		"stream":            streamStatus,
		"app":               version.Current(),
		"config":            s.configResponse(config),
		"presets":           quality.Selectable(),
		"platform":          ffmpeg.PlatformBackend(),
		"capabilities":      s.ffmpegCaps,
		"health":            health,
		"confidence":        confidence,
		"activeBroadcastId": broadcastID,
	}
	if s.adaptive != nil {
		result["adaptive"] = s.adaptive.State()
	}
	if s.ytAuth != nil {
		result["youtube"] = s.ytAuth.AuthStatus()
	}
	if s.scheduler != nil {
		result["scheduler"] = s.scheduler.Status()
	}
	if s.schedStore != nil {
		result["nextEvents"] = s.schedStore.NextEvents(5, time.Now().UTC())
	}
	return result
}

// --- Config ---

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, quality.Selectable())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.configResponse(config))
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		FFmpegBinary *string            `json:"ffmpegBinary"`
		PresetID     *string            `json:"presetId"`
		Encoder      *ffmpeg.Encoder    `json:"encoder"`
		OutputMode   *ffmpeg.OutputMode `json:"outputMode"`
		IngestURL    *string            `json:"ingestUrl"`
		StreamName   *string            `json:"streamName"`
		EnableHLS    *bool              `json:"enableHls"`
		Input        *ffmpeg.Input      `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	if patch.FFmpegBinary != nil && *patch.FFmpegBinary != "" {
		s.config.Binary = *patch.FFmpegBinary
	}
	if patch.PresetID != nil && *patch.PresetID != "" {
		preset, ok := quality.ByID(*patch.PresetID)
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "unknown quality preset")
			return
		}
		s.config.Preset = preset
	}
	if patch.OutputMode != nil {
		// Migrate legacy "hls" payloads from older UIs to the new shape.
		if *patch.OutputMode == "hls" {
			s.config.OutputMode = ffmpeg.OutputRTMP
			s.config.EnableHLS = true
		} else if *patch.OutputMode == ffmpeg.OutputSRT && !s.ffmpegCaps.SRT {
			// Don't let the operator silently set SRT when FFmpeg
			// can't actually push it — they'd find out at go-live.
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest,
				"this FFmpeg build does not support SRT — see README for install instructions.")
			return
		} else {
			s.config.OutputMode = *patch.OutputMode
		}
	}
	if patch.EnableHLS != nil {
		s.config.EnableHLS = *patch.EnableHLS
	}
	if patch.IngestURL != nil {
		s.config.IngestURL = strings.TrimSpace(*patch.IngestURL)
	}
	if patch.StreamName != nil {
		s.config.StreamName = strings.TrimSpace(*patch.StreamName)
	}
	if patch.Input != nil {
		// Reject saves that drop the device name for backends where
		// indexes shift between boots. The persisted name is what makes
		// "go live on the right source" robust; saving without it is
		// the silent failure mode that caused Sunday's wrong-source bug.
		in := *patch.Input
		needsName := in.Kind != ffmpeg.InputTestVideo && in.Kind != ""
		platformBackend := in.Backend
		if platformBackend == "" {
			platformBackend = ffmpeg.PlatformBackend()
		}
		stableNeeded := platformBackend == "avfoundation" || platformBackend == "dshow" || platformBackend == "v4l2"
		if needsName && stableNeeded && in.VideoDevice != "" && in.VideoDeviceName == "" {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest,
				"video device name is required (capture device unplugged when this save was made? refresh devices and try again)")
			return
		}
		s.config.Input = in
	}
	if patch.Encoder != nil {
		s.config.Encoder = *patch.Encoder
	}

	s.preview.UpdateConfig(s.config)
	config := s.config
	s.mu.Unlock()
	if s.configPath != "" {
		if err := savePersistedConfig(s.configPath, config); err != nil {
			s.logger.Printf("failed to persist stream config: %v", err)
		}
	}
	// Push to every open UI so a source/preset change in tab A propagates
	// to tab B immediately. The preview reconnects client-side once the
	// status event lands.
	s.publishState()
	writeJSON(w, http.StatusOK, s.configResponse(config))
}

// --- Start / Stop ---

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	// Clean old HLS segments before starting a new stream.
	if config.EnableHLS && s.hlsServer != nil {
		_ = s.hlsServer.Clean()
	}

	// Release the capture device from the preview before the main stream
	// claims it. On macOS, only one process can hold a camera at a time.
	if s.preview != nil && config.Input.Kind != ffmpeg.InputTestVideo {
		s.preview.Block()
	}

	if err := s.supervisor.Start(config); err != nil {
		// Unblock so the preview can resume if start failed.
		if s.preview != nil {
			s.preview.Unblock()
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.adaptive != nil {
		s.adaptive.OnStreamStart(config.Preset.ID)
	}
	s.resetDestinationBadCount()
	s.markLive("manual", "", "")
	s.publishState()
	writeJSON(w, http.StatusAccepted, s.supervisor.Status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	// Capture the active broadcast ID before anything clears it.
	// scheduler.StopActive → adapter.StopStream clears activeBroadcastID,
	// so we must read it first to transition the YouTube broadcast.
	s.mu.Lock()
	broadcastID := s.activeBroadcastID
	s.activeBroadcastID = ""
	s.activeStreamID = ""
	s.streamHealth = streamHealthSnapshot{}
	s.destinationBad = 0
	s.mu.Unlock()

	if s.scheduler != nil {
		s.scheduler.StopActive()
	}
	s.cancelTransitionGoroutine()
	s.supervisor.Stop()
	if s.adaptive != nil {
		s.adaptive.OnStreamStop()
	}
	if s.preview != nil {
		s.preview.Unblock()
	}
	s.markIdle()

	// Transition the YouTube broadcast to "complete" so viewers see
	// "stream ended" instead of spinning indefinitely.
	if broadcastID != "" && s.ytClient != nil && s.ytAuth.IsAuthenticated() {
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.ytClient.TransitionBroadcast(ctx, id, "complete"); err != nil {
				s.logger.Printf("stop: complete broadcast %s: %v", id, err)
			} else {
				s.logger.Printf("stop: broadcast %s transitioned to complete", id)
			}
		}(broadcastID)
	}
	s.publishState()
	writeJSON(w, http.StatusOK, s.supervisor.Status())
}

// --- Extend / Adaptive ---

// handleExtend bumps the auto-stop time of the currently-active scheduled event
// by N minutes (default 15). For services that run long.
func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusBadRequest, "scheduler not available")
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // tolerate empty body — defaults to 15
	endsAt, err := s.scheduler.Extend(body.Minutes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.publishState()
	writeJSON(w, http.StatusOK, map[string]any{"endsAt": endsAt})
}

func (s *Server) handleAdaptiveToggle(w http.ResponseWriter, r *http.Request) {
	if s.adaptive == nil {
		writeError(w, http.StatusBadRequest, "adaptive controller not available")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.adaptive.SetEnabled(body.Enabled)
	s.publishState()
	writeJSON(w, http.StatusOK, s.adaptive.State())
}

// --- Devices ---

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		s.devScanner.Invalidate()
	}
	writeJSON(w, http.StatusOK, s.devScanner.Scan())
}

func (s *Server) handleEncoders(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	binary := s.config.Binary
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, ffmpeg.DetectEncoders(binary))
}

// --- Config response helper ---

func (s *Server) configResponse(config ffmpeg.Config) map[string]any {
	outputMode := string(config.OutputMode)
	if outputMode == "" || outputMode == "hls" {
		// Normalise away the legacy "hls" value so clients only ever
		// see the new shape (outputMode is the primary; enableHls is a
		// separate boolean).
		outputMode = "rtmp"
	}
	result := map[string]any{
		"ffmpegBinary": config.Binary,
		"input":        config.Input,
		"preset":       config.Preset,
		"encoder":      string(config.EffectiveEncoder()),
		"outputMode":   outputMode,
		"ingestUrl":    config.IngestURL,
		"hasStreamKey": config.StreamName != "",
		"enableHls":    config.EnableHLS,
		"network":      config.Network,
	}
	if s.hlsServer != nil {
		result["hlsUrl"] = "http://" + s.addr + "/hls/stream.m3u8"
	}
	return result
}
