package attest

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/procfs/procfstest"
)

func TestStates(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(e *env)
		grace   bool
		frame   bool // frame check failed twice
		want    State
		reasons []string
	}{
		{name: "recording", want: StateRecording, reasons: []string{}},

		// Session.
		{name: "logind unavailable", mutate: func(e *env) { e.lg.err = errors.New("bus gone") }, want: StateAbsent, reasons: []string{ReasonLogindUnavailable}},
		{name: "nobody on seat", mutate: func(e *env) { e.lg.active = nil }, want: StateNoSession, reasons: []string{ReasonNoActiveSession}},
		{name: "greeter", mutate: func(e *env) {
			e.lg.active = &logind.Session{ID: "c1", UID: 42, Name: "gdm", Class: "greeter", Type: "wayland", Seat: "seat0", Active: true}
		}, want: StateNoSession, reasons: []string{ReasonGreeter}},
		{name: "other user active", mutate: func(e *env) {
			e.lg.active = &logind.Session{ID: "5", UID: 1001, Name: "parent", Class: "user", Type: "wayland", Seat: "seat0", Active: true}
		}, want: StateNoSession, reasons: []string{ReasonOtherUser}},
		{name: "tty session on seat", mutate: func(e *env) { e.lg.active.Type = "tty" }, want: StateNoSession, reasons: []string{ReasonUnsupportedSession}},
		{name: "x11 ok", mutate: func(e *env) { e.lg.active.Type = "x11" }, want: StateRecording, reasons: []string{}},
		{name: "locked", mutate: func(e *env) { e.lg.active.LockedHint = true }, want: StateLocked, reasons: []string{ReasonLocked}},
		{name: "locked without screencast", mutate: func(e *env) { e.lg.active.LockedHint = true; e.screen.PID = 0 }, want: StateLocked, reasons: []string{ReasonLocked}},
		{name: "locked without recorder", mutate: func(e *env) { e.lg.active.LockedHint = true; e.recorder.PID = 0 }, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},
		{name: "locked without recorder in grace", grace: true, mutate: func(e *env) { e.lg.active.LockedHint = true; e.recorder.PID = 0 }, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},
		{name: "ssh session", mutate: func(e *env) {
			e.lg.sessions = []logind.Session{{ID: "9", UID: kidUID, Class: "user", Type: "tty", Remote: true, Service: "sshd", State: "active"}}
		}, want: StateDegraded, reasons: []string{ReasonExtraSession}},
		{name: "text console login", mutate: func(e *env) {
			e.lg.sessions = []logind.Session{{ID: "8", UID: kidUID, Class: "user", Type: "tty", TTY: "tty3", Service: "login", State: "online"}}
		}, want: StateDegraded, reasons: []string{ReasonExtraSession}},
		{name: "ssh session allowed", mutate: func(e *env) {
			e.cfg.AllowExtraSessions = true
			e.lg.sessions = []logind.Session{{ID: "9", UID: kidUID, Class: "user", Type: "tty", Remote: true, State: "active"}}
		}, want: StateRecording, reasons: []string{}},
		{name: "closing and manager sessions ignored", mutate: func(e *env) {
			e.lg.sessions = []logind.Session{
				{ID: "9", UID: kidUID, Class: "user", Type: "tty", Remote: true, State: "closing"},
				{ID: "10", UID: kidUID, Class: "manager", Type: "unspecified", State: "active"},
				{ID: "11", UID: 1001, Class: "user", Type: "tty", Remote: true, State: "active"},
			}
		}, want: StateRecording, reasons: []string{}},

		// Recorder integrity.
		{name: "recorder not running", mutate: func(e *env) { e.recorder.PID = 0 }, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},
		{name: "recorder not running in grace", grace: true, mutate: func(e *env) { e.recorder.PID = 0 }, want: StateDegraded, reasons: []string{ReasonRecorderNotRunning, ReasonStartupGrace}},
		{name: "recorder not installed", mutate: func(e *env) { mustRemove(t, e.exe) }, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},
		{name: "copied recorder elsewhere", mutate: func(e *env) {
			cp := filepath.Join(e.home, "snoopy")
			writeFile(t, cp, "snoopy-binary", 0o755)
			e.recorder.Exe = cp
		}, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},
		{name: "replaced recorder binary", mutate: func(e *env) {
			old := e.exe + deletedSuffix
			writeFile(t, old, "old-binary", 0o755)
			e.recorder.Exe = old
		}, want: StateAbsent, reasons: []string{ReasonExeMismatch}},
		{name: "replaced recorder in grace stays absent", grace: true, mutate: func(e *env) {
			old := e.exe + deletedSuffix
			writeFile(t, old, "old-binary", 0o755)
			e.recorder.Exe = old
		}, want: StateAbsent, reasons: []string{ReasonExeMismatch}},
		{name: "writable recorder binary", mutate: func(e *env) { mustChmod(t, e.exe, 0o777) }, want: StateAbsent, reasons: []string{ReasonExeInsecure}},
		{name: "two recorders", mutate: func(e *env) {
			e.tr.Add(procfstest.Proc{PID: 101, UID: kidUID, Exe: e.exe, Cmdline: []string{e.exe}, Cgroup: e.recorder.Cgroup})
		}, want: StateAbsent, reasons: []string{ReasonMultipleRecorders}},
		{name: "recorder outside its unit", mutate: func(e *env) {
			e.recorder.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-terminal-123.scope"
		}, want: StateAbsent, reasons: []string{ReasonWrongCgroup}},
		{name: "recorder in another user's manager", mutate: func(e *env) {
			e.recorder.Cgroup = "/user.slice/user-1001.slice/user@1001.service/app.slice/snoopy.service"
		}, want: StateAbsent, reasons: []string{ReasonWrongCgroup}},
		{name: "LD_PRELOAD", mutate: func(e *env) { e.recorder.Environ = append(e.recorder.Environ, "LD_PRELOAD=/home/kid1/x.so") }, want: StateAbsent, reasons: []string{ReasonLDPreload}},
		{name: "LD_AUDIT in grace stays absent", grace: true, mutate: func(e *env) { e.recorder.Environ = append(e.recorder.Environ, "LD_AUDIT=/x.so") }, want: StateAbsent, reasons: []string{ReasonLDPreload}},
		{name: "not managed", mutate: func(e *env) { writeFile(t, e.conf, "SNOOPY_MANAGED=0\n", 0o644) }, want: StateAbsent, reasons: []string{ReasonNotManaged}},
		{name: "managed switch the recorder does not accept", mutate: func(e *env) { writeFile(t, e.conf, "SNOOPY_MANAGED=on\n", 0o644) }, want: StateAbsent, reasons: []string{ReasonNotManaged}},
		{name: "managed conf is a directory", mutate: func(e *env) { mustRemove(t, e.conf); mustMkdir(t, e.conf) }, want: StateAbsent, reasons: []string{ReasonConfInsecure}},
		{name: "managed conf missing", mutate: func(e *env) { mustRemove(t, e.conf) }, want: StateAbsent, reasons: []string{ReasonNotManaged}},
		{name: "managed conf malformed", mutate: func(e *env) { writeFile(t, e.conf, "garbage\n", 0o644) }, want: StateAbsent, reasons: []string{ReasonNotManaged}},
		{name: "managed conf writable", mutate: func(e *env) { mustChmod(t, e.conf, 0o666) }, want: StateAbsent, reasons: []string{ReasonConfInsecure}},
		{name: "managed conf dir writable", mutate: func(e *env) { mustChmod(t, filepath.Dir(e.conf), 0o777) }, want: StateAbsent, reasons: []string{ReasonConfInsecure}},
		{name: "recorder of another uid only", mutate: func(e *env) { e.recorder.UID = 1001 }, want: StateAbsent, reasons: []string{ReasonRecorderNotRunning}},

		// Recording evidence.
		{name: "no screencast service", mutate: func(e *env) { e.screen.PID = 0 }, want: StateDegraded, reasons: []string{ReasonNoScreencastProcess}},
		{name: "no screencast service in grace", grace: true, mutate: func(e *env) { e.screen.PID = 0 }, want: StateDegraded, reasons: []string{ReasonNoScreencastProcess, ReasonStartupGrace}},
		{name: "gjs running another script", mutate: func(e *env) {
			e.screen.Cmdline = []string{"/usr/bin/gjs", "-m", "/home/kid1/fake.js", e.script}
		}, want: StateDegraded, reasons: []string{ReasonNoScreencastProcess}},
		{name: "copied gjs", mutate: func(e *env) {
			cp := filepath.Join(e.home, "gjs-console")
			writeFile(t, cp, "gjs-binary", 0o755)
			e.screen.Exe = cp
		}, want: StateDegraded, reasons: []string{ReasonNoScreencastProcess}},
		{name: "writable gjs", mutate: func(e *env) { mustChmod(t, e.gjs, 0o777) }, want: StateDegraded, reasons: []string{ReasonScreencastInsecure}},
		{name: "writable screencast script", mutate: func(e *env) { mustChmod(t, e.script, 0o666) }, want: StateDegraded, reasons: []string{ReasonScreencastInsecure}},
		{name: "gst plugin path in screencast env", mutate: func(e *env) {
			e.screen.Environ = append(e.screen.Environ, "GST_PLUGIN_PATH=/home/kid1/plugins")
		}, want: StateDegraded, reasons: []string{ReasonScreencastEnv}},
		{name: "user gstreamer plugins", mutate: func(e *env) {
			writeFile(t, filepath.Join(e.home, ".local", "share", "gstreamer-1.0", "plugins", "libevil.so"), "x", 0o644)
		}, want: StateDegraded, reasons: []string{ReasonUserGstPlugins}},
		{name: "segment only open for reading", mutate: func(e *env) { e.screen.FDs[1].Flags = oRdonly }, want: StateDegraded, reasons: []string{ReasonNoScreencastWriter}},
		{name: "writer outside output dir", mutate: func(e *env) {
			other := filepath.Join(e.root, "tmp", "x.webm")
			writeFile(t, other, "x", 0o644)
			e.screen.FDs[1].Target = other
		}, want: StateDegraded, reasons: []string{ReasonNoScreencastWriter}},
		{name: "writer not newest", mutate: func(e *env) {
			newer := filepath.Join(e.outDir, "screen-2026-11-11-10-30-00.webm")
			writeFile(t, newer, "x", 0o644)
			e.touch(newer, time.Second)
		}, want: StateDegraded, reasons: []string{ReasonWriterNotNewest}},
		{name: "rotation overlap", mutate: func(e *env) {
			newer := filepath.Join(e.outDir, "screen-2026-11-11-10-30-00.mp4")
			writeFile(t, newer, "x", 0o644)
			e.touch(newer, time.Second)
			e.screen.FDs = append(e.screen.FDs, procfstest.OpenFile{Num: segFD + 1, Target: newer, Flags: 0o100002})
		}, want: StateRecording, reasons: []string{}},
		{name: "segment stale", mutate: func(e *env) { e.touch(e.segment, 6*time.Minute) }, want: StateDegraded, reasons: []string{ReasonSegmentStale}},
		{name: "segment quiet but within window", mutate: func(e *env) { e.touch(e.segment, 4*time.Minute) }, want: StateRecording, reasons: []string{}},
		{name: "SNOOPY_OUT elsewhere", mutate: func(e *env) {
			writeFile(t, e.conf, "SNOOPY_MANAGED=true\nSNOOPY_OUT="+filepath.Join(e.root, "srv", "rec")+"\n", 0o644)
		}, want: StateDegraded, reasons: []string{ReasonNoScreencastWriter}},
		{name: "SNOOPY_OUT honoured", mutate: func(e *env) {
			dir := filepath.Join(e.root, "srv", "rec")
			writeFile(t, e.conf, "SNOOPY_MANAGED=yes\nSNOOPY_OUT="+dir+"\n", 0o644)
			seg := filepath.Join(dir, "screen-1.webm")
			writeFile(t, seg, "x", 0o644)
			e.touch(seg, time.Second)
			e.screen.FDs[1].Target = seg
		}, want: StateRecording, reasons: []string{}},
		{name: "symlinked output dir", mutate: func(e *env) {
			real := filepath.Join(e.root, "data", "video")
			if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(e.outDir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, e.outDir); err != nil {
				t.Fatal(err)
			}
			e.screen.FDs[1].Target = filepath.Join(real, filepath.Base(e.segment))
		}, want: StateRecording, reasons: []string{}},
		{name: "no segments", mutate: func(e *env) {
			// The writer's file was unlinked from the directory.
			mustRemove(t, e.segment)
			writeFile(t, e.segment+".tmp", "x", 0o644)
			e.screen.FDs[1].Target = e.segment
		}, want: StateDegraded, reasons: []string{ReasonNoScreencastWriter}},
		{name: "frame check failing", frame: true, want: StateDegraded, reasons: []string{ReasonFrameCheckFailed}},
		{name: "extra session and stale segment", mutate: func(e *env) {
			e.lg.sessions = []logind.Session{{ID: "9", UID: kidUID, Class: "user", Type: "tty", Remote: true, State: "active"}}
			e.touch(e.segment, time.Hour)
		}, want: StateDegraded, reasons: []string{ReasonExtraSession, ReasonSegmentStale}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.mutate != nil {
				tt.mutate(e)
			}
			e.install()
			raw := e.checker().run()
			raw.FrameBad = tt.frame
			st, reasons := decide(&raw, tt.grace)
			if st != tt.want || !slices.Equal(reasons, tt.reasons) {
				var b strings.Builder
				for _, c := range []CheckResult{raw.Session.CheckResult, raw.Recorder.CheckResult, raw.Screencast.CheckResult} {
					b.WriteString("\n  " + c.Name + ": " + string(c.Status) + " " + c.Detail + " " + strings.Join(c.Notes, "; "))
				}
				t.Fatalf("got %s %q, want %s %q%s", st, reasons, tt.want, tt.reasons, b.String())
			}
		})
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, p string, m os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, m); err != nil {
		t.Fatal(err)
	}
}

func TestEvidence(t *testing.T) {
	e := newEnv(t)
	e.install()
	raw := e.checker().run()
	raw.FrameWH = []int{2560, 1440}
	ev := raw.evidence()
	if ev.RecorderPID != recorderPID || ev.ScreencastPID != gjsPID || ev.Segment != filepath.Base(e.segment) ||
		ev.SegmentSize != int64(len("webm-data")) || ev.RecorderStart != "2023-11-14T22:13:30Z" ||
		ev.SegmentMtime != e.now.Add(-10*time.Second).UTC().Format(time.RFC3339) || len(ev.FrameWH) != 2 {
		t.Fatalf("evidence %+v", ev)
	}
}

func TestEvaluatorGrace(t *testing.T) {
	e := newEnv(t)
	e.recorder.PID = 0 // the recorder has not started yet
	e.install()
	c := e.checker()
	ev := Evaluator{Grace: 30 * time.Second}
	t0 := e.now

	step := func(at time.Duration, want State, wantGrace bool) {
		t.Helper()
		e.now = t0.Add(at)
		raw := c.run()
		st, reasons, g := ev.evaluate(e.now, &raw)
		if st != want || (g != nil) != wantGrace {
			t.Fatalf("t=%v: got %s %v grace=%v, want %s grace=%v", at, st, reasons, g, want, wantGrace)
		}
	}
	// Service start: grace.
	step(0, StateDegraded, true)
	step(29*time.Second, StateDegraded, true)
	step(31*time.Second, StateAbsent, false)

	// Lock, then unlock: grace again while the recorder restarts.
	e.lg.set(func(f *fakeLogind) { f.active.LockedHint = true })
	step(40*time.Second, StateAbsent, false)
	e.lg.set(func(f *fakeLogind) { f.active.LockedHint = false })
	step(50*time.Second, StateDegraded, true)
	step(81*time.Second, StateAbsent, false)

	// Greeter, then the user logs in again.
	e.lg.set(func(f *fakeLogind) {
		f.active = &logind.Session{ID: "c1", UID: 42, Name: "gdm", Class: "greeter", Type: "wayland"}
	})
	step(90*time.Second, StateNoSession, false)
	e.lg.set(func(f *fakeLogind) {
		f.active = &logind.Session{ID: "3", UID: kidUID, Name: "kid1", Class: "user", Type: "wayland", State: "active", Active: true}
	})
	step(95*time.Second, StateDegraded, true)

	// The recorder starts: recording, even within the grace window.
	e.tr.Add(e.recorderProc())
	step(100*time.Second, StateRecording, true)
	step(200*time.Second, StateRecording, false)

	// Resume from sleep: grace opens, recorder missing → degraded.
	e.tr.Remove(recorderPID)
	ev.StartGrace(t0.Add(300 * time.Second))
	step(301*time.Second, StateDegraded, true)
	step(340*time.Second, StateAbsent, false)
}

func (e *env) recorderProc() procfstest.Proc {
	p := e.recorder
	p.PID = recorderPID
	return p
}

// TestManagedSwitchOnAgreesWithRecorder pins the SNOOPY_MANAGED truth table
// shared with the recorder (server/config.go, parseManagedSwitch, tested by
// TestParseManagedSwitchTable). Values the recorder rejects as invalid (it
// exits 2) never count as managed here.
func TestManagedSwitchOnAgreesWithRecorder(t *testing.T) {
	on := []string{"1", "true", "yes", "TRUE", "Yes", " 1 "}
	off := []string{"", "0", "false", "no", "FALSE", " no ", "on", "y", "2", "enabled"}
	for _, v := range on {
		if !managedSwitchOn(v) {
			t.Errorf("managedSwitchOn(%q) = false, want true", v)
		}
	}
	for _, v := range off {
		if managedSwitchOn(v) {
			t.Errorf("managedSwitchOn(%q) = true, want false", v)
		}
	}
}
