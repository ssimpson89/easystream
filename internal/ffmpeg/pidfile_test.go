package ffmpeg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidFile_NoFile(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "nope.pid")}
	got, err := p.ReapOrphan()
	if err != nil || got != 0 {
		t.Fatalf("expected (0, nil) for missing file, got (%d, %v)", got, err)
	}
}

func TestPidFile_StaleDead(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	// Write a PID that is essentially guaranteed to not exist.
	if err := p.Write(99999999); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0 (dead PID), got %d", got)
	}
	// Stale file should be cleaned up.
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected pid file to be removed, got err=%v", err)
	}
}

func TestPidFile_OwnPidNotReaped(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	// Use our own PID — definitely alive, but definitely not ffmpeg.
	// The reaper should refuse to kill it because isFFmpegProcess returns false.
	if err := p.Write(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("reaper should NOT have killed our own (non-ffmpeg) process, got reaped pid %d", got)
	}
	// Stale file should still be cleaned up so we don't keep checking the same PID forever.
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected pid file to be removed, got err=%v", err)
	}
}

func TestPidFile_Garbage(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := os.WriteFile(p.Path, []byte("not-a-number"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil || got != 0 {
		t.Fatalf("expected (0, nil) for garbage file, got (%d, %v)", got, err)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected garbage pid file to be removed")
	}
}

func TestPidFile_WriteAndClear(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := p.Write(1234); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1234" {
		t.Fatalf("expected '1234', got %q", string(data))
	}
	p.Clear()
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatal("expected pid file to be removed after Clear()")
	}
	// Clear is idempotent.
	p.Clear()
}

func TestIsEasyStreamCommandLineRequiresAllMarkers(t *testing.T) {
	line := "ffmpeg -hide_banner -progress pipe:1 -af astats=metadata=1:reset=1:length=1,ametadata=print:key=lavfi.astats.Overall.RMS_level:file=/dev/stderr -f rtp rtp://127.0.0.1:52001?pkt_size=1200 -f rtp rtp://127.0.0.1:52002?pkt_size=1200"
	if !isFFmpegCommandLine(line) {
		t.Fatal("expected ffmpeg command line")
	}
	if !isEasyStreamCommandLine(line) {
		t.Fatal("expected EasyStream FFmpeg markers")
	}
	if isEasyStreamCommandLine("ffmpeg -i input.mov output.mp4") {
		t.Fatal("unrelated FFmpeg command should not match EasyStream markers")
	}
	if isFFmpegCommandLine("ffmpeg.test -test.run TestSomething") {
		t.Fatal("test binary should not match ffmpeg executable")
	}
}
