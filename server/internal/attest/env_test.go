package attest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/procfs"
	"snoopy/internal/attest/procfs/procfstest"
)

const (
	kidUID      = 1000
	recorderPID = 100
	gjsPID      = 200
	segFD       = 21
	oWronly     = 0o100001 // O_WRONLY|O_LARGEFILE
	oRdonly     = 0o100000
)

type fakeLogind struct {
	mu       sync.Mutex
	active   *logind.Session
	sessions []logind.Session
	err      error
	events   chan logind.Event
}

func (f *fakeLogind) ActiveSession(string) (*logind.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.active == nil {
		return nil, nil
	}
	s := *f.active
	return &s, nil
}

func (f *fakeLogind) UserSessions(uid uint32) ([]logind.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []logind.Session
	for _, s := range f.sessions {
		if s.UID == uid {
			out = append(out, s)
		}
	}
	if f.active != nil && f.active.UID == uid {
		out = append(out, *f.active)
	}
	return out, nil
}

func (f *fakeLogind) Watch(context.Context) (<-chan logind.Event, error) { return f.events, nil }

func (f *fakeLogind) set(fn func(f *fakeLogind)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// env is a complete fake desktop: a fake /proc, a fake logind, real files
// for the recorder binary, gjs, the screencast script, the managed
// configuration and the output directory.
type env struct {
	t        *testing.T
	root     string
	tr       *procfstest.Tree
	lg       *fakeLogind
	cfg      *Config
	now      time.Time
	home     string
	outDir   string // resolved real path
	segment  string
	exe      string
	gjs      string
	script   string
	conf     string
	recorder procfstest.Proc
	screen   procfstest.Proc
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // ignore umask
		t.Fatal(err)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, tr: procfstest.New(t), now: time.Now().Truncate(time.Second)}
	e.home = filepath.Join(root, "home", "kid1")
	e.outDir = filepath.Join(e.home, ".cache", "snoopy", "video")
	e.exe = filepath.Join(root, "usr", "bin", "snoopy")
	e.gjs = filepath.Join(root, "usr", "bin", "gjs-console")
	e.script = filepath.Join(root, "usr", "share", "gnome-shell", "org.gnome.Shell.Screencast")
	e.conf = filepath.Join(root, "etc", "snoopy", "snoopy.conf")
	writeFile(t, e.exe, "snoopy-binary", 0o755)
	writeFile(t, e.gjs, "gjs-binary", 0o755)
	writeFile(t, e.script, "// screencast", 0o644)
	writeFile(t, e.conf, "SNOOPY_MANAGED=1\n", 0o644)
	if err := os.Chmod(filepath.Dir(e.conf), 0o755); err != nil {
		t.Fatal(err)
	}
	e.segment = filepath.Join(e.outDir, "screen-2026-11-11-10-00-00.webm")
	writeFile(t, e.segment, "webm-data", 0o644)
	e.touch(e.segment, 10*time.Second)

	cfg, err := Parse(map[string]string{
		"SNOOPY_ATTEST_USER":              "kid1",
		"SNOOPY_ATTEST_DEVICE_ID":         "desk1",
		"SNOOPY_ATTEST_RUNTIME_DIR":       filepath.Join(root, "run"),
		"SNOOPY_ATTEST_STATE_DIR":         filepath.Join(root, "state"),
		"SNOOPY_ATTEST_RECORDER_EXE":      e.exe,
		"SNOOPY_ATTEST_RECORDER_CONF":     e.conf,
		"SNOOPY_ATTEST_GJS_PATHS":         e.gjs + "," + filepath.Join(root, "usr", "bin", "gjs"),
		"SNOOPY_ATTEST_SCREENCAST_SCRIPT": e.script,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UID, cfg.GID, cfg.Home = kidUID, kidUID, e.home
	e.cfg = cfg

	e.lg = &fakeLogind{
		active: &logind.Session{ID: "2", UID: kidUID, Name: "kid1", Seat: "seat0", TTY: "tty2", Type: "wayland", Class: "user", State: "active", Active: true},
		events: make(chan logind.Event, 4),
	}
	e.recorder = procfstest.Proc{
		PID: recorderPID, UID: kidUID, Exe: e.exe,
		Cmdline:   []string{e.exe},
		Environ:   []string{"HOME=" + e.home, "PATH=/usr/bin"},
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/snoopy.service",
		StartTick: 1000,
	}
	e.screen = procfstest.Proc{
		PID: gjsPID, UID: kidUID, Exe: e.gjs,
		Cmdline: []string{"/usr/bin/gjs", "-m", e.script},
		Environ: []string{"HOME=" + e.home},
		Cgroup:  "/user.slice/user-1000.slice/user@1000.service/app.slice/dbus-:1.2-org.gnome.Shell.Screencast@0.service",
		FDs: []procfstest.OpenFile{
			{Num: 0, Target: "/dev/null", Flags: oRdonly},
			{Num: segFD, Target: e.segment, Flags: oWronly},
		},
	}
	// Unrelated processes of the user and of another user.
	e.tr.Add(procfstest.Proc{PID: 50, UID: kidUID, Exe: "/usr/bin/bash", Cmdline: []string{"bash"}})
	e.tr.Add(procfstest.Proc{PID: 60, UID: 1001, Exe: e.exe, Cmdline: []string{e.exe}, Cgroup: "/user.slice/user-1001.slice/user@1001.service/app.slice/snoopy.service"})
	return e
}

// install adds the recorder and screencast processes (call after mutating
// e.recorder / e.screen).
func (e *env) install() {
	e.t.Helper()
	if e.recorder.PID != 0 {
		e.tr.Add(e.recorder)
	}
	if e.screen.PID != 0 {
		e.tr.Add(e.screen)
	}
}

func (e *env) touch(path string, age time.Duration) {
	e.t.Helper()
	ts := e.now.Add(-age)
	if err := os.Chtimes(path, ts, ts); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) checker() *Checker {
	return &Checker{
		Cfg:        e.cfg,
		Proc:       procfs.FS{Root: e.tr.Root},
		Logind:     e.lg,
		Now:        func() time.Time { return e.now },
		TrustedUID: uint32(os.Getuid()), //nolint:gosec // test uid fits
	}
}
