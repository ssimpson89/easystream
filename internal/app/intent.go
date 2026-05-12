package app

import (
	"encoding/json"
	"os"
	"time"

	"github.com/ssimpson89/easystream/internal/atomicfile"
)

// streamIntent records the operator's intent to be live, persisted across
// EasyStream restarts. On boot, if intent says we were live and the record
// is fresh, EasyStream re-spawns FFmpeg with the same destination so the
// broadcast resumes transparently — "as if nothing happened."
//
// The intent file's presence is the signal. Created on every Go Live,
// removed on every Stop. If we crash uncleanly, the file remains and the
// next boot uses it to resume.
//
// Intent is intentionally separate from supervisor State (which only
// reflects what FFmpeg is doing right now). A crashed supervisor has no
// state; persisted intent is what tells us the operator wanted us live.
type streamIntent struct {
	Live        bool      `json:"live"`
	Mode        string    `json:"mode,omitempty"` // "manual" | "go-live-now" | "scheduled"
	BroadcastID string    `json:"broadcastId,omitempty"`
	StreamID    string    `json:"streamId,omitempty"`
	IngestURL   string    `json:"ingestUrl,omitempty"`
	StreamName  string    `json:"streamName,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
}

// maxIntentAge bounds how old an intent record can be before we ignore it.
// Anything older than this is presumed stale from a previous abandoned
// session — even the longest church services are well under 6 hours.
const maxIntentAge = 6 * time.Hour

func loadStreamIntent(path string) (streamIntent, error) {
	var i streamIntent
	data, err := os.ReadFile(path)
	if err != nil {
		return i, err
	}
	err = json.Unmarshal(data, &i)
	return i, err
}

func saveStreamIntent(path string, i streamIntent) error {
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0600)
}

func clearStreamIntent(path string) {
	_ = os.Remove(path)
}

// fresh returns true if the intent is recent enough to act on. A stale
// intent (from days ago) shouldn't trigger an auto-resume.
func (i streamIntent) fresh() bool {
	if i.StartedAt.IsZero() {
		return false
	}
	return time.Since(i.StartedAt) < maxIntentAge
}
