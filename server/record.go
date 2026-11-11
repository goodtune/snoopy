package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// errConnectionLost is returned when the session bus goes away (for example
// when the session ends); systemd restarts the service.
var errConnectionLost = errors.New("DBus session connection lost")

// screencaster is one GNOME Shell screencast client (one DBus connection).
type screencaster interface {
	// Start begins a recording using the GNOME Shell filename template and
	// returns the file being written.
	Start(template string) (string, error)
	// Stop ends this client's recording.
	Stop() error
	// Ping checks that the underlying DBus connection is alive.
	Ping() error
}

const dbusCallTimeout = 10 * time.Second

type dbusScreencaster struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

func newDBusScreencaster(conn *dbus.Conn) *dbusScreencaster {
	return &dbusScreencaster{conn: conn, obj: conn.Object(dest, dbus.ObjectPath(objPath))}
}

func (d *dbusScreencaster) Start(template string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()
	var (
		ok   bool
		file string
	)
	err := d.obj.CallWithContext(ctx, startMethod, 0, template, map[string]dbus.Variant{}).Store(&ok, &file)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("GNOME Shell refused to start screencast")
	}
	return file, nil
}

func (d *dbusScreencaster) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()
	return d.obj.CallWithContext(ctx, stopMethod, 0).Err
}

func (d *dbusScreencaster) Ping() error {
	return checkDBusConnection(d.conn)
}

type loopConfig struct {
	template    string
	segment     time.Duration // length of each recording segment
	pause       time.Duration // settle time after stopping the previous segment
	healthCheck time.Duration // DBus ping (and stall check) interval
	retryDelay  time.Duration // longest wait between attempts to (re)start recording

	// recoverDelay is the first wait before retrying a failed restart after
	// unlock, resume, a screencast error or a stall; it doubles per attempt
	// up to retryDelay. Zero means 500ms.
	recoverDelay time.Duration
	// stallTimeout restarts the segment when its file has not changed for
	// this long while unlocked. Zero disables stall detection.
	stallTimeout time.Duration
	// events delivers lock/unlock/resume/screencast-error events; nil means
	// none (stall detection and the segment timer still apply).
	events sessionEvents
	// stat inspects the segment file; nil means os.Stat.
	stat func(string) (os.FileInfo, error)
}

// recorder is the record loop's state. Only active (if non-nil) may hold a
// recording; spare is idle and is where the next segment starts, because
// GNOME Shell allows one recording per D-Bus client.
type recorder struct {
	cfg           loopConfig
	a, b          screencaster
	active, spare screencaster
	file          string // file being written by active
	locked        bool
	retries       int // consecutive failed (re)starts, for backoff

	// Stall detection: last observed size/mtime of file and when it last
	// changed (or the segment started).
	lastSize   int64
	lastMod    time.Time
	lastChange time.Time
}

// recordLoop records consecutive segments, alternating between the two
// screencasters, until ctx is cancelled (returns nil) or the DBus connection
// is lost (returns an error wrapping errConnectionLost). The active recording
// is always stopped before it returns.
//
// Besides the segment timer it reacts to session events: while the screen
// is locked GNOME Shell kills screencasts and refuses new ones, so the loop
// waits, and on unlock or resume starts a fresh segment at once (retrying
// with backoff while the shell is not ready). A stalled segment file
// (cfg.stallTimeout) also triggers a restart, as a fallback for anything the
// signals miss.
func recordLoop(ctx context.Context, cfg loopConfig, a, b screencaster) error {
	if cfg.stat == nil {
		cfg.stat = os.Stat
	}
	if cfg.recoverDelay <= 0 {
		cfg.recoverDelay = 500 * time.Millisecond
	}
	if cfg.retryDelay <= 0 {
		cfg.retryDelay = cfg.recoverDelay
	}
	r := &recorder{cfg: cfg, a: a, b: b, spare: b}

	var events <-chan sessionEvent
	if cfg.events != nil {
		events = cfg.events.Events()
		r.locked = cfg.events.Locked()
	}

	if r.locked {
		log.Printf("Session is locked; recording will start when it is unlocked")
	} else {
		file, err := a.Start(cfg.template)
		if err != nil {
			return fmt.Errorf("start initial screencast: %w", err)
		}
		log.Printf("Started initial recording: %s", file)
		r.recording(a, b, file)
	}

	defer func() {
		if r.active == nil {
			return
		}
		if err := r.active.Stop(); err != nil {
			log.Printf("Stop screencast on exit failed: %v", err)
		}
	}()

	healthCheckTicker := time.NewTicker(cfg.healthCheck)
	defer healthCheckTicker.Stop()

	// The segment timer is created once and re-armed after each rotation; it
	// must not be recreated per select iteration, or every health-check tick
	// would restart the segment countdown. While nothing is recording it
	// paces the restart attempts instead.
	segmentTimer := time.NewTimer(cfg.segment)
	defer segmentTimer.Stop()

	// restart starts a fresh segment now and arms the timer for the next
	// rotation, or for a retry with backoff if no recording could start.
	restart := func(reason string) error {
		log.Printf("Starting a fresh segment: %s", reason)
		if r.restart(ctx) {
			segmentTimer.Reset(cfg.segment)
			return nil
		}
		if err := r.ping(); err != nil {
			return err
		}
		segmentTimer.Reset(r.backoff())
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Received shutdown signal, cleaning up...")
			return nil

		case <-healthCheckTicker.C:
			if err := r.ping(); err != nil {
				log.Printf("DBus session connection lost")
				return err
			}
			if reason := r.stalled(time.Now()); reason != "" {
				if err := restart(reason); err != nil {
					return err
				}
			}

		case ev, ok := <-events:
			if !ok {
				log.Printf("Session events unavailable; relying on stall detection (-stall-timeout)")
				events = nil
				continue
			}
			switch ev {
			case evLocked:
				if !r.locked {
					log.Printf("Session locked: GNOME Shell stops screencasts until unlock")
				}
				r.locked = true
			case evUnlocked:
				r.locked = false
				if err := restart("session unlocked"); err != nil {
					return err
				}
			case evResumed:
				if r.locked {
					log.Printf("Resumed from suspend while locked; waiting for unlock")
					continue
				}
				if err := restart("resumed from suspend"); err != nil {
					return err
				}
			case evScreencastError:
				if r.locked {
					// Expected: locking killed the recording. Clean up the
					// dead client; unlock starts the next segment.
					r.discard()
					continue
				}
				if err := restart("GNOME Shell reported a screencast error"); err != nil {
					return err
				}
			}

		case <-segmentTimer.C:
			if r.active == nil {
				if r.locked {
					// Starting is refused while locked; unlock restarts.
					segmentTimer.Reset(cfg.segment)
					continue
				}
				if err := restart("retrying after failed start"); err != nil {
					return err
				}
				continue
			}

			next, file, err := rotate(ctx, r.active, r.spare, cfg.template, cfg.pause)
			if err != nil {
				log.Printf("Segment rotation failed, no recording active: %v", err)
				if perr := r.ping(); perr != nil {
					log.Printf("DBus connection lost during segment rotation")
					return perr
				}
				r.active = nil
				if r.locked {
					segmentTimer.Reset(cfg.segment)
				} else {
					segmentTimer.Reset(r.backoff())
				}
				continue
			}
			r.rotated(next, file)
			segmentTimer.Reset(cfg.segment)
		}
	}
}

// recording records that sc is now writing file and other is idle.
func (r *recorder) recording(sc, other screencaster, file string) {
	r.active, r.spare, r.file = sc, other, file
	r.retries = 0
	r.lastSize, r.lastMod, r.lastChange = -1, time.Time{}, time.Now()
}

// rotated records the result of a successful rotate.
func (r *recorder) rotated(next screencaster, file string) {
	other := r.a
	if next == r.a {
		other = r.b
	}
	r.recording(next, other, file)
}

// restart replaces the current recording (if any) with a fresh segment and
// reports whether one is now recording. With a recording believed active it
// rotates (overlap, then stop the old one; its Stop failing is expected when
// the shell already killed it). Otherwise it starts on either client.
func (r *recorder) restart(ctx context.Context) bool {
	if r.active != nil {
		next, file, err := rotate(ctx, r.active, r.spare, r.cfg.template, r.cfg.pause)
		if err == nil {
			r.rotated(next, file)
			return true
		}
		log.Printf("Restart failed: %v", err)
		r.active = nil
		r.retries++
		return false
	}
	for _, sc := range []screencaster{r.a, r.b} {
		file, err := sc.Start(r.cfg.template)
		if err == nil {
			log.Printf("Started recording: %s", file)
			other := r.b
			if sc == r.b {
				other = r.a
			}
			r.recording(sc, other, file)
			return true
		}
		log.Printf("Start screencast failed: %v", err)
	}
	r.retries++
	return false
}

// discard stops the active client (errors ignored: the shell has usually
// already dropped the recording) and forgets it.
func (r *recorder) discard() {
	if r.active == nil {
		return
	}
	if err := r.active.Stop(); err != nil {
		log.Printf("Stop of interrupted screencast failed (expected): %v", err)
	}
	r.spare = r.b
	if r.active == r.b {
		r.spare = r.a
	}
	r.active = nil
}

// backoff returns the delay before the next restart attempt: recoverDelay
// doubled per consecutive failure, capped at retryDelay.
func (r *recorder) backoff() time.Duration {
	d := r.cfg.recoverDelay
	for i := 1; i < r.retries && d < r.cfg.retryDelay; i++ {
		d *= 2
	}
	return min(d, r.cfg.retryDelay)
}

func (r *recorder) ping() error {
	for _, sc := range []screencaster{r.a, r.b} {
		if err := sc.Ping(); err != nil {
			return fmt.Errorf("%w: %v", errConnectionLost, err)
		}
	}
	return nil
}

// stalled reports why the active segment should be restarted, or "".
//
// GNOME Shell's pipeline is damage driven, but pipewiresrc runs with
// keepalive-time=1000 (screencastService.js), so even a static screen yields
// about one frame per second. Those frames are tiny, though, and the muxer
// and filesink buffer output, so a file on a static screen can go minutes
// without a visible size or mtime change. A false positive costs only an
// extra (overlapping) segment file, while a missed stall loses recording,
// so the default timeout (5m) errs on the side of restarting; it fires at
// most once per timeout because a restart resets the clock. Checks run on
// the health-check ticker, and never while locked (nothing can record).
func (r *recorder) stalled(now time.Time) string {
	if r.cfg.stallTimeout <= 0 || r.locked || r.active == nil {
		return ""
	}
	fi, err := r.cfg.stat(r.file)
	if err == nil && (fi.Size() != r.lastSize || !fi.ModTime().Equal(r.lastMod)) {
		r.lastSize, r.lastMod, r.lastChange = fi.Size(), fi.ModTime(), now
		return ""
	}
	if now.Sub(r.lastChange) < r.cfg.stallTimeout {
		return ""
	}
	r.lastChange = now // at most one restart per stallTimeout, even if it fails
	if err != nil {
		return fmt.Sprintf("segment file unreadable for %s (%v)", r.cfg.stallTimeout, err)
	}
	return fmt.Sprintf("segment file %s unchanged for %s", r.file, r.cfg.stallTimeout)
}

// rotate starts the next segment on spare BEFORE stopping cur so coverage
// overlaps, then pauses briefly for cleanup. If spare cannot start, it falls
// back to stopping cur and restarting on it (a gap of roughly pause). It
// returns the screencaster now recording and its file, or an error if none
// is recording.
func rotate(ctx context.Context, cur, spare screencaster, template string, pause time.Duration) (screencaster, string, error) {
	file, err := spare.Start(template)
	if err == nil {
		log.Printf("Started next recording: %s", file)
		if err := cur.Stop(); err != nil {
			log.Printf("Stop previous screencast failed (may already be stopped): %v", err)
		}
		sleepCtx(ctx, pause)
		return spare, file, nil
	}
	log.Printf("Start next screencast failed, falling back to stop/start: %v", err)

	if err := cur.Stop(); err != nil {
		log.Printf("Stop screencast failed: %v", err)
	}
	sleepCtx(ctx, pause)
	file, err = cur.Start(template)
	if err != nil {
		return nil, "", err
	}
	log.Printf("Started next recording: %s", file)
	return cur, file, nil
}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
