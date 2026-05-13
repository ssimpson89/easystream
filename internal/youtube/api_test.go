package youtube

import (
	"errors"
	"testing"
)

func TestParseAPIErrorExtractsReason(t *testing.T) {
	body := []byte(`{
	  "error": {
	    "code": 403,
	    "message": "The broadcast cannot be transitioned to the requested status.",
	    "errors": [
	      {
	        "domain": "youtube.liveBroadcast",
	        "reason": "redundantTransition",
	        "message": "Broadcast is already in the requested state."
	      }
	    ]
	  }
	}`)
	err := parseAPIError(403, body)
	if err.StatusCode != 403 {
		t.Errorf("StatusCode = %d, want 403", err.StatusCode)
	}
	if err.Reason != "redundantTransition" {
		t.Errorf("Reason = %q, want redundantTransition", err.Reason)
	}
	if !IsReason(err, "redundantTransition") {
		t.Error("IsReason failed to recognize redundantTransition")
	}
	if IsReason(err, "invalidTransition") {
		t.Error("IsReason matched wrong reason")
	}
}

func TestIsReasonNonAPIError(t *testing.T) {
	if IsReason(errors.New("not a youtube error"), "redundantTransition") {
		t.Error("IsReason should be false for non-APIError values")
	}
	if IsReason(nil, "x") {
		t.Error("IsReason(nil) should be false")
	}
}

func TestParseAPIErrorFallback(t *testing.T) {
	// Non-JSON body — Reason stays empty but Body is preserved.
	err := parseAPIError(500, []byte("Internal Server Error"))
	if err.StatusCode != 500 {
		t.Errorf("StatusCode = %d", err.StatusCode)
	}
	if err.Reason != "" {
		t.Errorf("expected empty Reason for non-JSON body, got %q", err.Reason)
	}
	if err.Body == "" {
		t.Error("Body should be preserved")
	}
	// Error() should still produce something useful.
	if err.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

func TestResolutionCategoryMatchesYouTubeStreamResolutions(t *testing.T) {
	// liveStreams.insert accepts these resolution categories; anything
	// not on the list falls back to "variable" which displays as
	// unresolved on YouTube's broadcast dashboard. Locking each
	// resolution prefix here prevents a new preset (4K, etc.) from
	// silently regressing the YouTube classification.
	cases := map[string]string{
		"2560x1440": "1440p",
		"1920x1080": "1080p",
		"1280x720":  "720p",
		"854x480":   "480p",
		"640x360":   "variable",
	}
	for res, want := range cases {
		if got := resolutionCategory(res); got != want {
			t.Errorf("resolutionCategory(%q) = %q, want %q", res, got, want)
		}
	}
}
