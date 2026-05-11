package app

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"sync"
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
		_, _ = w.Write([]byte(`<html><body><h2>YouTube login cancelled</h2><p>` + html.EscapeString(errParam) + `</p><p>You can close this tab.</p><script>window.close()</script></body></html>`))
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

	// If a previous broadcast is still active, complete it before starting
	// a new one. This prevents orphaned YouTube broadcasts that stay "live"
	// even though nothing is streaming to them.
	s.mu.Lock()
	prevBroadcast := s.activeBroadcastID
	config := s.config
	s.mu.Unlock()
	if prevBroadcast != "" {
		s.logger.Printf("go-live-now: completing previous broadcast %s before starting new one", prevBroadcast)
		// Stop the existing stream first.
		s.supervisor.Stop()
		if s.preview != nil {
			s.preview.Unblock()
		}
		s.cancelTransitionGoroutine()
		// Transition old broadcast to complete.
		if err := s.ytClient.TransitionBroadcast(prevBroadcast, "complete"); err != nil {
			s.logger.Printf("go-live-now: complete previous broadcast %s: %v", prevBroadcast, err)
		}
		s.mu.Lock()
		s.activeBroadcastID = ""
		s.activeStreamID = ""
		s.streamHealth = streamHealthSnapshot{}
		s.destinationBad = 0
		s.mu.Unlock()
		s.markIdle()
	}

	// Create broadcast.
	broadcast, err := s.ytClient.CreateBroadcast(body.Title, body.Description, time.Now(), body.Privacy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create broadcast: "+err.Error())
		return
	}

	// Create a fresh, non-reusable stream endpoint for this broadcast.
	// Reusing YouTube's named stream can leave multiple active broadcasts
	// bound to the same ingest, causing the wrong watch page.
	streamTitle := "EasyStream - " + body.Title + " - " + broadcast.ID
	stream, err := s.ytClient.CreateStreamForBroadcast(streamTitle, config.Preset.Resolution(), config.Preset.FPS)
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
		s.destinationBad = 0
		s.mu.Unlock()
		if s.preview != nil {
			s.preview.Unblock()
		}
		writeError(w, http.StatusBadRequest, "start ffmpeg: "+err.Error())
		return
	}
	s.resetDestinationBadCount()
	s.markLive("go-live-now", broadcast.ID, stream.ID)

	// Transition in background with cancellation support. The goroutine
	// checks that the broadcast is still the active one before each attempt
	// and exits if the context is cancelled (e.g., user stops or starts a
	// new broadcast).
	s.startTransitionGoroutine(broadcast.ID)

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

	// Only stop FFmpeg and clear state if completing the *currently active*
	// broadcast. Completing an old/different broadcast should not kill the
	// current stream.
	s.mu.Lock()
	isActiveBroadcast := body.BroadcastID != "" && body.BroadcastID == s.activeBroadcastID
	s.mu.Unlock()

	if isActiveBroadcast {
		s.cancelTransitionGoroutine()
		s.supervisor.Stop()
		if s.preview != nil {
			s.preview.Unblock()
		}
		s.markIdle()
		s.mu.Lock()
		s.activeBroadcastID = ""
		s.activeStreamID = ""
		s.streamHealth = streamHealthSnapshot{}
		s.destinationBad = 0
		s.mu.Unlock()
	}

	if body.BroadcastID != "" {
		if err := s.ytClient.TransitionBroadcast(body.BroadcastID, "complete"); err != nil {
			s.logger.Printf("complete broadcast: %v", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}

// --- Transition goroutine management ---
//
// The YouTube broadcast transition (testing → live) runs as a background
// goroutine. We track it so it can be cancelled when the user stops or
// starts a new broadcast — preventing zombie goroutines that keep retrying
// transitions for a broadcast that's no longer active.

var (
	transitionMu     sync.Mutex
	transitionCancel context.CancelFunc
)

// cancelTransitionGoroutine cancels any in-flight YouTube transition goroutine.
func (s *Server) cancelTransitionGoroutine() {
	transitionMu.Lock()
	if transitionCancel != nil {
		transitionCancel()
		transitionCancel = nil
	}
	transitionMu.Unlock()
}

// startTransitionGoroutine launches the testing → live transition for the
// given broadcast. Any previous transition goroutine is cancelled first.
func (s *Server) startTransitionGoroutine(broadcastID string) {
	s.cancelTransitionGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	transitionMu.Lock()
	transitionCancel = cancel
	transitionMu.Unlock()

	go func() {
		defer cancel()
		adapter := &ytControllerAdapter{client: s.ytClient, auth: s.ytAuth}

		// Phase 1: transition to "testing" — YouTube needs to ingest for a
		// few seconds before accepting this.
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				s.logger.Printf("go-live-now: transition cancelled for broadcast %s", broadcastID)
				return
			case <-time.After(5 * time.Second):
			}
			// Abort if this broadcast is no longer the active one.
			s.mu.Lock()
			stillActive := s.activeBroadcastID == broadcastID
			s.mu.Unlock()
			if !stillActive {
				s.logger.Printf("go-live-now: broadcast %s no longer active, stopping transition", broadcastID)
				return
			}
			if err := adapter.TransitionBroadcast(broadcastID, "testing"); err != nil {
				s.logger.Printf("go-live-now: transition to testing attempt %d: %v", i+1, err)
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s in testing", broadcastID)
			break
		}

		// Brief pause before transitioning to live.
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}

		// Phase 2: transition to "live".
		for i := 0; i < 10; i++ {
			s.mu.Lock()
			stillActive := s.activeBroadcastID == broadcastID
			s.mu.Unlock()
			if !stillActive {
				s.logger.Printf("go-live-now: broadcast %s no longer active, stopping transition", broadcastID)
				return
			}
			if err := adapter.TransitionBroadcast(broadcastID, "live"); err != nil {
				s.logger.Printf("go-live-now: transition to live attempt %d: %v", i+1, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s is LIVE", broadcastID)
			return
		}
		s.logger.Printf("go-live-now: gave up transitioning broadcast %s to live after all retries", broadcastID)
	}()
}
