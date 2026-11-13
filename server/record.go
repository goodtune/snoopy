package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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

// GNOME Shell refuses Screencast and ScreencastArea while the
// org.gnome.desktop.lockdown key disable-save-to-disk is true (GDM sets it
// for its login screen; administrators can set it for users):
//
//	invocation.return_error_literal(ScreencastErrors,
//	    ScreencastError.SAVE_TO_DISK_DISABLED, 'Saving to disk is disabled');
//
// (js/dbusServices/screencast/screencastService.js, ScreencastAsync and
// ScreencastAreaAsync, unchanged from gnome-46 to main). js/misc/dbusErrors.js
// registers the domain with Gio.dbus_error_register_error as
// org.gnome.Shell.Screencast.Error.<CamelCase>, so SAVE_TO_DISK_DISABLED (2)
// arrives as the D-Bus error below with the message as its only body
// argument; godbus returns it as a dbus.Error whose Error() is the message.
// Were the domain ever left unregistered, GDBus would send the unmapped
// GError name instead (quark "shell-screencast-error", code 2).
const (
	saveToDiskDisabledName     = "org.gnome.Shell.Screencast.Error.SaveToDiskDisabled"
	saveToDiskDisabledUnmapped = "org.gtk.GDBus.UnmappedGError.Quark._shell_2dscreencast_2derror.Code2"
	saveToDiskDisabledMessage  = "Saving to disk is disabled"
)

// lockdownRetryDelay is how often the loop retries while the session's
// lockdown setting disables saving screencasts to disk.
const lockdownRetryDelay = 60 * time.Second

// isSaveToDiskDisabled reports whether err is GNOME Shell refusing to record
// because saving to disk is administratively disabled. That is a setting,
// not a transient fault, so the loop waits instead of exiting.
func isSaveToDiskDisabled(err error) bool {
	if err == nil {
		return false
	}
	var name string
	var de dbus.Error
	var dep *dbus.Error
	switch {
	case errors.As(err, &de):
		name = de.Name
	case errors.As(err, &dep) && dep != nil:
		name = dep.Name
	}
	if name == saveToDiskDisabledName || name == saveToDiskDisabledUnmapped {
		return true
	}
	return strings.Contains(err.Error(), saveToDiskDisabledMessage)
}

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
	// lockdownRetry is the wait between attempts while saving to disk is
	// disabled. Zero means lockdownRetryDelay; tests shorten it.
	lockdownRetry time.Duration
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
	retries       int   // consecutive failed (re)starts, for backoff
	lastErr       error // last start failure from restart
	// lockdown: GNOME Shell refuses to record because saving to disk is
	// disabled; retries are quiet and paced by cfg.lockdownRetry.
	lockdown bool

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
//
// If the shell refuses with "Saving to disk is disabled" (the session's
// org.gnome.desktop.lockdown disable-save-to-disk setting), at startup or
// later, the loop logs once and retries quietly every cfg.lockdownRetry
// instead of failing; it logs again when recording finally starts.
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
	if cfg.lockdownRetry <= 0 {
		cfg.lockdownRetry = lockdownRetryDelay
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
		switch {
		case isSaveToDiskDisabled(err):
			r.enterLockdown()
		case err != nil:
			return fmt.Errorf("start initial screencast: %w", err)
		default:
			log.Printf("Started initial recording: %s", file)
			r.recording(a, b, file)
		}
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
	firstWait := cfg.segment
	if r.lockdown {
		firstWait = cfg.lockdownRetry
	}
	segmentTimer := time.NewTimer(firstWait)
	defer segmentTimer.Stop()

	// retryLockdown quietly tries to start while saving to disk is disabled
	// and arms the timer for the next segment, the next lockdown retry, or
	// (if the shell now fails for another reason) a normal backoff retry.
	retryLockdown := func() error {
		sc, file, err := r.startQuiet()
		if err == nil {
			log.Printf("Saving to disk is allowed again; started recording: %s", file)
			other := r.b
			if sc == r.b {
				other = r.a
			}
			r.recording(sc, other, file)
			segmentTimer.Reset(cfg.segment)
			return nil
		}
		if perr := r.ping(); perr != nil {
			return perr
		}
		if isSaveToDiskDisabled(err) {
			segmentTimer.Reset(cfg.lockdownRetry)
			return nil
		}
		log.Printf("Start screencast failed: %v", err)
		r.lockdown = false
		r.retries++
		segmentTimer.Reset(r.backoff())
		return nil
	}

	// restart starts a fresh segment now and arms the timer for the next
	// rotation, or for a retry with backoff if no recording could start.
	restart := func(reason string) error {
		if r.lockdown && r.active == nil {
			return retryLockdown()
		}
		log.Printf("Starting a fresh segment: %s", reason)
		if r.restart(ctx) {
			segmentTimer.Reset(cfg.segment)
			return nil
		}
		if err := r.ping(); err != nil {
			return err
		}
		if isSaveToDiskDisabled(r.lastErr) {
			r.enterLockdown()
			segmentTimer.Reset(cfg.lockdownRetry)
			return nil
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
				if isSaveToDiskDisabled(err) {
					r.enterLockdown()
					segmentTimer.Reset(cfg.lockdownRetry)
					continue
				}
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
	r.lockdown = false
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
		r.lastErr = err
		return false
	}
	for _, sc := range []screencaster{r.a, r.b} {
		file, err := sc.Start(r.cfg.template)
		r.lastErr = err
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

// enterLockdown switches to quiet, slow retries after GNOME Shell refused to
// record because saving to disk is disabled, logging once.
func (r *recorder) enterLockdown() {
	if r.lockdown {
		return
	}
	r.lockdown = true
	log.Printf("Recording is disabled by this session's lockdown setting (org.gnome.desktop.lockdown disable-save-to-disk); retrying every %s", r.cfg.lockdownRetry)
}

// startQuiet tries to start a recording on either client without logging
// (used while saving to disk is disabled, so retries do not flood the log).
// A save-to-disk refusal is returned at once: the other client would get the
// same answer. Otherwise it returns the last error.
func (r *recorder) startQuiet() (screencaster, string, error) {
	var err error
	for _, sc := range []screencaster{r.a, r.b} {
		var file string
		file, err = sc.Start(r.cfg.template)
		if err == nil {
			return sc, file, nil
		}
		if isSaveToDiskDisabled(err) {
			return nil, "", err
		}
	}
	return nil, "", err
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
