package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://www.googleapis.com/youtube/v3"

// APIError captures the structured error returned by the YouTube Data API
// so callers can react to specific reason codes (redundantTransition,
// errorStreamInactive, invalidTransition, etc.) instead of pattern-matching
// error strings.
type APIError struct {
	StatusCode int
	Reason     string // first error.errors[].reason from the response
	Message    string // human-readable message from YouTube
	Body       string // truncated raw body for diagnostics
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("youtube api %d %s: %s", e.StatusCode, e.Reason, e.Message)
	}
	return fmt.Sprintf("youtube api %d: %s", e.StatusCode, truncate([]byte(e.Body), 200))
}

// IsReason reports whether err is an APIError with the given reason code.
func IsReason(err error, reason string) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Reason == reason
	}
	return false
}

// Client wraps authenticated YouTube Data API v3 calls.
type Client struct {
	Auth *Auth
}

// Broadcast represents a YouTube live broadcast.
type Broadcast struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	ScheduledStart time.Time `json:"scheduledStart"`
	ActualStart    time.Time `json:"actualStart,omitempty"`
	Status         string    `json:"status"` // created, ready, testing, live, complete, revoked
	StreamID       string    `json:"streamId,omitempty"`
}

// StreamHealth is the live health snapshot for a bound stream.
type StreamHealth struct {
	StreamStatus       string   `json:"streamStatus"` // active|created|error|inactive|ready
	HealthStatus       string   `json:"healthStatus"` // good|ok|bad|noData
	Issues             []string `json:"issues,omitempty"`
	LastUpdateTimeUnix int64    `json:"lastUpdateTime,omitempty"`
}

// Stream represents a YouTube live stream (RTMP endpoint).
type Stream struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	IngestURL string `json:"ingestUrl"`
	StreamKey string `json:"streamKey"`
	Status    string `json:"status"` // active, created, error, inactive, ready
}

// CreateBroadcast creates a scheduled YouTube live broadcast.
//
// enableAutoStart/enableAutoStop are intentionally false: we own the
// testing→live→complete lifecycle so a transient FFmpeg restart doesn't
// auto-end the broadcast and the explicit transition flow doesn't race
// with YouTube's auto-start. recordFromStart ensures a VOD is created.
func (c *Client) CreateBroadcast(ctx context.Context, title, description string, scheduledStart time.Time, privacy string) (*Broadcast, error) {
	if privacy == "" {
		privacy = "unlisted"
	}
	payload := map[string]any{
		"snippet": map[string]any{
			"title":              title,
			"description":        description,
			"scheduledStartTime": scheduledStart.UTC().Format(time.RFC3339),
		},
		"contentDetails": map[string]any{
			"enableAutoStart":   false,
			"enableAutoStop":    false,
			"enableDvr":         true,
			"recordFromStart":   true,
			"latencyPreference": "normal",
			"monitorStream": map[string]any{
				"enableMonitorStream": true,
			},
		},
		"status": map[string]any{
			"privacyStatus":           privacy,
			"selfDeclaredMadeForKids": false,
		},
	}
	body, err := c.post(ctx, "/liveBroadcasts?part=snippet,contentDetails,status", payload)
	if err != nil {
		return nil, fmt.Errorf("create broadcast: %w", err)
	}
	return parseBroadcast(body)
}

// ListUpcomingBroadcasts returns broadcasts with status upcoming or active.
func (c *Client) ListUpcomingBroadcasts(ctx context.Context) ([]Broadcast, error) {
	var all []Broadcast
	for _, status := range []string{"upcoming", "active"} {
		broadcasts, err := c.listBroadcasts(ctx, status)
		if err != nil {
			return nil, err
		}
		all = append(all, broadcasts...)
	}
	return all, nil
}

func (c *Client) listBroadcasts(ctx context.Context, broadcastStatus string) ([]Broadcast, error) {
	path := fmt.Sprintf("/liveBroadcasts?part=snippet,contentDetails,status&broadcastStatus=%s&maxResults=25", broadcastStatus)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("list broadcasts (%s): %w", broadcastStatus, err)
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var broadcasts []Broadcast
	for _, item := range resp.Items {
		b, err := parseBroadcast(item)
		if err != nil {
			continue
		}
		broadcasts = append(broadcasts, *b)
	}
	return broadcasts, nil
}

// CreateStreamForBroadcast creates a non-reusable live stream endpoint
// intended for a single broadcast. Non-reusable streams prevent multiple
// active broadcasts from sharing the same ingest (which causes YouTube to
// show the wrong watch page) and let us delete the stream cleanly when
// the broadcast completes.
func (c *Client) CreateStreamForBroadcast(ctx context.Context, title string, resolution string, fps int) (*Stream, error) {
	payload := map[string]any{
		"snippet": map[string]any{
			"title": title,
		},
		"cdn": map[string]any{
			"frameRate":     fpsCategory(fps),
			"ingestionType": "rtmp",
			"resolution":    resolutionCategory(resolution),
		},
		"contentDetails": map[string]any{
			"isReusable": false,
		},
	}
	body, err := c.post(ctx, "/liveStreams?part=snippet,cdn,contentDetails,status", payload)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}
	return parseStream(body)
}

// DeleteStream removes a live stream resource. Call this after a broadcast
// transitions to complete; otherwise the per-broadcast non-reusable
// streams accumulate on the channel and count against quota.
func (c *Client) DeleteStream(ctx context.Context, streamID string) error {
	path := fmt.Sprintf("/liveStreams?id=%s", url.QueryEscape(streamID))
	return c.delete(ctx, path)
}

// GetStreamHealth returns the platform-reported health for a stream.
// Use this while broadcasting to know whether YouTube is actually
// receiving and accepting the feed.
func (c *Client) GetStreamHealth(ctx context.Context, streamID string) (*StreamHealth, error) {
	path := fmt.Sprintf("/liveStreams?part=status&id=%s", url.QueryEscape(streamID))
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get stream health: %w", err)
	}
	var resp struct {
		Items []struct {
			Status struct {
				StreamStatus string `json:"streamStatus"`
				HealthStatus struct {
					Status              string `json:"status"`
					LastUpdateTimeSecs  int64  `json:"lastUpdateTimeSeconds,string"`
					ConfigurationIssues []struct {
						Type        string `json:"type"`
						Severity    string `json:"severity"`
						Reason      string `json:"reason"`
						Description string `json:"description"`
					} `json:"configurationIssues"`
				} `json:"healthStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Items) == 0 {
		return nil, fmt.Errorf("stream %s not found", streamID)
	}
	item := resp.Items[0]
	issues := make([]string, 0, len(item.Status.HealthStatus.ConfigurationIssues))
	for _, ci := range item.Status.HealthStatus.ConfigurationIssues {
		if ci.Description != "" {
			issues = append(issues, ci.Description)
		} else if ci.Reason != "" {
			issues = append(issues, ci.Reason)
		}
	}
	return &StreamHealth{
		StreamStatus:       item.Status.StreamStatus,
		HealthStatus:       item.Status.HealthStatus.Status,
		Issues:             issues,
		LastUpdateTimeUnix: item.Status.HealthStatus.LastUpdateTimeSecs,
	}, nil
}

// GetConcurrentViewers returns the current number of viewers for a live
// broadcast. Uses the videos.list endpoint with liveStreamingDetails part.
// Returns -1 if the field is not available (e.g. broadcast not yet live).
func (c *Client) GetConcurrentViewers(ctx context.Context, broadcastID string) (int, error) {
	path := fmt.Sprintf("/videos?part=liveStreamingDetails&id=%s", url.QueryEscape(broadcastID))
	body, err := c.get(ctx, path)
	if err != nil {
		return -1, fmt.Errorf("get concurrent viewers: %w", err)
	}
	var resp struct {
		Items []struct {
			LiveStreamingDetails struct {
				ConcurrentViewers string `json:"concurrentViewers"`
			} `json:"liveStreamingDetails"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return -1, err
	}
	if len(resp.Items) == 0 {
		return -1, nil
	}
	raw := resp.Items[0].LiveStreamingDetails.ConcurrentViewers
	if raw == "" {
		return -1, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return -1, nil
	}
	return n, nil
}

// BindBroadcast binds a broadcast to a stream.
func (c *Client) BindBroadcast(ctx context.Context, broadcastID, streamID string) error {
	path := fmt.Sprintf("/liveBroadcasts/bind?id=%s&streamId=%s&part=id",
		url.QueryEscape(broadcastID), url.QueryEscape(streamID))
	_, err := c.post(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("bind broadcast: %w", err)
	}
	return nil
}

// TransitionBroadcast changes the broadcast status (testing, live, complete).
//
// Treats redundantTransition (we're already in the target state) as success
// so retry loops exit cleanly when YouTube updates state independently.
func (c *Client) TransitionBroadcast(ctx context.Context, broadcastID, status string) error {
	path := fmt.Sprintf("/liveBroadcasts/transition?id=%s&broadcastStatus=%s&part=id,status",
		url.QueryEscape(broadcastID), url.QueryEscape(status))
	_, err := c.post(ctx, path, nil)
	if err != nil {
		if IsReason(err, "redundantTransition") {
			return nil
		}
		return fmt.Errorf("transition broadcast to %s: %w", status, err)
	}
	return nil
}

// WaitStreamActive polls liveStreams.status.streamStatus until it reaches
// "active" or ctx is cancelled. YouTube refuses transition→testing until
// the stream has been ingesting for ~15-30s; this poll surfaces that exact
// signal instead of blind retry. Returns nil when active, ctx.Err() on
// cancellation, or a wrapped error if the API consistently fails.
func (c *Client) WaitStreamActive(ctx context.Context, streamID string, poll time.Duration) error {
	if poll <= 0 {
		poll = 3 * time.Second
	}
	timer := time.NewTimer(0) // fire immediately on first iteration
	defer timer.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait stream active: %w (last error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-timer.C:
		}
		h, err := c.GetStreamHealth(ctx, streamID)
		if err == nil && h.StreamStatus == "active" {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		timer.Reset(poll)
	}
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, "GET", path, nil)
}

func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.do(ctx, "POST", path, payload)
}

func (c *Client) delete(ctx context.Context, path string) error {
	_, err := c.do(ctx, "DELETE", path, nil)
	return err
}

// do issues an authenticated request bound to ctx. Cancellation /
// deadlines on ctx propagate through to the HTTP transport, so the
// scheduler's 30s prepare timeout actually times out the YouTube call.
func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := c.Auth.HTTPClient()
	if err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	} else if method == "POST" {
		reqBody = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reqBody)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	// 204 No Content for DELETE is success with empty body.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, parseAPIError(resp.StatusCode, body)
}

// parseAPIError extracts the YouTube error envelope so callers can react to
// reason codes (redundantTransition, errorStreamInactive, invalidTransition,
// quotaExceeded, etc.) instead of pattern-matching the message string.
func parseAPIError(statusCode int, body []byte) *APIError {
	apiErr := &APIError{StatusCode: statusCode, Body: truncate(body, 300)}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		apiErr.Message = envelope.Error.Message
		if len(envelope.Error.Errors) > 0 {
			apiErr.Reason = envelope.Error.Errors[0].Reason
			if apiErr.Message == "" {
				apiErr.Message = envelope.Error.Errors[0].Message
			}
		}
	}
	return apiErr
}

func parseBroadcast(data []byte) (*Broadcast, error) {
	var raw struct {
		ID      string `json:"id"`
		Snippet struct {
			Title              string `json:"title"`
			Description        string `json:"description"`
			ScheduledStartTime string `json:"scheduledStartTime"`
			ActualStartTime    string `json:"actualStartTime"`
		} `json:"snippet"`
		ContentDetails struct {
			BoundStreamID string `json:"boundStreamId"`
		} `json:"contentDetails"`
		Status struct {
			LifeCycleStatus string `json:"lifeCycleStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	b := &Broadcast{
		ID:          raw.ID,
		Title:       raw.Snippet.Title,
		Description: raw.Snippet.Description,
		Status:      raw.Status.LifeCycleStatus,
		StreamID:    raw.ContentDetails.BoundStreamID,
	}
	if t, err := time.Parse(time.RFC3339, raw.Snippet.ScheduledStartTime); err == nil {
		b.ScheduledStart = t
	}
	if t, err := time.Parse(time.RFC3339, raw.Snippet.ActualStartTime); err == nil {
		b.ActualStart = t
	}
	return b, nil
}

func parseStream(data []byte) (*Stream, error) {
	var raw struct {
		ID      string `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
		CDN struct {
			IngestionInfo struct {
				IngestionAddress string `json:"ingestionAddress"`
				StreamName       string `json:"streamName"`
			} `json:"ingestionInfo"`
		} `json:"cdn"`
		Status struct {
			StreamStatus string `json:"streamStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return &Stream{
		ID:        raw.ID,
		Title:     raw.Snippet.Title,
		IngestURL: raw.CDN.IngestionInfo.IngestionAddress,
		StreamKey: raw.CDN.IngestionInfo.StreamName,
		Status:    raw.Status.StreamStatus,
	}, nil
}

func resolutionCategory(res string) string {
	switch {
	case strings.HasPrefix(res, "2560"):
		// 1440p (QHD). Used by the cinema-1440p24 preset. Without
		// this branch, the YouTube stream resource would be created
		// with resolution="variable" and the broadcast metadata
		// would mis-classify the stream — broadcast quality dashboard
		// shows "variable" instead of "1440p".
		return "1440p"
	case strings.HasPrefix(res, "1920"):
		return "1080p"
	case strings.HasPrefix(res, "1280"):
		return "720p"
	case strings.HasPrefix(res, "854"):
		return "480p"
	default:
		return "variable"
	}
}

func fpsCategory(fps int) string {
	if fps >= 60 {
		return "60fps"
	}
	return "30fps"
}

func truncate(data []byte, max int) string {
	s := string(data)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
