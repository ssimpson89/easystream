package app

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
	"github.com/ssimpson89/easystream/internal/youtube"
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
	s.publishState()
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// --- YouTube broadcast handlers ---

func (s *Server) handleYTBroadcasts(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	broadcasts, err := s.ytClient.ListUpcomingBroadcasts(r.Context())
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
		// Transition old broadcast to complete (bounded so a hung YT API
		// can't block the new go-live indefinitely).
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		if err := s.ytClient.TransitionBroadcast(ctx, prevBroadcast, "complete"); err != nil {
			s.logger.Printf("go-live-now: complete previous broadcast %s: %v", prevBroadcast, err)
		}
		cancel()
		s.mu.Lock()
		s.activeBroadcastID = ""
		s.activeStreamID = ""
		s.streamHealth = streamHealthSnapshot{}
		s.destinationBad = 0
		s.mu.Unlock()
		s.markIdle()
	}

	// Create broadcast + stream + bind, all tied to the request context so
	// a client-side cancel or server shutdown unblocks the YouTube calls.
	broadcast, err := s.ytClient.CreateBroadcast(r.Context(), body.Title, body.Description, time.Now(), body.Privacy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create broadcast: "+err.Error())
		return
	}

	// Create a fresh, non-reusable stream endpoint for this broadcast.
	// Reusing YouTube's named stream can leave multiple active broadcasts
	// bound to the same ingest, causing the wrong watch page.
	streamTitle := "EasyStream - " + body.Title + " - " + broadcast.ID
	stream, err := s.ytClient.CreateStreamForBroadcast(r.Context(), streamTitle, config.Preset.Resolution(), config.Preset.FPS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create stream: "+err.Error())
		return
	}

	// Bind.
	if err := s.ytClient.BindBroadcast(r.Context(), broadcast.ID, stream.ID); err != nil {
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
	// gates testing→live on stream-side health and exits cleanly when
	// cancelled (user stops or starts a new broadcast).
	s.startTransitionGoroutine(broadcast.ID, stream.ID)

	s.publishState()
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
	streamToDelete := ""
	if isActiveBroadcast {
		streamToDelete = s.activeStreamID
	}
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
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		if err := s.ytClient.TransitionBroadcast(ctx, body.BroadcastID, "complete"); err != nil {
			s.logger.Printf("complete broadcast: %v", err)
		}
		cancel()
	}
	// Clean up the per-broadcast non-reusable stream so it doesn't
	// accumulate on the channel. Failure is non-fatal.
	if streamToDelete != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		if err := s.ytClient.DeleteStream(ctx, streamToDelete); err != nil {
			s.logger.Printf("complete broadcast: delete stream %s: %v", streamToDelete, err)
		}
		cancel()
	}

	s.publishState()
	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}

// --- Transition goroutine management ---
//
// The YouTube broadcast transition (testing → live) runs as a background
// goroutine on the Server. It is tracked so it can be cancelled when the
// user stops or starts a different broadcast, preventing zombie goroutines
// that keep retrying transitions on a broadcast that is no longer active.

// cancelTransitionGoroutine cancels any in-flight YouTube transition.
func (s *Server) cancelTransitionGoroutine() {
	s.transitionMu.Lock()
	if s.transitionCancel != nil {
		s.transitionCancel()
		s.transitionCancel = nil
	}
	s.transitionMu.Unlock()
}

// startTransitionGoroutine launches testing→live for the given broadcast,
// gating each step on stream-side signals from YouTube. Any previous
// transition is cancelled first.
//
// Flow:
//  1. Poll liveStreams.status until streamStatus=="active" (YouTube
//     requires ingest before accepting testing transition).
//  2. Transition to "testing". redundantTransition is treated as success.
//  3. Transition to "live". Same handling.
//
// All steps respect ctx; cancellation stops the goroutine immediately.
func (s *Server) startTransitionGoroutine(broadcastID, streamID string) {
	s.cancelTransitionGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	s.transitionMu.Lock()
	s.transitionCancel = cancel
	s.transitionMu.Unlock()

	go func() {
		defer cancel()
		s.runTransitionToLive(ctx, broadcastID, streamID)
	}()
}

// runTransitionToLive is the shared implementation used by both Go-Live-Now
// and Scheduled paths. Returns when the broadcast reaches "live", ctx is
// cancelled, or YouTube rejects with a terminal reason.
func (s *Server) runTransitionToLive(ctx context.Context, broadcastID, streamID string) {
	stillActive := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.activeBroadcastID == broadcastID
	}

	// Phase 1: wait for stream-side "active" signal. YouTube refuses
	// testing transitions until ingest has been flowing for ~15-30s.
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	if streamID != "" {
		if err := s.ytClient.WaitStreamActive(waitCtx, streamID, 3*time.Second); err != nil {
			waitCancel()
			if ctx.Err() != nil {
				s.logger.Printf("transition: cancelled before stream went active (broadcast=%s)", broadcastID)
				return
			}
			s.logger.Printf("transition: stream did not go active in 2min (broadcast=%s): %v", broadcastID, err)
			// Fall through and try the transition anyway — sometimes
			// liveStreams.status lags but the transition still works.
		} else {
			s.logger.Printf("transition: stream %s active, transitioning broadcast %s → testing", streamID, broadcastID)
		}
	}
	waitCancel()

	if !stillActive() || ctx.Err() != nil {
		return
	}

	// Phase 2: testing. Up to 3 retries on transient errors.
	if err := s.transitionWithRetry(ctx, broadcastID, "testing", 3, stillActive); err != nil {
		s.logger.Printf("transition: %s → testing failed: %v", broadcastID, err)
		return
	}

	// Brief pause to let YouTube settle in testing before the live transition.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}

	// Phase 3: live. Up to 5 retries.
	if err := s.transitionWithRetry(ctx, broadcastID, "live", 5, stillActive); err != nil {
		s.logger.Printf("transition: %s → live failed: %v", broadcastID, err)
		return
	}
	s.logger.Printf("transition: broadcast %s is LIVE", broadcastID)
}

// transitionWithRetry attempts the given transition with bounded retries.
// Terminal YouTube reasons (invalidTransition, liveStreamingNotEnabled)
// short-circuit. redundantTransition is success.
func (s *Server) transitionWithRetry(ctx context.Context, broadcastID, status string, maxAttempts int, stillActive func() bool) error {
	delay := 3 * time.Second
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if !stillActive() {
			return context.Canceled
		}
		err := s.ytClient.TransitionBroadcast(ctx, broadcastID, status)
		if err == nil {
			return nil
		}
		lastErr = err
		// Terminal reasons — don't retry.
		if youtube.IsReason(err, "invalidTransition") ||
			youtube.IsReason(err, "liveStreamingNotEnabled") ||
			youtube.IsReason(err, "forbidden") {
			return err
		}
		s.logger.Printf("transition: %s → %s attempt %d: %v", broadcastID, status, i+1, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay *= 2
		}
	}
	return lastErr
}
