package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExtractFrameFromGrowingWebM checks that the frame taken from a WebM
// that is still being written (no duration, no cues, last cluster cut off)
// is its newest frame, not its first: ffmpeg cannot use -sseof on such a
// file and silently decodes from the start instead.
func TestExtractFrameFromGrowingWebM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	live := filepath.Join(dir, "live.webm")
	// 12 s with a keyframe every 2 s, written like a live recording.
	runCmd(t, "ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-t", "12",
		"-c:v", "libvpx", "-b:v", "300k", "-g", "50", "-f", "webm", "-live", "1", live)
	full, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	growing := filepath.Join(dir, "growing.webm")
	if err := os.WriteFile(growing, full[:len(full)*3/4], 0o644); err != nil {
		t.Fatal(err)
	}

	got := filepath.Join(dir, "got.jpg")
	if err := extractFrame(context.Background(), growing, got); err != nil {
		t.Fatal(err)
	}
	// Reference: decode the whole file and keep the last frame.
	last := filepath.Join(dir, "last.jpg")
	runCmd(t, "ffmpeg", "-loglevel", "error", "-i", growing, "-update", "1", "-q:v", "2", "-y", last)
	first := filepath.Join(dir, "first.jpg")
	runCmd(t, "ffmpeg", "-loglevel", "error", "-i", growing, "-frames:v", "1", "-q:v", "2", "-y", first)

	g, l, f := readFile(t, got), readFile(t, last), readFile(t, first)
	if bytes.Equal(g, f) {
		t.Fatal("extracted the first frame of the recording")
	}
	if !bytes.Equal(g, l) {
		t.Fatal("extracted frame is not the newest frame on disk")
	}
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
