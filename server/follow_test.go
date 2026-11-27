package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gnomeLikeWebM writes a VP8 WebM the way GNOME Shell's software screencast
// pipeline does: keyframes disabled (one keyframe, the first frame), written
// live (no duration, no cues).
func gnomeLikeWebM(t *testing.T, path string, seconds int) {
	t.Helper()
	ff(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", strconv.Itoa(seconds),
		"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "16", "-static-thresh", "1000",
		"-g", "100000", "-auto-alt-ref", "0", "-lag-in-frames", "0", "-b:v", "300k",
		"-f", "webm", "-live", "1", path)
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if k := strings.Count(string(out), "K"); k != 1 {
		t.Fatalf("test recording has %d keyframes, want 1 like GNOME's", k)
	}
}

func ff(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// selectedFrames decodes path with the follower's filter and returns the
// JPEGs it emits.
func selectedFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	in, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	var frames [][]byte
	// Same command as decode, over the finished file.
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1", "-i", "pipe:0",
		"-an", "-vf", "select='isnan(prev_selected_t)+gte(t-prev_selected_t,1)'", "-fps_mode", "passthrough",
		"-c:v", "mjpeg", "-q:v", "2", "-threads", "1", "-f", "image2pipe", "pipe:1")
	cmd.Stdin = in
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := readJPEGs(bytes.NewReader(out), maxFrameBytes, func(b []byte) { frames = append(frames, b) }); err != nil {
		t.Fatal(err)
	}
	return frames
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFollowerShowsNewestFrameOfGrowingGNOMERecording is the regression test
// for the live view showing the first frame of the segment: with a single
// keyframe per segment only a decoder that follows the file can produce the
// current frame.
func TestFollowerShowsNewestFrameOfGrowingGNOMERecording(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.webm")
	gnomeLikeWebM(t, src, 12)
	full, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// The whole recording's selected frames; the follower must end on the
	// last one, and pass through later ones as the file grows.
	f := &frameFollower{dir: dir, ffmpeg: "ffmpeg", every: time.Second, poll: 50 * time.Millisecond, logf: t.Logf}
	want := selectedFrames(t, src)
	if len(want) < 10 {
		t.Fatalf("reference produced %d frames", len(want))
	}

	rec := filepath.Join(dir, "recording.webm")
	out, err := os.Create(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	third := len(full) / 3
	if _, err := out.Write(full[:third]); err != nil {
		t.Fatal(err)
	}

	f.newest = func(string) (string, error) { return rec, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitUntil(t, "a first frame", func() bool { b, _ := f.Latest(); return b != nil })
	early, _ := f.Latest()

	// GNOME keeps appending.
	for off := third; off < len(full); off += 4096 {
		if _, err := out.Write(full[off:min(off+4096, len(full))]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	last := want[len(want)-1]
	waitUntil(t, "the newest frame", func() bool { b, _ := f.Latest(); return bytes.Equal(b, last) })
	if bytes.Equal(early, last) || bytes.Equal(last, want[0]) {
		t.Fatal("the picture did not move on as the recording grew")
	}
}

func TestFollowerSwitchesSegmentsAndNeverGoesBack(t *testing.T) {
	var mu sync.Mutex
	listing := "a"
	var started []string
	f := &frameFollower{
		poll: 10 * time.Millisecond, logf: t.Logf,
		newest: func(string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if listing == "" {
				return "", errors.New("no video files")
			}
			return listing, nil
		},
		follow: func(ctx context.Context, path string) {
			mu.Lock()
			started = append(started, path)
			mu.Unlock()
			<-ctx.Done()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	// a; then b starts (segment change); a gets its final write and is
	// newest again for a moment; then c; then the directory is unreadable.
	for _, l := range []string{"a", "b", "a", "b", "c", "", "c"} {
		mu.Lock()
		listing = l
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if got := strings.Join(started, ","); got != "a,b,c" {
		t.Fatalf("followed %s, want a,b,c", got)
	}
}

func testJPEG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReadJPEGs(t *testing.T) {
	a := testJPEG(t, color.RGBA{255, 0, 0, 255})
	b := testJPEG(t, color.RGBA{0, 0, 255, 255})
	// c has a comment segment containing FF D9, which is not an EOI.
	c := append([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0x00, 0x06, 'x', 0xFF, 0xD9, 'y'}, testJPEG(t, color.White)[2:]...)
	var got [][]byte
	if err := readJPEGs(bytes.NewReader(bytes.Join([][]byte{a, b, c}, nil)), maxFrameBytes, func(img []byte) { got = append(got, img) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
		t.Fatalf("split into %d images", len(got))
	}
	for _, img := range got {
		if _, err := jpeg.Decode(bytes.NewReader(img)); err != nil {
			t.Fatalf("split image does not decode: %v", err)
		}
	}
	if err := readJPEGs(bytes.NewReader(nil), maxFrameBytes, func([]byte) {}); err != nil {
		t.Fatalf("empty stream: %v", err)
	}
	if err := readJPEGs(bytes.NewReader(a[:len(a)-10]), maxFrameBytes, func([]byte) {}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated stream: %v", err)
	}
	if err := readJPEGs(bytes.NewReader(a), 100, func([]byte) {}); err == nil {
		t.Fatal("oversized image accepted")
	}
}

func TestTailFileFollowsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var mu sync.Mutex
	var got bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- tailFile(ctx, writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return got.Write(p)
		}), r, 5*time.Millisecond)
	}()
	for _, s := range []string{"one ", "two ", "three"} {
		_, _ = w.WriteString(s)
		time.Sleep(20 * time.Millisecond)
	}
	waitUntil(t, "all appends", func() bool { mu.Lock(); defer mu.Unlock(); return got.String() == "one two three" })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("tailFile: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
