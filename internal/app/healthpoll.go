package app

import (
	"fmt"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

const (
	destinationRecoveryStartupGrace  = 60 * time.Second
	destinationBadPollsBeforeRestart = 3
)

// runHealthPoller polls YouTube for the bound stream's health every 15s
// while we have an active broadcast. Updates s.streamHealth so the UI
// confidence indicators reflect what YouTube actually sees.
func (s *Server) runHealthPoller() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.safePollStreamHealth()
	}
}

func (s *Server) safePollStreamHealth() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("health poller panic: %v", r)
		}
	}()
	s.pollStreamHealth()
}

func (s *Server) pollStreamHealth() {
	s.mu.Lock()
	streamID := s.activeStreamID
	broadcastID := s.activeBroadcastID
	hasBroadcast := broadcastID != ""
	s.mu.Unlock()

	// Only poll when we have a bound stream and YT auth.
	if streamID == "" || s.ytClient == nil || s.ytAuth == nil || !s.ytAuth.IsAuthenticated() {
		s.mu.Lock()
		s.streamHealth = streamHealthSnapshot{HasBroadcast: hasBroadcast}
		s.mu.Unlock()
		return
	}

	health, err := s.ytClient.GetStreamHealth(streamID)
	if err != nil {
		s.logger.Printf("health poll: %v", err)
		return
	}

	snap := streamHealthSnapshot{
		StreamStatus: health.StreamStatus,
		HealthStatus: health.HealthStatus,
		Issues:       health.Issues,
		LastUpdate:   time.Now().UTC(),
		Source:       "youtube",
		HasBroadcast: true,
	}

	// Fetch concurrent viewers if we have an active broadcast.
	if broadcastID != "" {
		if viewers, err := s.ytClient.GetConcurrentViewers(broadcastID); err != nil {
			s.logger.Printf("health poll: viewer count: %v", err)
		} else if viewers >= 0 {
			snap.ConcurrentViewers = &viewers
		}
	}

	s.mu.Lock()
	s.streamHealth = snap
	s.mu.Unlock()

	s.applyDestinationHealth(snap)
}

func (s *Server) applyDestinationHealth(snap streamHealthSnapshot) {
	status := s.supervisor.Status()
	if status.State != ffmpeg.StateRunning && status.State != ffmpeg.StateDegraded {
		s.resetDestinationBadCount()
		return
	}
	if !status.StartedAt.IsZero() && time.Since(status.StartedAt) < destinationRecoveryStartupGrace {
		return
	}

	bad, reason := destinationRestartReason(snap)
	if !bad {
		s.resetDestinationBadCount()
		return
	}

	s.mu.Lock()
	s.destinationBad++
	badCount := s.destinationBad
	s.mu.Unlock()

	if badCount < destinationBadPollsBeforeRestart {
		s.logger.Printf("health poll: destination unhealthy (%d/%d): %s", badCount, destinationBadPollsBeforeRestart, reason)
		return
	}

	msg := fmt.Sprintf("destination unhealthy: %s", reason)
	if s.supervisor.Restart(msg) {
		s.logger.Printf("health poll: restarting FFmpeg because %s", reason)
		s.resetDestinationBadCount()
	}
}

func (s *Server) resetDestinationBadCount() {
	s.mu.Lock()
	s.destinationBad = 0
	s.mu.Unlock()
}

func destinationRestartReason(snap streamHealthSnapshot) (bool, string) {
	switch snap.StreamStatus {
	case "inactive", "error":
		if snap.HealthStatus != "" {
			return true, fmt.Sprintf("YouTube streamStatus=%s healthStatus=%s", snap.StreamStatus, snap.HealthStatus)
		}
		return true, "YouTube streamStatus=" + snap.StreamStatus
	}
	if snap.HealthStatus == "noData" {
		if snap.StreamStatus != "" {
			return true, fmt.Sprintf("YouTube healthStatus=noData streamStatus=%s", snap.StreamStatus)
		}
		return true, "YouTube healthStatus=noData"
	}
	return false, ""
}
