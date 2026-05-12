package app

import (
	"context"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
	"github.com/ssimpson89/easystream/internal/quality"
)

// streamControllerAdapter implements schedule.StreamController on top of
// the Server's supervisor + preview + state. Lets the scheduler drive
// FFmpeg start/stop without depending on the app package directly.
type streamControllerAdapter struct {
	server *Server
}

func (a *streamControllerAdapter) StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error {
	a.server.mu.Lock()
	if presetID != "" {
		if preset, ok := quality.ByID(presetID); ok {
			a.server.config.Preset = preset
		}
	}
	a.server.config.IngestURL = ingestURL
	a.server.config.StreamName = streamKey
	a.server.activeBroadcastID = broadcastID
	a.server.activeStreamID = streamID
	config := a.server.config
	a.server.mu.Unlock()
	if a.server.preview != nil && config.Input.Kind != ffmpeg.InputTestVideo {
		a.server.preview.Block()
	}
	if err := a.server.supervisor.Start(config); err != nil {
		a.server.mu.Lock()
		a.server.activeBroadcastID = ""
		a.server.activeStreamID = ""
		a.server.destinationBad = 0
		a.server.mu.Unlock()
		if a.server.preview != nil {
			a.server.preview.Unblock()
		}
		return err
	}
	a.server.resetDestinationBadCount()
	a.server.markLive("scheduled", broadcastID, streamID)
	if a.server.adaptive != nil {
		a.server.adaptive.OnStreamStart(config.Preset.ID)
	}
	a.server.publishState()
	return nil
}

func (a *streamControllerAdapter) StopStream() {
	a.server.supervisor.Stop()
	if a.server.preview != nil {
		a.server.preview.Unblock()
	}
	if a.server.adaptive != nil {
		a.server.adaptive.OnStreamStop()
	}
	a.server.markIdle()
	// CompleteBroadcast (called by the scheduler) handles the YouTube
	// side of the lifecycle; just clear local state here.
	a.server.mu.Lock()
	a.server.activeBroadcastID = ""
	a.server.activeStreamID = ""
	a.server.streamHealth = streamHealthSnapshot{}
	a.server.destinationBad = 0
	a.server.mu.Unlock()
	a.server.publishState()
}

func (a *streamControllerAdapter) IsStreaming() bool {
	status := a.server.supervisor.Status()
	return status.State == ffmpeg.StateRunning || status.State == ffmpeg.StateDegraded || status.State == ffmpeg.StateStarting
}

// broadcastControllerAdapter implements schedule.BroadcastController on
// top of the Server's YouTube client + Auth + transition state. The
// scheduler delegates all YouTube lifecycle work here so it never has to
// touch HTTP or OAuth directly.
type broadcastControllerAdapter struct {
	server *Server
}

func (a *broadcastControllerAdapter) IsAuthenticated() bool {
	return a.server.ytAuth != nil && a.server.ytAuth.IsAuthenticated()
}

func (a *broadcastControllerAdapter) CreateBroadcast(ctx context.Context, title, description string, scheduledStart time.Time, privacy string) (string, error) {
	b, err := a.server.ytClient.CreateBroadcast(title, description, scheduledStart, privacy)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func (a *broadcastControllerAdapter) CreateBoundStream(ctx context.Context, broadcastID, presetID string) (string, string, string, error) {
	preset, ok := quality.ByID(presetID)
	if !ok {
		preset = quality.Default()
	}
	title := "EasyStream - " + preset.Name + " - " + time.Now().UTC().Format("20060102-150405")
	stream, err := a.server.ytClient.CreateStreamForBroadcast(title, preset.Resolution(), preset.FPS)
	if err != nil {
		return "", "", "", err
	}
	if err := a.server.ytClient.BindBroadcast(broadcastID, stream.ID); err != nil {
		// Best-effort cleanup of the orphan stream so we don't leak it
		// on a transient bind failure.
		_ = a.server.ytClient.DeleteStream(stream.ID)
		return "", "", "", err
	}
	return stream.ID, stream.IngestURL, stream.StreamKey, nil
}

func (a *broadcastControllerAdapter) StartTransitionToLive(broadcastID, streamID string) {
	a.server.startTransitionGoroutine(broadcastID, streamID)
}

func (a *broadcastControllerAdapter) CancelTransition() {
	a.server.cancelTransitionGoroutine()
}

func (a *broadcastControllerAdapter) CompleteBroadcast(broadcastID, streamID string) {
	if broadcastID == "" {
		return
	}
	if err := a.server.ytClient.TransitionBroadcast(broadcastID, "complete"); err != nil {
		a.server.logger.Printf("complete broadcast %s: %v", broadcastID, err)
	}
	if streamID != "" {
		if err := a.server.ytClient.DeleteStream(streamID); err != nil {
			a.server.logger.Printf("delete stream %s: %v", streamID, err)
		}
	}
}
