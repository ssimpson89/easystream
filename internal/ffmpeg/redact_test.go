package ffmpeg

import (
	"strings"
	"testing"
)

func TestRedactURLCredentialsUserinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   "rtsp://admin:Sup3rSecret@cam.local:554/Streaming/Channels/101",
			want: "rtsp://REDACTED:REDACTED@cam.local:554/Streaming/Channels/101",
		},
		{
			in:   "rtsps://user:pass@host/path?other=keep",
			want: "rtsps://REDACTED:REDACTED@host/path?other=keep",
		},
		{
			in:   "http://anonymous-feed/stream.m3u8",
			want: "http://anonymous-feed/stream.m3u8",
		},
	}
	for _, c := range cases {
		got := RedactURLCredentials(c.in)
		if got != c.want {
			t.Errorf("RedactURLCredentials(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactURLCredentialsSecretQuery(t *testing.T) {
	in := "srt://relay.example.com:9999?streamid=publish:test&passphrase=Sup3rSecretPwd&latency=4000000"
	got := RedactURLCredentials(in)
	if !strings.Contains(got, "passphrase=REDACTED") {
		t.Errorf("expected passphrase to be redacted, got %q", got)
	}
	if !strings.Contains(got, "streamid=publish") {
		t.Errorf("expected streamid to be preserved, got %q", got)
	}
	if !strings.Contains(got, "latency=4000000") {
		t.Errorf("expected latency to be preserved, got %q", got)
	}
}

func TestRedactURLCredentialsEmpty(t *testing.T) {
	if RedactURLCredentials("") != "" {
		t.Error("expected empty input to round-trip empty")
	}
}

func TestRedactStreamKey(t *testing.T) {
	cases := []struct {
		name string
		line string
		key  string
		want string
	}{
		{
			name: "rtmp URL containing key",
			line: "Failed to connect to rtmp://a.rtmp.youtube.com/live2/abcd-1234-efgh-5678",
			key:  "abcd-1234-efgh-5678",
			want: "Failed to connect to rtmp://a.rtmp.youtube.com/live2/<redacted>",
		},
		{
			name: "no key in line",
			line: "warning: dropped frame",
			key:  "abcd-1234-efgh-5678",
			want: "warning: dropped frame",
		},
		{
			name: "empty key skips redaction",
			line: "rtmp://example/live2/somekey",
			key:  "",
			want: "rtmp://example/live2/somekey",
		},
		{
			name: "short key skips redaction (avoid false positives)",
			line: "the abc rtmp://thing",
			key:  "abc",
			want: "the abc rtmp://thing",
		},
		{
			name: "multiple occurrences",
			line: "key=abcd-1234-efgh-5678 url=rtmp://x/abcd-1234-efgh-5678",
			key:  "abcd-1234-efgh-5678",
			want: "key=<redacted> url=rtmp://x/<redacted>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactStreamKey(tc.line, tc.key)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
