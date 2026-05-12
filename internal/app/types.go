package app

import (
	"encoding/json"
	"os"
	"time"

	"github.com/ssimpson89/easystream/internal/atomicfile"
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

// persistedConfig is the subset of ffmpeg.Config we save across restarts.
// HLSDir and Binary are recomputed at startup; Network is fixed.
type persistedConfig struct {
	PresetID   string            `json:"presetId"`
	Encoder    ffmpeg.Encoder    `json:"encoder,omitempty"`
	OutputMode ffmpeg.OutputMode `json:"outputMode"`
	IngestURL  string            `json:"ingestUrl"`
	StreamName string            `json:"streamName"`
	Input      ffmpeg.Input      `json:"input"`
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
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0600)
}
