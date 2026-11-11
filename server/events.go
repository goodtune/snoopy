package main

import (
	"context"
	"fmt"
	"log"

	"github.com/godbus/dbus/v5"
)

// sessionEvent is a desktop state change that affects screencasting.
type sessionEvent int

const (
	// evLocked: the screen shield came up (org.gnome.ScreenSaver
	// ActiveChanged(true)).
	evLocked sessionEvent = iota + 1
	// evUnlocked: the screen shield went away (ActiveChanged(false)).
	evUnlocked
	// evResumed: the machine woke from suspend (login1 PrepareForSleep(false)).
	evResumed
	// evScreencastError: GNOME Shell's screencast service emitted Error, i.e.
	// a recording ended without StopScreencast.
	evScreencastError
)

func (e sessionEvent) String() string {
	switch e {
	case evLocked:
		return "locked"
	case evUnlocked:
		return "unlocked"
	case evResumed:
		return "resumed"
	case evScreencastError:
		return "screencast error"
	}
	return fmt.Sprintf("sessionEvent(%d)", int(e))
}

// sessionEvents delivers lock, unlock, resume and screencast-error events to
// the record loop. The production implementation listens on D-Bus; tests use
// a fake, so the loop never needs a live GNOME session.
type sessionEvents interface {
	// Locked reports whether the session was locked when the source was
	// created (queried after subscribing, so no transition is missed).
	Locked() bool
	// Events returns the event channel. It is closed if the source dies;
	// the loop then carries on with stall detection only.
	Events() <-chan sessionEvent
}

// Why these signals (GNOME Shell / Mutter main, unchanged since gnome-46):
//
//   - Locking switches the shell to the "unlock-dialog" session mode, which
//     inherits allowScreencast: false from the "restrictive" defaults
//     (js/ui/sessionMode.js). js/ui/main.js _sessionUpdated() then calls
//     remoteAccessController.inhibit_remote_access(); Mutter closes every
//     screencast session and refuses new ones with "Session creation
//     inhibited" until uninhibited on unlock
//     (src/backends/meta-dbus-session-manager.c, meta_dbus_session_manager_inhibit
//     and the inhibit_count check in the create-session path).
//   - The closed Mutter session makes the shell's recorder bail out ("Session
//     closed unexpectedly"); the service drops it and emits the broadcast
//     signal Error(s name, s message)
//     (js/dbusServices/screencast/screencastService.js, recorder 'error'
//     handler; data/dbus-interfaces/org.gnome.Shell.Screencast.xml). A later
//     StopScreencast from that client returns false (no recorder), and
//     Screencast fails while the inhibit lasts.
//   - The screen shield publishes org.gnome.ScreenSaver ActiveChanged(b) and
//     GetActive() at /org/gnome/ScreenSaver on the session bus
//     (js/ui/shellDBus.js ScreenSaverDBus). "Active" also covers a blanked
//     but unlocked screen, so the loop never assumes the recording died on
//     ActiveChanged(true) alone.
//   - logind emits PrepareForSleep(false) on the system bus after resume.
const (
	screenSaverDest   = "org.gnome.ScreenSaver"
	screenSaverPath   = "/org/gnome/ScreenSaver"
	screenSaverIface  = "org.gnome.ScreenSaver"
	login1Dest        = "org.freedesktop.login1"
	login1Path        = "/org/freedesktop/login1"
	login1ManagerIfce = "org.freedesktop.login1.Manager"
)

type dbusSessionEvents struct {
	locked bool
	ch     chan sessionEvent
}

func (d *dbusSessionEvents) Locked() bool                { return d.locked }
func (d *dbusSessionEvents) Events() <-chan sessionEvent { return d.ch }

// newDBusSessionEvents subscribes to the lock, screencast-error and resume
// signals on dedicated bus connections, which are closed when ctx is done.
// It fails only if the session-bus subscription fails; a missing system bus
// (no resume events) is logged and tolerated.
//
// Each match rule names the well-known sender, so the bus only delivers the
// signal when it comes from the current owner of that name: another
// process in the session cannot fake a lock to pause recording.
func newDBusSessionEvents(ctx context.Context) (*dbusSessionEvents, error) {
	session, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	sessionSig := make(chan *dbus.Signal, 16)
	session.Signal(sessionSig)
	for _, m := range [][]dbus.MatchOption{
		{dbus.WithMatchSender(screenSaverDest), dbus.WithMatchObjectPath(screenSaverPath), dbus.WithMatchInterface(screenSaverIface), dbus.WithMatchMember("ActiveChanged")},
		{dbus.WithMatchSender(dest), dbus.WithMatchObjectPath(objPath), dbus.WithMatchInterface(iface), dbus.WithMatchMember("Error")},
	} {
		if err := session.AddMatchSignal(m...); err != nil {
			_ = session.Close()
			return nil, fmt.Errorf("subscribe to session signals: %w", err)
		}
	}

	d := &dbusSessionEvents{ch: make(chan sessionEvent, 16)}

	callCtx, cancel := context.WithTimeout(ctx, dbusCallTimeout)
	err = session.Object(screenSaverDest, screenSaverPath).
		CallWithContext(callCtx, screenSaverIface+".GetActive", 0).Store(&d.locked)
	cancel()
	if err != nil {
		// The shield may not be up yet; assume unlocked and rely on signals.
		log.Printf("Session events: GetActive failed, assuming unlocked: %v", err)
		d.locked = false
	}

	var systemSig chan *dbus.Signal
	system, err := dbus.ConnectSystemBus()
	if err == nil {
		systemSig = make(chan *dbus.Signal, 4)
		system.Signal(systemSig)
		err = system.AddMatchSignal(dbus.WithMatchSender(login1Dest), dbus.WithMatchObjectPath(login1Path),
			dbus.WithMatchInterface(login1ManagerIfce), dbus.WithMatchMember("PrepareForSleep"))
		if err != nil {
			_ = system.Close()
			system, systemSig = nil, nil
		}
	}
	if err != nil {
		log.Printf("Session events: no resume notifications (system bus): %v", err)
	}

	go func() {
		defer close(d.ch)
		defer func() { _ = session.Close() }()
		if system != nil {
			defer func() { _ = system.Close() }()
		}
		for {
			var sig *dbus.Signal
			var ok bool
			select {
			case <-ctx.Done():
				return
			case sig, ok = <-sessionSig:
				if !ok {
					log.Printf("Session events: session bus connection closed")
					return
				}
			case sig, ok = <-systemSig:
				if !ok {
					systemSig = nil // keep lock events; resume notifications stop
					continue
				}
			}
			ev, ok := translateSignal(sig)
			if !ok {
				continue
			}
			select {
			case d.ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return d, nil
}

// translateSignal maps a D-Bus signal to a sessionEvent.
func translateSignal(sig *dbus.Signal) (sessionEvent, bool) {
	switch sig.Name {
	case screenSaverIface + ".ActiveChanged":
		if len(sig.Body) == 1 {
			if active, ok := sig.Body[0].(bool); ok {
				if active {
					return evLocked, true
				}
				return evUnlocked, true
			}
		}
	case iface + ".Error":
		if len(sig.Body) == 2 {
			log.Printf("GNOME Shell screencast error: %v: %v", sig.Body[0], sig.Body[1])
		}
		return evScreencastError, true
	case login1ManagerIfce + ".PrepareForSleep":
		if len(sig.Body) == 1 {
			if sleeping, ok := sig.Body[0].(bool); ok && !sleeping {
				return evResumed, true
			}
		}
	}
	return 0, false
}

var _ sessionEvents = (*dbusSessionEvents)(nil)
