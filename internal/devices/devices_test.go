package devices

import "testing"

// TestClassifyCaptureCards pins the device-classification against real
// device names we expect to see in production. Anything that comes in
// as v4l2 / avfoundation / dshow should group under "Capture cards" so
// it shows up next to other capture hardware in the picker — not next
// to the FaceTime camera.
//
// The capture path itself is name-agnostic (the v4l2/avfoundation/dshow
// backends read whatever the OS exposes), so a miscategorisation here
// is a UX bug, not a streaming bug.
func TestClassifyCaptureCards(t *testing.T) {
	cases := []struct {
		name string
		want DeviceType
	}{
		// BlackMagic — dedicated decklink backend (covered separately)
		// SDI / HDMI USB capture (most common in church installs)
		{"Magewell USB Capture HDMI 4K Plus", TypeCaptureCard},
		{"Magewell Pro Capture SDI", TypeCaptureCard},
		{"INOGENI 4K HDMI to USB", TypeCaptureCard},
		{"INOGENI SDI to USB", TypeCaptureCard},
		{"AJA U-TAP HDMI", TypeCaptureCard},
		{"AJA U-TAP SDI", TypeCaptureCard},
		{"Elgato Cam Link 4K", TypeCaptureCard},
		{"Elgato HD60 S+", TypeCaptureCard},
		{"AVerMedia Live Gamer 4K", TypeCaptureCard},
		{"Epiphan AV.io HD", TypeCaptureCard},
		{"YUAN SDI Capture", TypeCaptureCard},
		// Cameras
		{"FaceTime HD Camera", TypeCamera},
		{"Studio Display Camera", TypeCamera},
		{"OBS Virtual Camera", TypeCamera},
		{"Ssimpson Desk View Camera", TypeCamera},
		{"iPhone (2)", TypeCamera},
		// Screen capture
		{"Capture screen 0", TypeScreen},
	}
	for _, tc := range cases {
		got := classifyVideoDevice(Device{Name: tc.name, Backend: "avfoundation"})
		if got != tc.want {
			t.Errorf("classifyVideoDevice(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestClassifyDeckLinkBackendIsSDI confirms BlackMagic devices come
// through the dedicated SDI backend regardless of name.
func TestClassifyDeckLinkBackendIsSDI(t *testing.T) {
	got := classifyVideoDevice(Device{Name: "DeckLink Mini Recorder HD", Backend: "decklink"})
	if got != TypeSDI {
		t.Errorf("DeckLink should classify as SDI, got %q", got)
	}
}
