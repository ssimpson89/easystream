package ffmpeg

import "testing"

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
