package app

import (
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
	"github.com/ssimpson89/easystream/internal/quality"
	"github.com/ssimpson89/easystream/internal/youtube"
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
		a.server.mu.Unlock()
		if a.server.preview != nil {
			a.server.preview.Unblock()
		}
		return err
	}
	return nil
}

func (a *streamControllerAdapter) StopStream() {
	a.server.supervisor.Stop()
	if a.server.preview != nil {
		a.server.preview.Unblock()
	}
	// Caller (scheduler) already calls TransitionBroadcast itself; just clear local state.
	a.server.mu.Lock()
	a.server.activeBroadcastID = ""
	a.server.activeStreamID = ""
	a.server.streamHealth = streamHealthSnapshot{}
	a.server.mu.Unlock()
}

func (a *streamControllerAdapter) IsStreaming() bool {
	status := a.server.supervisor.Status()
	return status.State == ffmpeg.StateRunning || status.State == ffmpeg.StateDegraded || status.State == ffmpeg.StateStarting
}

// ytControllerAdapter implements schedule.YouTubeController on top of the
// YouTube API client + Auth.
type ytControllerAdapter struct {
	client *youtube.Client
	auth   *youtube.Auth
}

func (a *ytControllerAdapter) IsAuthenticated() bool {
	return a.auth.IsAuthenticated()
}

func (a *ytControllerAdapter) CreateBroadcast(title, description string, scheduledStart time.Time, privacy string) (string, error) {
	b, err := a.client.CreateBroadcast(title, description, scheduledStart, privacy)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func (a *ytControllerAdapter) EnsureStream(presetID string) (streamID, ingestURL, streamKey string, err error) {
	preset, ok := quality.ByID(presetID)
	if !ok {
		preset = quality.Default()
	}
	title := "EasyStream - " + preset.Name
	stream, err := a.client.EnsureStream(title, preset.Resolution(), preset.FPS)
	if err != nil {
		return "", "", "", err
	}
	return stream.ID, stream.IngestURL, stream.StreamKey, nil
}

func (a *ytControllerAdapter) BindBroadcast(broadcastID, streamID string) error {
	return a.client.BindBroadcast(broadcastID, streamID)
}

func (a *ytControllerAdapter) TransitionBroadcast(broadcastID, status string) error {
	return a.client.TransitionBroadcast(broadcastID, status)
}
