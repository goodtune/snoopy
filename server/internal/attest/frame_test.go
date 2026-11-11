package attest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pgm(w, h int, fill func(i int) byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "P5\n# made by test\n%d %d\n255\n", w, h)
	for i := range w * h {
		b.WriteByte(fill(i))
	}
	return b.Bytes()
}

func TestAnalysePGM(t *testing.T) {
	w, h, sd, err := analysePGM(pgm(4, 2, func(i int) byte { return byte(i * 30) }))
	if err != nil || w != 4 || h != 2 || sd < 60 {
		t.Fatalf("got %d %d %.1f %v", w, h, sd, err)
	}
	_, _, sd, err = analysePGM(pgm(3, 3, func(int) byte { return 7 }))
	if err != nil || sd != 0 {
		t.Fatalf("uniform: %.2f %v", sd, err)
	}
	for _, bad := range [][]byte{nil, []byte("P6\n1 1\n255\n\x00\x00\x00"), []byte("P5\n2 2\n255\n\x00"), []byte("P5\n2 2\n65535\n")} {
		if _, _, _, err := analysePGM(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeFFmpeg writes a shell script that checks it got the segment on fd 3
// and prints the given PGM file (or fails).
func fakeFFmpeg(t *testing.T, pgmFile string, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	script += "case \"$*\" in *'-i fd:3'*) ;; *) echo bad args >&2; exit 3;; esac\n"
	script += "head -c 4 <&3 | grep -q webm || { echo no fd3 >&2; exit 4; }\n"
	if fail {
		script += "echo 'Invalid data found when processing input' >&2; exit 1\n"
	} else {
		script += "cat '" + pgmFile + "'\n"
	}
	p := filepath.Join(dir, "ffmpeg")
	writeFile(t, p, script, 0o755)
	return p
}

func TestFrameChecker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("would drop privileges to nobody")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	seg := filepath.Join(dir, "screen.webm")
	writeFile(t, seg, "webm-data", 0o644)
	good := filepath.Join(dir, "good.pgm")
	if err := os.WriteFile(good, pgm(1024, 768, func(i int) byte { return byte(i % 251) }), 0o644); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "small.pgm")
	if err := os.WriteFile(small, pgm(320, 200, func(i int) byte { return byte(i % 251) }), 0o644); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(dir, "flat.pgm")
	if err := os.WriteFile(flat, pgm(800, 600, func(int) byte { return 0 }), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	f := &FrameChecker{FFmpeg: fakeFFmpeg(t, good, false), User: "nobody", Interval: 5 * time.Minute}
	if !f.Due(now) {
		t.Fatal("not due initially")
	}
	f.Run(context.Background(), now, seg)
	res, bad, wh := f.Result()
	if res.Status != StatusPass || bad || len(wh) != 2 || wh[0] != 1024 || wh[1] != 768 {
		t.Fatalf("good: %+v %v %v", res, bad, wh)
	}
	if f.Due(now.Add(time.Minute)) || !f.Due(now.Add(5*time.Minute)) {
		t.Fatal("Due does not follow Interval")
	}

	for _, tc := range []struct {
		name  string
		bin   string
		match string
	}{
		{"small", fakeFFmpeg(t, small, false), "at least 640x480"},
		{"flat", fakeFFmpeg(t, flat, false), "uniform"},
		{"decode error", fakeFFmpeg(t, "", true), "Invalid data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &FrameChecker{FFmpeg: tc.bin, User: "nobody", Interval: time.Minute}
			f.Run(context.Background(), now, seg)
			res, bad, _ := f.Result()
			if res.Status != StatusFail || bad || !strings.Contains(strings.Join(res.Notes, " "), tc.match) {
				t.Fatalf("first failure: %+v bad=%v", res, bad)
			}
			f.Run(context.Background(), now.Add(time.Minute), seg)
			if _, bad, _ := f.Result(); !bad {
				t.Fatal("two consecutive failures not reported")
			}
		})
	}

	// Recovery resets the failure count.
	f.FFmpeg = fakeFFmpeg(t, flat, false)
	f.Run(context.Background(), now, seg)
	f.FFmpeg = fakeFFmpeg(t, good, false)
	f.Run(context.Background(), now, seg)
	if _, bad, _ := f.Result(); bad {
		t.Fatal("failure count not reset after success")
	}
}

func TestFrameCheckerMissingFFmpeg(t *testing.T) {
	var logs []string
	f := &FrameChecker{
		FFmpeg: "ffmpeg", Interval: time.Minute,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	for range 3 {
		f.Run(context.Background(), time.Now(), "/x.webm")
	}
	res, bad, _ := f.Result()
	if res.Status != StatusSkip || bad || len(logs) != 1 {
		t.Fatalf("%+v bad=%v logs=%q", res, bad, logs)
	}
}
