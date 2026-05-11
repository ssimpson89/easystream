package app

import (
	"fmt"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// confidenceIndicator is a traffic-light view of one stream characteristic
// for the operator's "is the broadcast actually working?" panel.
type confidenceIndicator struct {
	Label  string `json:"label"`
	Status string `json:"status"` // "green" | "yellow" | "red" | "unknown" | "off"
	Detail string `json:"detail,omitempty"`
}

// computeConfidence turns raw supervisor + destination signals into
// operator-facing traffic-light indicators. Answers "is the church
// actually live?" — separate from the engineering metrics in the UI.
func computeConfidence(stream ffmpeg.Status, health streamHealthSnapshot, broadcastID, destMode string) []confidenceIndicator {
	out := []confidenceIndicator{}

	// 1. Encoder is sending.
	enc := confidenceIndicator{Label: "Encoder"}
	switch stream.State {
	case ffmpeg.StateRunning:
		enc.Status = "green"
		enc.Detail = "Sending data"
	case ffmpeg.StateDegraded:
		enc.Status = "yellow"
		enc.Detail = "Stalled or backed up"
	case ffmpeg.StateRestarting, ffmpeg.StateStarting:
		enc.Status = "yellow"
		enc.Detail = "Reconnecting"
	case ffmpeg.StateFailed:
		enc.Status = "red"
		enc.Detail = "Stopped on error"
	default:
		enc.Status = "off"
		enc.Detail = "Idle"
	}
	out = append(out, enc)

	// 2. Audio detected.
	aud := confidenceIndicator{Label: "Audio"}
	if stream.State != ffmpeg.StateRunning && stream.State != ffmpeg.StateDegraded {
		aud.Status = "off"
	} else if stream.AudioRMSAt.IsZero() {
		aud.Status = "unknown"
		aud.Detail = "Waiting for level..."
	} else if !stream.AudioDetectedAt.IsZero() && time.Since(stream.AudioDetectedAt) < 5*time.Second {
		aud.Status = "green"
		aud.Detail = fmt.Sprintf("%.0f dB RMS", stream.AudioRMSdB)
	} else {
		aud.Status = "red"
		aud.Detail = "Silence detected — check mic / source"
	}
	out = append(out, aud)

	// 3. Destination receiving (YouTube-side health, when available).
	dest := confidenceIndicator{Label: "Destination"}
	switch {
	case stream.State != ffmpeg.StateRunning && stream.State != ffmpeg.StateDegraded:
		dest.Status = "off"
	case broadcastID == "" || health.Source == "":
		// Manual RTMP / no YT API — we can only infer from encoder
		dest.Status = "unknown"
		dest.Detail = "No platform feedback (manual RTMP)"
	case health.HealthStatus == "good":
		dest.Status = "green"
		dest.Detail = "YouTube: good"
	case health.HealthStatus == "ok":
		dest.Status = "green"
		dest.Detail = "YouTube: ok"
	case health.HealthStatus == "bad":
		dest.Status = "yellow"
		dest.Detail = "YouTube reports issues"
		if len(health.Issues) > 0 {
			dest.Detail = "YouTube: " + health.Issues[0]
		}
	case health.HealthStatus == "noData":
		dest.Status = "red"
		dest.Detail = "YouTube not receiving"
	default:
		dest.Status = "unknown"
		dest.Detail = "Checking..."
	}
	out = append(out, dest)

	return out
}
