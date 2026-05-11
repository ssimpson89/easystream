package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// streamHealthSnapshot is the latest result of polling the destination
// (currently YouTube) for the bound stream's health.
type streamHealthSnapshot struct {
	StreamStatus      string    `json:"streamStatus,omitempty"` // active|created|error|inactive|ready
	HealthStatus      string    `json:"healthStatus,omitempty"` // good|ok|bad|noData
	Issues            []string  `json:"issues,omitempty"`
	LastUpdate        time.Time `json:"lastUpdate,omitempty"`
	Source            string    `json:"source,omitempty"` // "youtube" | "" if not available
	HasBroadcast      bool      `json:"hasBroadcast"`
	ConcurrentViewers *int      `json:"concurrentViewers,omitempty"` // nil = unavailable
}

// persistedConfig is a subset of ffmpeg.Config we save across restarts,
// plus a few UI-only fields (active destination tab) so the UI restores
// exactly as the user left it. HLSDir and Binary are recomputed at
// startup; Network is fixed.
type persistedConfig struct {
	PresetID        string            `json:"presetId"`
	Encoder         ffmpeg.Encoder    `json:"encoder,omitempty"`
	OutputMode      ffmpeg.OutputMode `json:"outputMode"`
	IngestURL       string            `json:"ingestUrl"`
	StreamName      string            `json:"streamName"`
	Input           ffmpeg.Input      `json:"input"`
	DestinationMode string            `json:"destinationMode,omitempty"` // "scheduled" | "now" | "manual"
}

func loadPersistedConfig(path string) (persistedConfig, error) {
	var p persistedConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	return p, err
}

func savePersistedConfig(path string, cfg ffmpeg.Config) error {
	p := persistedConfig{
		PresetID:   cfg.Preset.ID,
		Encoder:    cfg.Encoder,
		OutputMode: cfg.OutputMode,
		IngestURL:  cfg.IngestURL,
		StreamName: cfg.StreamName,
		Input:      cfg.Input,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
