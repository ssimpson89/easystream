package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// --- YouTube auth handlers ---

func (s *Server) handleYTAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, s.ytAuth.AuthStatus())
}

func (s *Server) handleYTAuthURL(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		writeError(w, http.StatusBadRequest, "YouTube credentials not configured. Set YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET environment variables.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.ytAuth.AuthURL()})
}

func (s *Server) handleYTAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		http.Error(w, "YouTube not configured", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body><h2>YouTube login cancelled</h2><p>` + errParam + `</p><p>You can close this tab.</p><script>window.close()</script></body></html>`))
		return
	}
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}
	if err := s.ytAuth.Exchange(code, state); err != nil {
		http.Error(w, "Authentication failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<html><body><h2>YouTube connected!</h2><p>You can close this tab and return to EasyStream.</p><script>window.close()</script></body></html>`))
}

func (s *Server) handleYTLogout(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth != nil {
		_ = s.ytAuth.Logout()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// --- YouTube broadcast handlers ---

func (s *Server) handleYTBroadcasts(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	broadcasts, err := s.ytClient.ListUpcomingBroadcasts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, broadcasts)
}

func (s *Server) handleGoLiveNow(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeError(w, http.StatusBadRequest, "not authenticated with YouTube")
		return
	}

	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Privacy     string `json:"privacy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Title == "" {
		body.Title = "Live Stream"
	}
	if body.Privacy == "" {
		body.Privacy = "unlisted"
	}

	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	// Create broadcast.
	broadcast, err := s.ytClient.CreateBroadcast(body.Title, body.Description, time.Now(), body.Privacy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create broadcast: "+err.Error())
		return
	}

	// Ensure stream.
	streamTitle := "EasyStream - " + config.Preset.Name
	stream, err := s.ytClient.EnsureStream(streamTitle, config.Preset.Resolution(), config.Preset.FPS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create stream: "+err.Error())
		return
	}

	// Bind.
	if err := s.ytClient.BindBroadcast(broadcast.ID, stream.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "bind broadcast: "+err.Error())
		return
	}

	// Configure FFmpeg with stream ingest details and start.
	s.mu.Lock()
	s.config.IngestURL = stream.IngestURL
	s.config.StreamName = stream.StreamKey
	s.activeBroadcastID = broadcast.ID
	s.activeStreamID = stream.ID
	startConfig := s.config
	s.mu.Unlock()

	if s.preview != nil && startConfig.Input.Kind != ffmpeg.InputTestVideo {
		s.preview.Block()
	}
	if err := s.supervisor.Start(startConfig); err != nil {
		s.mu.Lock()
		s.activeBroadcastID = ""
		s.activeStreamID = ""
		s.mu.Unlock()
		if s.preview != nil {
			s.preview.Unblock()
		}
		writeError(w, http.StatusBadRequest, "start ffmpeg: "+err.Error())
		return
	}
	s.markLive("go-live-now", broadcast.ID, stream.ID)

	// Transition in background.
	go func() {
		adapter := &ytControllerAdapter{client: s.ytClient, auth: s.ytAuth}
		for i := 0; i < 30; i++ {
			time.Sleep(5 * time.Second)
			if err := adapter.TransitionBroadcast(broadcast.ID, "testing"); err != nil {
				s.logger.Printf("go-live-now: transition to testing attempt %d: %v", i+1, err)
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s in testing", broadcast.ID)
			break
		}
		time.Sleep(10 * time.Second)
		for i := 0; i < 10; i++ {
			if err := adapter.TransitionBroadcast(broadcast.ID, "live"); err != nil {
				s.logger.Printf("go-live-now: transition to live attempt %d: %v", i+1, err)
				time.Sleep(5 * time.Second)
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s is LIVE", broadcast.ID)
			return
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"broadcast": broadcast,
		"stream":    stream,
	})
}

func (s *Server) handleCompleteBroadcast(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeError(w, http.StatusBadRequest, "not authenticated with YouTube")
		return
	}

	var body struct {
		BroadcastID string `json:"broadcastId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Stop FFmpeg first.
	s.supervisor.Stop()
	if s.preview != nil {
		s.preview.Unblock()
	}
	s.markIdle()

	if body.BroadcastID != "" {
		if err := s.ytClient.TransitionBroadcast(body.BroadcastID, "complete"); err != nil {
			s.logger.Printf("complete broadcast: %v", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}
