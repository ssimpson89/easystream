package youtube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://www.googleapis.com/youtube/v3"

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
	StreamStatus       string   `json:"streamStatus"`       // active|created|error|inactive|ready
	HealthStatus       string   `json:"healthStatus"`       // good|ok|bad|noData
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
func (c *Client) CreateBroadcast(title, description string, scheduledStart time.Time, privacy string) (*Broadcast, error) {
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
			"enableAutoStart":  false,
			"enableAutoStop":   false,
			"enableDvr":        true,
			"latencyPreference": "normal",
		},
		"status": map[string]any{
			"privacyStatus":           privacy,
			"selfDeclaredMadeForKids": false,
		},
	}
	body, err := c.post("/liveBroadcasts?part=snippet,contentDetails,status", payload)
	if err != nil {
		return nil, fmt.Errorf("create broadcast: %w", err)
	}
	return parseBroadcast(body)
}

// ListUpcomingBroadcasts returns broadcasts with status upcoming or active.
func (c *Client) ListUpcomingBroadcasts() ([]Broadcast, error) {
	var all []Broadcast
	for _, status := range []string{"upcoming", "active"} {
		broadcasts, err := c.listBroadcasts(status)
		if err != nil {
			return nil, err
		}
		all = append(all, broadcasts...)
	}
	return all, nil
}

func (c *Client) listBroadcasts(broadcastStatus string) ([]Broadcast, error) {
	path := fmt.Sprintf("/liveBroadcasts?part=snippet,contentDetails,status&broadcastStatus=%s&maxResults=25", broadcastStatus)
	body, err := c.get(path)
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

// CreateStream creates a reusable live stream endpoint.
func (c *Client) CreateStream(title string, resolution string, fps int) (*Stream, error) {
	payload := map[string]any{
		"snippet": map[string]any{
			"title": title,
		},
		"cdn": map[string]any{
			"frameRate":  fpsCategory(fps),
			"ingestionType": "rtmp",
			"resolution": resolutionCategory(resolution),
		},
		"contentDetails": map[string]any{
			"isReusable": true,
		},
	}
	body, err := c.post("/liveStreams?part=snippet,cdn,contentDetails,status", payload)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}
	return parseStream(body)
}

// ListStreams returns the user's live streams.
func (c *Client) ListStreams() ([]Stream, error) {
	body, err := c.get("/liveStreams?part=snippet,cdn,status&mine=true&maxResults=50")
	if err != nil {
		return nil, fmt.Errorf("list streams: %w", err)
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var streams []Stream
	for _, item := range resp.Items {
		s, err := parseStream(item)
		if err != nil {
			continue
		}
		streams = append(streams, *s)
	}
	return streams, nil
}

// GetStream returns a stream by ID.
func (c *Client) GetStream(streamID string) (*Stream, error) {
	path := fmt.Sprintf("/liveStreams?part=snippet,cdn,status&id=%s", url.QueryEscape(streamID))
	body, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("get stream: %w", err)
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Items) == 0 {
		return nil, fmt.Errorf("stream %s not found", streamID)
	}
	return parseStream(resp.Items[0])
}

// GetStreamHealth returns the platform-reported health for a stream.
// Use this while broadcasting to know whether YouTube is actually
// receiving and accepting the feed.
func (c *Client) GetStreamHealth(streamID string) (*StreamHealth, error) {
	path := fmt.Sprintf("/liveStreams?part=status&id=%s", url.QueryEscape(streamID))
	body, err := c.get(path)
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

// BindBroadcast binds a broadcast to a stream.
func (c *Client) BindBroadcast(broadcastID, streamID string) error {
	path := fmt.Sprintf("/liveBroadcasts/bind?id=%s&streamId=%s&part=id",
		url.QueryEscape(broadcastID), url.QueryEscape(streamID))
	_, err := c.post(path, nil)
	if err != nil {
		return fmt.Errorf("bind broadcast: %w", err)
	}
	return nil
}

// TransitionBroadcast changes the broadcast status (testing, live, complete).
func (c *Client) TransitionBroadcast(broadcastID, status string) error {
	path := fmt.Sprintf("/liveBroadcasts/transition?id=%s&broadcastStatus=%s&part=id,status",
		url.QueryEscape(broadcastID), url.QueryEscape(status))
	_, err := c.post(path, nil)
	if err != nil {
		return fmt.Errorf("transition broadcast to %s: %w", status, err)
	}
	return nil
}

// EnsureStream finds or creates a reusable stream for the given quality.
func (c *Client) EnsureStream(title, resolution string, fps int) (*Stream, error) {
	streams, err := c.ListStreams()
	if err != nil {
		return nil, err
	}
	// Look for a reusable stream with matching title.
	for _, s := range streams {
		if s.Title == title {
			return &s, nil
		}
	}
	// Create a new one.
	return c.CreateStream(title, resolution, fps)
}

func (c *Client) get(path string) ([]byte, error) {
	client, err := c.Auth.HTTPClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Get(apiBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("YouTube API %s returned %d: %s", path, resp.StatusCode, truncate(body, 300))
	}
	return body, nil
}

func (c *Client) post(path string, payload any) ([]byte, error) {
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
	} else {
		reqBody = strings.NewReader("")
	}
	req, err := http.NewRequest("POST", apiBase+path, reqBody)
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("YouTube API POST %s returned %d: %s", path, resp.StatusCode, truncate(body, 300))
	}
	return body, nil
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
