package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// frameFollower keeps the newest frame of the recording in progress.
//
// GNOME Shell's software (VP8/WebM) screencast pipeline runs vp8enc with
// keyframe-mode=disabled: a segment has a single keyframe, its first frame,
// and every later frame depends on all the frames before it. No frame can be
// decoded without decoding the segment from its start, so a one-shot ffmpeg
// per capture cannot return a recent frame cheaply (and -sseof does not work
// on a file still being written: it has no duration, and ffmpeg then decodes
// the first frame).
//
// Instead one long-lived ffmpeg per segment decodes the file as GNOME
// appends to it, like `tail -f`, and writes at most one JPEG per second of
// recording (GNOME's pipewiresrc resends the last frame every second on a
// static screen, so the final state after a change is always emitted). The
// capture loop publishes the newest one. When a newer segment appears the
// follower moves to it and never goes back to a segment it left: during
// the overlap at a segment change the old file still gets its last writes.
type frameFollower struct {
	dir    string
	ffmpeg string
	// every is the minimum recording time between emitted frames.
	every time.Duration
	// poll is how often the directory is checked for a newer segment and
	// how long the reader waits at the end of the file for more data.
	poll time.Duration
	logf func(string, ...any)
	// newest returns the most recent recording in dir (findMostRecentVideo).
	newest func(dir string) (string, error)
	// follow decodes one segment until its context is done; nil means
	// followFile (replaceable in tests).
	follow func(ctx context.Context, path string)

	mu     sync.Mutex
	latest []byte
	seq    uint64
}

// maxFrameBytes bounds one JPEG read from ffmpeg.
const maxFrameBytes = 32 << 20

// Latest returns the newest frame (nil before the first) and a sequence
// number that changes with every new frame.
func (f *frameFollower) Latest() ([]byte, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest, f.seq
}

func (f *frameFollower) set(jpeg []byte) {
	f.mu.Lock()
	f.latest = jpeg
	f.seq++
	f.mu.Unlock()
}

// Run follows the newest segment until ctx is done.
func (f *frameFollower) Run(ctx context.Context) {
	var (
		current string
		stop    context.CancelFunc
		done    chan struct{}
	)
	retired := map[string]bool{}
	defer func() {
		if stop != nil {
			stop()
			<-done
		}
	}()
	t := time.NewTicker(f.poll)
	defer t.Stop()
	for {
		if p, err := f.newest(f.dir); err == nil && p != current && !retired[p] {
			if stop != nil {
				stop()
				<-done
				retired[current] = true
			}
			current = p
			fctx, cancel := context.WithCancel(ctx)
			stop = cancel
			done = make(chan struct{})
			follow := f.follow
			if follow == nil {
				follow = f.followFile
			}
			go func() { defer close(done); follow(fctx, p) }()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// followFile decodes path until ctx is done, restarting ffmpeg (from the
// start of the file) with backoff if it fails.
func (f *frameFollower) followFile(ctx context.Context, path string) {
	f.logf("Following %s for the live view", filepath.Base(path))
	backoff := 5 * time.Second
	for {
		err := f.decode(ctx, path)
		if ctx.Err() != nil {
			return
		}
		f.logf("Live view decoder for %s stopped, restarting in %s: %v", filepath.Base(path), backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// decode runs one ffmpeg over path, fed by tailFile, and stores each JPEG
// it writes.
func (f *frameFollower) decode(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	select_ := fmt.Sprintf("select='isnan(prev_selected_t)+gte(t-prev_selected_t,%g)'", f.every.Seconds())
	cmd := exec.CommandContext(ctx, f.ffmpeg, //nolint:gosec // ffmpeg from PATH, arguments fixed
		"-hide_banner", "-loglevel", "error",
		// One thread for the decoder and for the encoder: frame threading
		// holds back the newest frames until more arrive, which on a static
		// screen (one frame a second) would delay the picture by seconds.
		"-threads", "1",
		"-i", "pipe:0",
		"-an", "-vf", select_, "-fps_mode", "passthrough",
		"-c:v", "mjpeg", "-q:v", "2", "-threads", "1",
		"-f", "image2pipe", "pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{b: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return err
	}

	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go func() {
		_ = tailFile(feedCtx, stdin, file, f.poll)
		_ = stdin.Close()
	}()
	readErr := readJPEGs(stdout, maxFrameBytes, f.set)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", waitErr, msg)
		}
		return waitErr
	}
	if readErr != nil {
		return readErr
	}
	return errors.New("ffmpeg exited")
}

// tailFile copies r to w and keeps copying what is appended to r, checking
// every poll at the end of the data, until ctx is done or w fails.
func tailFile(ctx context.Context, w io.Writer, r io.Reader, poll time.Duration) error {
	buf := make([]byte, 256<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// readJPEGs splits a stream of concatenated baseline/progressive JPEG
// images (ffmpeg -f image2pipe -c:v mjpeg) and calls emit with each one.
// It returns nil at a clean end of stream.
func readJPEGs(r io.Reader, maxBytes int, emit func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		img, err := readJPEG(br, maxBytes)
		if err != nil {
			if errors.Is(err, io.EOF) && len(img) == 0 {
				return nil
			}
			return err
		}
		emit(img)
	}
}

// readJPEG reads one JPEG image, from SOI to EOI, walking its marker
// segments so that 0xFFD9 inside a segment is not mistaken for EOI.
func readJPEG(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var img []byte
	next := func() (byte, error) {
		c, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if len(img) >= maxBytes {
			return 0, fmt.Errorf("JPEG larger than %d bytes", maxBytes)
		}
		img = append(img, c)
		return c, nil
	}
	a, err := next()
	if err != nil {
		return img, err
	}
	b, err := next()
	if err != nil || a != 0xFF || b != 0xD8 {
		if err == nil {
			err = fmt.Errorf("not a JPEG (starts %02x %02x)", a, b)
		}
		return img, unexpected(err)
	}
	haveFF := false // the entropy-data scan already consumed a marker's 0xFF
	for {
		// Marker: 0xFF, optional fill bytes, then the code.
		c := byte(0xFF)
		if !haveFF {
			if c, err = next(); err != nil {
				return img, unexpected(err)
			}
			if c != 0xFF {
				return img, fmt.Errorf("JPEG: expected a marker, got %02x", c)
			}
		}
		haveFF = false
		for c == 0xFF {
			if c, err = next(); err != nil {
				return img, unexpected(err)
			}
		}
		switch {
		case c == 0xD9: // EOI
			return img, nil
		case c == 0x01 || (c >= 0xD0 && c <= 0xD7): // no length
			continue
		}
		hi, err := next()
		if err != nil {
			return img, unexpected(err)
		}
		lo, err := next()
		if err != nil {
			return img, unexpected(err)
		}
		n := int(hi)<<8 | int(lo)
		if n < 2 {
			return img, fmt.Errorf("JPEG: bad segment length %d", n)
		}
		for range n - 2 {
			if _, err := next(); err != nil {
				return img, unexpected(err)
			}
		}
		if c != 0xDA { // not SOS
			continue
		}
		// Entropy-coded data: 0xFF is followed by 0x00 (stuffing), a
		// restart marker, or the next marker.
		for {
			d, err := next()
			if err != nil {
				return img, unexpected(err)
			}
			if d != 0xFF {
				continue
			}
			e, err := br.Peek(1)
			if err != nil {
				return img, unexpected(err)
			}
			if e[0] == 0x00 || (e[0] >= 0xD0 && e[0] <= 0xD7) {
				if _, err := next(); err != nil {
					return img, unexpected(err)
				}
				continue
			}
			// A marker follows; its 0xFF is already read.
			haveFF = true
			break
		}
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// limitedBuffer keeps the first n bytes written to it.
type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
