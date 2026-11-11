package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeEvents is a scripted sessionEvents source.
type fakeEvents struct {
	locked bool
	ch     chan sessionEvent
}

func newFakeEvents(locked bool) *fakeEvents {
	return &fakeEvents{locked: locked, ch: make(chan sessionEvent, 8)}
}

func (f *fakeEvents) Locked() bool                { return f.locked }
func (f *fakeEvents) Events() <-chan sessionEvent { return f.ch }

// fakeInfo is a minimal os.FileInfo for stall tests.
type fakeInfo struct {
	size int64
	mod  time.Time
}

func (f fakeInfo) Name() string       { return "seg.webm" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeInfo) ModTime() time.Time { return f.mod }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// constStat reports an unchanging file and counts calls.
func constStat(calls *atomic.Int32) func(string) (os.FileInfo, error) {
	mod := time.Now()
	return func(string) (os.FileInfo, error) {
		calls.Add(1)
		return fakeInfo{size: 1000, mod: mod}, nil
	}
}

// runLoop runs recordLoop in the background; stop cancels it and returns its
// result.
func runLoop(t *testing.T, cfg loopConfig, a, b screencaster) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- recordLoop(ctx, cfg, a, b) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				err = errors.New("recordLoop did not return")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func totalStarts(fs ...*fakeScreencaster) int {
	n := 0
	for _, f := range fs {
		s, _, _ := f.counts()
		n += s
	}
	return n
}

func recordingCount(fs ...*fakeScreencaster) int {
	n := 0
	for _, f := range fs {
		if _, _, rec := f.counts(); rec {
			n++
		}
	}
	return n
}

// lockShell simulates locking: the shell kills the recording, refuses new
// ones and emits Error after ActiveChanged(true).
func lockShell(ev *fakeEvents, refuse error, fakes ...*fakeScreencaster) {
	ev.ch <- evLocked
	for _, f := range fakes {
		f.setStartErr(refuse)
		if _, _, rec := f.counts(); rec {
			f.kill()
		}
	}
	ev.ch <- evScreencastError
}

func TestRecordLoopLockUnlockRestartsAndRearmsTimer(t *testing.T) {
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	const segment = 400 * time.Millisecond
	start := time.Now()
	stop := runLoop(t, loopConfig{
		segment:      segment,
		healthCheck:  time.Hour,
		retryDelay:   50 * time.Millisecond,
		recoverDelay: 5 * time.Millisecond,
		events:       ev,
	}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })

	refused := errors.New("Session creation inhibited")
	lockShell(ev, refused, a, b)
	waitFor(t, time.Second, "dead client stopped", func() bool { _, stops, _ := a.counts(); return stops == 1 })

	// While locked nothing is attempted.
	attempts := a.startAttempts() + b.startAttempts()
	time.Sleep(100 * time.Millisecond)
	if got := a.startAttempts() + b.startAttempts(); got != attempts {
		t.Fatalf("start attempted while locked: %d -> %d", attempts, got)
	}

	// Unlock at ~300ms, before the original timer (400ms) would fire.
	time.Sleep(time.Until(start.Add(300 * time.Millisecond)))
	a.setStartErr(nil)
	b.setStartErr(nil)
	unlockedAt := time.Now()
	ev.ch <- evUnlocked
	waitFor(t, 200*time.Millisecond, "recording after unlock", func() bool { return recordingCount(a, b) == 1 })
	if d := time.Since(unlockedAt); d > 100*time.Millisecond {
		t.Errorf("restart took %s after unlock", d)
	}
	if got := totalStarts(a, b); got != 2 {
		t.Fatalf("starts = %d, want 2 (initial + unlock)", got)
	}

	// The timer was re-armed at unlock: no rotation at the original 400ms.
	time.Sleep(time.Until(unlockedAt.Add(segment / 2)))
	if got := totalStarts(a, b); got != 2 {
		t.Fatalf("starts = %d at unlock+segment/2, want 2 (timer not re-armed)", got)
	}
	waitFor(t, 2*time.Second, "rotation one segment after unlock", func() bool { return totalStarts(a, b) == 3 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
	if n := recordingCount(a, b); n != 0 {
		t.Errorf("%d clients still recording after exit", n)
	}
}

func TestRecordLoopStartWhileLockedWaits(t *testing.T) {
	a, b, _ := newFakePair()
	refused := errors.New("Session creation inhibited")
	a.setStartErr(refused)
	b.setStartErr(refused)
	ev := newFakeEvents(true)
	stop := runLoop(t, loopConfig{
		segment:      20 * time.Millisecond, // timer fires repeatedly while locked
		healthCheck:  5 * time.Millisecond,
		retryDelay:   10 * time.Millisecond,
		recoverDelay: time.Millisecond,
		events:       ev,
	}, a, b)

	time.Sleep(100 * time.Millisecond)
	if got := a.startAttempts() + b.startAttempts(); got != 0 {
		t.Fatalf("%d start attempts while locked, want 0", got)
	}

	a.setStartErr(nil)
	b.setStartErr(nil)
	ev.ch <- evUnlocked
	waitFor(t, time.Second, "recording after unlock", func() bool { return recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v, want nil", err)
	}
}

func TestRecordLoopUnlockRetriesUntilShellReady(t *testing.T) {
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	stop := runLoop(t, loopConfig{
		segment:      time.Hour,
		healthCheck:  time.Hour,
		retryDelay:   40 * time.Millisecond,
		recoverDelay: 5 * time.Millisecond,
		events:       ev,
	}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })

	notReady := errors.New("not ready")
	lockShell(ev, notReady, a, b)
	ev.ch <- evUnlocked // shell still refusing right after unlock
	waitFor(t, time.Second, "retries", func() bool { return a.startAttempts()+b.startAttempts() >= 5 })
	if n := recordingCount(a, b); n != 0 {
		t.Fatalf("recording while shell refuses: %d", n)
	}
	a.setStartErr(nil)
	b.setStartErr(nil)
	// Backoff is capped at retryDelay, so recovery is quick.
	waitFor(t, 500*time.Millisecond, "recording once the shell is ready", func() bool { return recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}

func TestRecordLoopResume(t *testing.T) {
	a, b, events := newFakePair()
	ev := newFakeEvents(false)
	stop := runLoop(t, loopConfig{segment: time.Hour, healthCheck: time.Hour, events: ev}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })

	ev.ch <- evResumed
	waitFor(t, time.Second, "fresh segment on resume", func() bool { return totalStarts(a, b) == 2 })
	if got := events(); len(got) < 3 || got[1] != "b:start" || got[2] != "a:stop" {
		t.Errorf("resume should rotate with overlap, events = %v", got)
	}

	// Resume while locked waits for unlock.
	ev.ch <- evLocked
	ev.ch <- evResumed
	time.Sleep(50 * time.Millisecond)
	if got := totalStarts(a, b); got != 2 {
		t.Errorf("starts = %d after resume while locked, want 2", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}

func TestRecordLoopScreencastErrorWhileUnlockedRestarts(t *testing.T) {
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	stop := runLoop(t, loopConfig{segment: time.Hour, healthCheck: time.Hour, events: ev}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })

	a.kill() // e.g. pipeline error or out of disk space
	ev.ch <- evScreencastError
	waitFor(t, time.Second, "restart after error", func() bool { return totalStarts(a, b) == 2 && recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}

func TestRecordLoopEventsClosed(t *testing.T) {
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	close(ev.ch)
	stop := runLoop(t, loopConfig{segment: 30 * time.Millisecond, healthCheck: time.Hour, events: ev}, a, b)
	waitFor(t, time.Second, "rotation continues", func() bool { return totalStarts(a, b) >= 3 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}

func TestRecordLoopStallRestartsOnce(t *testing.T) {
	a, b, _ := newFakePair()
	var calls atomic.Int32
	stop := runLoop(t, loopConfig{
		segment:      time.Hour,
		healthCheck:  5 * time.Millisecond,
		stallTimeout: 200 * time.Millisecond,
		stat:         constStat(&calls),
	}, a, b)

	waitFor(t, time.Second, "stall restart", func() bool { return totalStarts(a, b) == 2 })
	// Not again until another full stall timeout has passed.
	time.Sleep(120 * time.Millisecond)
	if got := totalStarts(a, b); got != 2 {
		t.Fatalf("starts = %d within one stall timeout of the restart, want 2", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
	if calls.Load() == 0 {
		t.Error("stat never called")
	}
}

func TestRecordLoopGrowingFileNoStall(t *testing.T) {
	a, b, _ := newFakePair()
	var size atomic.Int64
	stop := runLoop(t, loopConfig{
		segment:      time.Hour,
		healthCheck:  5 * time.Millisecond,
		stallTimeout: 30 * time.Millisecond,
		stat: func(string) (os.FileInfo, error) {
			return fakeInfo{size: size.Add(10)}, nil
		},
	}, a, b)
	time.Sleep(150 * time.Millisecond)
	if got := totalStarts(a, b); got != 1 {
		t.Fatalf("starts = %d with a growing file, want 1", got)
	}
	_ = stop()
}

func TestRecordLoopStallDisabled(t *testing.T) {
	a, b, _ := newFakePair()
	var calls atomic.Int32
	stop := runLoop(t, loopConfig{
		segment:      time.Hour,
		healthCheck:  5 * time.Millisecond,
		stallTimeout: 0,
		stat:         constStat(&calls),
	}, a, b)
	time.Sleep(100 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := totalStarts(a, b); got != 1 {
		t.Errorf("starts = %d with stall detection disabled, want 1", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("stat called %d times with stall detection disabled", n)
	}
}

func TestRecordLoopLockedSuppressesStall(t *testing.T) {
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	var calls atomic.Int32
	stop := runLoop(t, loopConfig{
		segment:      time.Hour,
		healthCheck:  5 * time.Millisecond,
		stallTimeout: 30 * time.Millisecond,
		stat:         constStat(&calls),
		events:       ev,
	}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })
	ev.ch <- evLocked // shield up; recording not (yet) reported dead
	time.Sleep(150 * time.Millisecond)
	if got := totalStarts(a, b); got != 1 {
		t.Fatalf("starts = %d while locked, want 1 (stall suppressed)", got)
	}
	_ = stop()
}

func TestBackoff(t *testing.T) {
	r := &recorder{cfg: loopConfig{recoverDelay: 500 * time.Millisecond, retryDelay: 5 * time.Second}}
	want := []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		r.retries = i
		if got := r.backoff(); got != w {
			t.Errorf("retries=%d: backoff = %s, want %s", i, got, w)
		}
	}
}

func TestTranslateSignal(t *testing.T) {
	tests := []struct {
		name string
		sig  *dbus.Signal
		want sessionEvent
		ok   bool
	}{
		{"lock", &dbus.Signal{Name: "org.gnome.ScreenSaver.ActiveChanged", Body: []any{true}}, evLocked, true},
		{"unlock", &dbus.Signal{Name: "org.gnome.ScreenSaver.ActiveChanged", Body: []any{false}}, evUnlocked, true},
		{"bad body", &dbus.Signal{Name: "org.gnome.ScreenSaver.ActiveChanged", Body: []any{"x"}}, 0, false},
		{"error", &dbus.Signal{Name: "org.gnome.Shell.Screencast.Error", Body: []any{"org.gnome.Shell.Screencast.Error.RecorderError", "Session closed unexpectedly"}}, evScreencastError, true},
		{"sleeping", &dbus.Signal{Name: "org.freedesktop.login1.Manager.PrepareForSleep", Body: []any{true}}, 0, false},
		{"resumed", &dbus.Signal{Name: "org.freedesktop.login1.Manager.PrepareForSleep", Body: []any{false}}, evResumed, true},
		{"other", &dbus.Signal{Name: "org.freedesktop.DBus.NameAcquired", Body: []any{":1.2"}}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := translateSignal(tt.sig)
			if got != tt.want || ok != tt.ok {
				t.Errorf("translateSignal = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
