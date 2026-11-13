package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// errSaveToDiskDisabled is the reply GNOME Shell sends while
// org.gnome.desktop.lockdown disable-save-to-disk is set, as godbus
// surfaces it.
var errSaveToDiskDisabled = dbus.Error{Name: saveToDiskDisabledName, Body: []any{saveToDiskDisabledMessage}}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), substr)
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

const lockdownLine = "disabled by this session's lockdown setting"

func TestIsSaveToDiskDisabled(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dbus error value", errSaveToDiskDisabled, true},
		{"dbus error pointer", &dbus.Error{Name: saveToDiskDisabledName, Body: []any{saveToDiskDisabledMessage}}, true},
		{"wrapped", fmt.Errorf("start initial screencast: %w", errSaveToDiskDisabled), true},
		{"name without body", dbus.Error{Name: saveToDiskDisabledName}, true},
		{"unmapped GError", dbus.Error{Name: saveToDiskDisabledUnmapped, Body: []any{"GDBus.Error: Saving to disk is disabled"}}, true},
		{"message only", errors.New(saveToDiskDisabledMessage), true},
		{"already recording", dbus.Error{Name: "org.gnome.Shell.Screencast.Error.AlreadyRecording", Body: []any{"Service is already recording"}}, false},
		{"inhibited", errors.New("Session creation inhibited"), false},
		{"timeout", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSaveToDiskDisabled(tt.err); got != tt.want {
				t.Errorf("isSaveToDiskDisabled(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRecordLoopInitialSaveToDiskDisabledWaits(t *testing.T) {
	logs := captureLog(t)
	a, b, _ := newFakePair()
	a.setStartErr(errSaveToDiskDisabled)
	b.setStartErr(errSaveToDiskDisabled)
	const (
		segment = 300 * time.Millisecond
		retry   = 30 * time.Millisecond
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- recordLoop(ctx, loopConfig{
			segment:       segment,
			healthCheck:   5 * time.Millisecond,
			retryDelay:    5 * time.Millisecond,
			recoverDelay:  time.Millisecond,
			lockdownRetry: retry,
		}, a, b)
	}()

	waitFor(t, time.Second, "lockdown retries", func() bool { return a.startAttempts() >= 4 })
	select {
	case err := <-done:
		t.Fatalf("recordLoop returned %v while saving to disk is disabled", err)
	default:
	}
	// Paced by lockdownRetry, not the 5ms retry/health-check intervals, and
	// the second client is not asked (it would get the same answer).
	before := a.startAttempts()
	time.Sleep(100 * time.Millisecond)
	if n := a.startAttempts() - before; n > 5 {
		t.Errorf("%d attempts in 100ms with a %s lockdown retry", n, retry)
	}
	if n := b.startAttempts(); n != 0 {
		t.Errorf("second client asked %d times during lockdown", n)
	}

	a.setStartErr(nil)
	b.setStartErr(nil)
	waitFor(t, time.Second, "recording once allowed", func() bool { return recordingCount(a, b) == 1 })
	startedAt := time.Now()
	// The segment timer was re-armed when recording started.
	time.Sleep(segment / 2)
	if got := totalStarts(a, b); got != 1 {
		t.Fatalf("starts = %d at segment/2 after recovery, want 1", got)
	}
	waitFor(t, 2*time.Second, "rotation one segment later", func() bool { return totalStarts(a, b) == 2 })
	if d := time.Since(startedAt); d < segment*3/4 {
		t.Errorf("rotated %s after recovery, want about %s", d, segment)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordLoop returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recordLoop did not return")
	}
	if n := logs.count(lockdownLine); n != 1 {
		t.Errorf("lockdown line logged %d times, want 1", n)
	}
	if n := logs.count("Saving to disk is allowed again"); n != 1 {
		t.Errorf("recovery line logged %d times, want 1", n)
	}
	if n := logs.count("Start screencast failed"); n != 0 {
		t.Errorf("retries logged %d failures, want 0", n)
	}
}

func TestRecordLoopSaveToDiskDisabledShutdownIsPrompt(t *testing.T) {
	a, b, _ := newFakePair()
	a.setStartErr(errSaveToDiskDisabled)
	b.setStartErr(errSaveToDiskDisabled)
	stop := runLoop(t, loopConfig{segment: time.Hour, healthCheck: time.Hour, lockdownRetry: time.Hour}, a, b)
	waitFor(t, time.Second, "initial attempt", func() bool { return a.startAttempts() == 1 })
	begin := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v, want nil", err)
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("shutdown took %s while waiting", d)
	}
}

func TestRecordLoopSaveToDiskDisabledOnUnlock(t *testing.T) {
	logs := captureLog(t)
	a, b, _ := newFakePair()
	ev := newFakeEvents(false)
	stop := runLoop(t, loopConfig{
		segment:       time.Hour,
		healthCheck:   5 * time.Millisecond,
		retryDelay:    5 * time.Millisecond,
		recoverDelay:  time.Millisecond,
		lockdownRetry: 30 * time.Millisecond,
		events:        ev,
	}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })

	// Locked, and the administrator turned the lockdown key on meanwhile.
	lockShell(ev, errSaveToDiskDisabled, a, b)
	ev.ch <- evUnlocked
	waitFor(t, time.Second, "lockdown state", func() bool { return logs.count(lockdownLine) == 1 })
	before := a.startAttempts() + b.startAttempts()
	time.Sleep(100 * time.Millisecond)
	if n := a.startAttempts() + b.startAttempts() - before; n > 5 {
		t.Errorf("%d attempts in 100ms during lockdown", n)
	}
	if n := recordingCount(a, b); n != 0 {
		t.Fatalf("%d recording during lockdown", n)
	}

	// Another unlock while waiting retries quietly, without a second line.
	ev.ch <- evLocked
	ev.ch <- evUnlocked
	a.setStartErr(nil)
	b.setStartErr(nil)
	waitFor(t, time.Second, "recording once allowed", func() bool { return recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
	if n := logs.count(lockdownLine); n != 1 {
		t.Errorf("lockdown line logged %d times, want 1", n)
	}
}

func TestRecordLoopSaveToDiskDisabledOnRotation(t *testing.T) {
	logs := captureLog(t)
	a, b, _ := newFakePair()
	stop := runLoop(t, loopConfig{
		segment:       30 * time.Millisecond,
		healthCheck:   time.Hour,
		retryDelay:    5 * time.Millisecond,
		recoverDelay:  time.Millisecond,
		lockdownRetry: 40 * time.Millisecond,
	}, a, b)
	waitFor(t, time.Second, "initial recording", func() bool { return recordingCount(a, b) == 1 })
	a.setStartErr(errSaveToDiskDisabled)
	b.setStartErr(errSaveToDiskDisabled)
	waitFor(t, time.Second, "lockdown state", func() bool { return logs.count(lockdownLine) == 1 })
	waitFor(t, time.Second, "rotation stopped the old segment", func() bool { return recordingCount(a, b) == 0 })

	before := a.startAttempts() + b.startAttempts()
	time.Sleep(200 * time.Millisecond)
	if n := a.startAttempts() + b.startAttempts() - before; n > 7 {
		t.Errorf("%d attempts in 200ms with a 40ms lockdown retry", n)
	}
	a.setStartErr(nil)
	b.setStartErr(nil)
	waitFor(t, time.Second, "recording once allowed", func() bool { return recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
	if n := logs.count(lockdownLine); n != 1 {
		t.Errorf("lockdown line logged %d times, want 1", n)
	}
}

func TestRecordLoopLockdownThenOtherErrorBacksOff(t *testing.T) {
	a, b, _ := newFakePair()
	a.setStartErr(errSaveToDiskDisabled)
	b.setStartErr(errSaveToDiskDisabled)
	stop := runLoop(t, loopConfig{
		segment:       time.Hour,
		healthCheck:   time.Hour,
		retryDelay:    10 * time.Millisecond,
		recoverDelay:  time.Millisecond,
		lockdownRetry: 20 * time.Millisecond,
	}, a, b)
	waitFor(t, time.Second, "lockdown retry", func() bool { return a.startAttempts() >= 2 })
	// The key was turned off but the shell is not ready yet: normal backoff
	// (both clients tried), then recording.
	notReady := errors.New("not ready")
	a.setStartErr(notReady)
	b.setStartErr(notReady)
	waitFor(t, time.Second, "normal retries", func() bool { return b.startAttempts() >= 2 })
	a.setStartErr(nil)
	b.setStartErr(nil)
	waitFor(t, time.Second, "recording", func() bool { return recordingCount(a, b) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("recordLoop returned %v", err)
	}
}
