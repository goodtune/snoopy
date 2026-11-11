package attest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// Frame check thresholds.
const (
	minFrameWidth   = 640
	minFrameHeight  = 480
	minFrameStddev  = 2.0 // grey levels; a solid colour has 0
	frameTimeout    = 30 * time.Second
	frameMaxBytes   = 64 << 20
	frameFailLimit  = 2 // consecutive failures before the state is degraded
	frameCheckLabel = "frame"
)

// FrameChecker decodes one frame of the newest segment every Interval with
// ffmpeg and checks that it is a plausible screen image (experimental).
//
// ffmpeg parses a file the supervised user can influence, so it never runs as
// root: when the attestor is root, ffmpeg runs as FrameUser (default
// "nobody") and receives the segment as an already-open, read-only file
// descriptor (fd 3, "-i fd:3"), so it needs no access to the user's home.
// This needs FFmpeg 6.0 or newer for the fd: protocol.
type FrameChecker struct {
	FFmpeg   string
	User     string
	Interval time.Duration
	Logf     func(format string, args ...any)
	// LookPath and run are replaceable in tests.
	LookPath func(string) (string, error)

	lastRun     time.Time
	failures    int
	lastWH      []int
	lastResult  CheckResult
	missingOnce bool
	ran         bool
}

// Due reports whether a new decode should run at now.
func (f *FrameChecker) Due(now time.Time) bool {
	return !f.ran || now.Sub(f.lastRun) >= f.Interval
}

// Result returns the latest result, whether failures reached the limit, and
// the last decoded frame size.
func (f *FrameChecker) Result() (CheckResult, bool, []int) {
	if !f.ran {
		return CheckResult{Name: frameCheckLabel, Status: StatusSkip, Detail: "not run yet"}, false, nil
	}
	return f.lastResult, f.failures >= frameFailLimit, f.lastWH
}

// Run decodes one frame of segment (empty: no segment known) and records
// the outcome.
func (f *FrameChecker) Run(ctx context.Context, now time.Time, segment string) {
	f.ran = true
	f.lastRun = now
	r := CheckResult{Name: frameCheckLabel, Status: StatusPass}
	lookPath := f.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	bin, err := lookPath(f.FFmpeg)
	if err != nil {
		if !f.missingOnce && f.Logf != nil {
			f.Logf("frame check: %s not found; skipping frame checks (%v)", f.FFmpeg, err)
		}
		f.missingOnce = true
		f.failures = 0
		f.lastResult = CheckResult{Name: frameCheckLabel, Status: StatusSkip, Detail: f.FFmpeg + " not installed"}
		return
	}
	if segment == "" {
		f.failures++
		r.fail(ReasonFrameCheckFailed, "no current segment to decode")
		r.Detail = fmt.Sprintf("no segment (%d consecutive failure(s))", f.failures)
		f.lastResult = r
		return
	}
	w, h, stddev, err := f.decode(ctx, bin, segment)
	switch {
	case err != nil:
		f.failures++
		r.fail(ReasonFrameCheckFailed, "%v", err)
	case w < minFrameWidth || h < minFrameHeight:
		f.failures++
		r.fail(ReasonFrameCheckFailed, "frame is %dx%d, want at least %dx%d", w, h, minFrameWidth, minFrameHeight)
	case stddev < minFrameStddev:
		f.failures++
		r.fail(ReasonFrameCheckFailed, "frame %dx%d is uniform (stddev %.2f)", w, h, stddev)
	default:
		f.failures = 0
		f.lastWH = []int{w, h}
	}
	if r.Status == StatusPass {
		r.Detail = fmt.Sprintf("%dx%d frame, stddev %.1f", w, h, stddev)
	} else {
		r.Detail = fmt.Sprintf("decode failed (%d consecutive failure(s); degraded after %d)", f.failures, frameFailLimit)
	}
	f.lastResult = r
}

func (f *FrameChecker) decode(ctx context.Context, bin, segment string) (int, int, float64, error) {
	file, err := os.Open(segment) //nolint:gosec // segment path was verified by the screencast check
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = file.Close() }()
	ctx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	// The same seek-from-end approach as the recorder's live frames (an
	// in-progress segment has no index), but decoded to an 8-bit greyscale
	// PGM on stdout instead of a JPEG file.
	cmd := exec.CommandContext(ctx, bin, //nolint:gosec // binary from root-owned configuration
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-sseof", "-3", "-i", "fd:3",
		"-frames:v", "1", "-pix_fmt", "gray", "-c:v", "pgm", "-f", "image2pipe", "pipe:1")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Dir = "/"
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, n: frameMaxBytes}
	cmd.Stderr = &limitWriter{w: &stderr, n: 4096}
	if os.Geteuid() == 0 {
		cred, err := lookupCredential(f.User)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("frame user %q: %w", f.User, err)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setpgid: true}
	}
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return 0, 0, 0, fmt.Errorf("ffmpeg: %w: %s", err, msg)
		}
		return 0, 0, 0, fmt.Errorf("ffmpeg: %w", err)
	}
	return analysePGM(stdout.Bytes())
}

func lookupCredential(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		return nil, errors.New("refusing to run ffmpeg as root")
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// analysePGM parses a binary 8-bit PGM (P5) and returns its size and the
// standard deviation of its pixel values.
func analysePGM(data []byte) (int, int, float64, error) {
	br := bufio.NewReader(bytes.NewReader(data))
	var fields []int
	magic, err := pgmToken(br)
	if err != nil || magic != "P5" {
		return 0, 0, 0, fmt.Errorf("not a binary PGM image (magic %q)", magic)
	}
	for len(fields) < 3 {
		tok, err := pgmToken(br)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("PGM header: %w", err)
		}
		n, err := strconv.Atoi(tok)
		if err != nil || n <= 0 {
			return 0, 0, 0, fmt.Errorf("PGM header: bad number %q", tok)
		}
		fields = append(fields, n)
	}
	w, h, maxval := fields[0], fields[1], fields[2]
	if maxval > 255 {
		return 0, 0, 0, fmt.Errorf("PGM maxval %d: want 8-bit", maxval)
	}
	if w > 1<<15 || h > 1<<15 {
		return 0, 0, 0, fmt.Errorf("PGM %dx%d too large", w, h)
	}
	pix := make([]byte, w*h)
	if _, err := io.ReadFull(br, pix); err != nil {
		return 0, 0, 0, fmt.Errorf("PGM pixels: %w", err)
	}
	var sum, sumSq float64
	for _, p := range pix {
		v := float64(p)
		sum += v
		sumSq += v * v
	}
	n := float64(len(pix))
	mean := sum / n
	variance := math.Max(sumSq/n-mean*mean, 0)
	return w, h, math.Sqrt(variance), nil
}

// pgmToken reads one whitespace-delimited header token, skipping comments,
// and consumes exactly one whitespace byte after it.
func pgmToken(br *bufio.Reader) (string, error) {
	var tok []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case b == '#' && len(tok) == 0:
			if _, err := br.ReadString('\n'); err != nil {
				return "", err
			}
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			if len(tok) > 0 {
				return string(tok), nil
			}
		default:
			tok = append(tok, b)
		}
	}
}
