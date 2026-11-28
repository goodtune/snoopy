package main

import (
	"log"
	"os"
	"strings"

	"snoopy/internal/attest/logind"
)

// Display managers run their login screen (the "greeter") as a logind session
// of class "greeter" belonging to a dedicated user (gdm-greeter/gdm on GNOME,
// lightdm, sddm, ...). That user has its own systemd user manager, so a
// globally enabled snoopy.service starts there too. The unit skips GDM with
// ConditionGroup=!gdm; this check covers every other display manager: if all
// of the uid's interactive sessions are greeters, there is nothing to record
// (GDM even sets org.gnome.desktop.lockdown disable-save-to-disk there).
//
// Session classes (systemd src/login/logind-session.c, session_class_table):
// user, user-early, user-incomplete, user-light, user-early-light, greeter,
// lock-screen, background, background-light, manager, manager-early, none.
// The service manager's own session ("manager", "manager-early") exists for
// every user with a running user manager, and "background*" sessions (cron
// and the like) are not interactive, so neither says anything about who is
// at the screen and both are ignored. Only "greeter" means a login screen;
// "lock-screen" and the user-* classes keep snoopy running.

// userSessionSource lists the logind sessions of a uid. *logind.Client
// implements it; tests use a fake.
type userSessionSource interface {
	UserSessions(uid uint32) ([]logind.Session, error)
}

// ignoredForGreeterCheck reports whether a session class says nothing about
// what is on screen.
func ignoredForGreeterCheck(class string) bool {
	return strings.HasPrefix(class, "manager") || strings.HasPrefix(class, "background") || class == "none"
}

// greeterOnly reports whether uid's interactive sessions are all greeters.
// No sessions, or only ignored ones, is false: without evidence snoopy runs.
func greeterOnly(sessions []logind.Session) bool {
	n := 0
	for _, s := range sessions {
		if ignoredForGreeterCheck(s.Class) {
			continue
		}
		if s.Class != "greeter" {
			return false
		}
		n++
	}
	return n > 0
}

// textOnly reports whether uid's interactive sessions are all text
// sessions (logind Type "tty": a virtual console or SSH). GNOME Shell is not
// on the session bus then, so its screencast service cannot be activated
// and the recorder would fail and be restarted every RestartSec (#22). The
// unit is WantedBy=graphical-session.target and so is not started for such
// sessions by itself, but an explicit `systemctl --user start` (e.g. by
// configuration management) still starts it. Any other type, including
// "unspecified" or none, keeps snoopy running: without evidence it runs.
func textOnly(sessions []logind.Session) bool {
	n := 0
	for _, s := range sessions {
		if ignoredForGreeterCheck(s.Class) {
			continue
		}
		if s.Type != "tty" {
			return false
		}
		n++
	}
	return n > 0
}

// skipReason asks src about uid's sessions and returns the message to log
// when there is nothing for snoopy to record (a greeter, or text sessions
// only), or "". A lookup error is returned for logging only, with "".
func skipReason(src userSessionSource, uid uint32) (string, error) {
	sessions, err := src.UserSessions(uid)
	if err != nil {
		return "", err
	}
	switch {
	case greeterOnly(sessions):
		return greeterMessage, nil
	case textOnly(sessions):
		return textOnlyMessage, nil
	}
	return "", nil
}

// notRecordableSession returns why this process's user has nothing to
// record (see skipReason), asking logind on the system bus, or "". Any
// failure is logged in one line and reported as "" so it never blocks
// recording.
func notRecordableSession() string {
	c, err := logind.Connect()
	if err != nil {
		log.Printf("Session check skipped (logind unavailable): %v", err)
		return ""
	}
	defer func() { _ = c.Close() }()
	uid := uint32(os.Getuid()) //nolint:gosec // uids fit in uint32
	reason, err := skipReason(c, uid)
	if err != nil {
		log.Printf("Session check skipped (logind query failed): %v", err)
		return ""
	}
	return reason
}

// greeterMessage is logged when snoopy declines to run in a greeter session.
const greeterMessage = `Not recording: this is a display-manager greeter session (logind class "greeter"); exiting`

// textOnlyMessage is logged when the user has only text sessions.
const textOnlyMessage = `Not recording: this user has only text sessions (logind type "tty", e.g. a console or SSH) and no graphical session; exiting (snoopy starts with the graphical session)`
