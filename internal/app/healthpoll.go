package app

import "time"

// runHealthPoller polls YouTube for the bound stream's health every 15s
// while we have an active broadcast. Updates s.streamHealth so the UI
// confidence indicators reflect what YouTube actually sees.
func (s *Server) runHealthPoller() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.pollStreamHealth()
	}
}

func (s *Server) pollStreamHealth() {
	s.mu.Lock()
	streamID := s.activeStreamID
	hasBroadcast := s.activeBroadcastID != ""
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
	s.mu.Lock()
	s.streamHealth = streamHealthSnapshot{
		StreamStatus: health.StreamStatus,
		HealthStatus: health.HealthStatus,
		Issues:       health.Issues,
		LastUpdate:   time.Now().UTC(),
		Source:       "youtube",
		HasBroadcast: true,
	}
	s.mu.Unlock()
}
