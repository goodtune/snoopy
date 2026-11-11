package procfs_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"snoopy/internal/attest/procfs"
	"snoopy/internal/attest/procfs/procfstest"
)

func TestFS(t *testing.T) {
	tr := procfstest.New(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin")
	out := filepath.Join(dir, "seg.webm")
	for _, p := range []string{exe, out} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr.Add(procfstest.Proc{
		PID: 42, UID: 1000, Exe: exe,
		Cmdline:   []string{"/usr/bin/gjs", "-m", "/script"},
		Environ:   []string{"A=1", "LD_PRELOAD=/x.so"},
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/snoopy.service",
		StartTick: 250,
		FDs: []procfstest.OpenFile{
			{Num: 0, Target: "/dev/null", Flags: 0o100000},
			{Num: 7, Target: out, Flags: 0o100001},
			{Num: 8, Target: "pipe:[123]", Flags: 0o2},
		},
	})
	fs := procfs.FS{Root: tr.Root}

	pids, err := fs.PIDs()
	if err != nil || !reflect.DeepEqual(pids, []int{42}) {
		t.Fatalf("PIDs = %v, %v", pids, err)
	}
	if uid, err := fs.UID(42); err != nil || uid != 1000 {
		t.Fatalf("UID = %d, %v", uid, err)
	}
	if e, err := fs.Exe(42); err != nil || e != exe {
		t.Fatalf("Exe = %q, %v", e, err)
	}
	if fi, err := fs.StatExe(42); err != nil || fi.Size() != 1 {
		t.Fatalf("StatExe = %v, %v", fi, err)
	}
	if c, err := fs.Cmdline(42); err != nil || !reflect.DeepEqual(c, []string{"/usr/bin/gjs", "-m", "/script"}) {
		t.Fatalf("Cmdline = %q, %v", c, err)
	}
	if e, err := fs.Environ(42); err != nil || len(e) != 2 || e[1] != "LD_PRELOAD=/x.so" {
		t.Fatalf("Environ = %q, %v", e, err)
	}
	if c, err := fs.Cgroup(42); err != nil || filepath.Base(c) != "snoopy.service" {
		t.Fatalf("Cgroup = %q, %v", c, err)
	}
	fds, err := fs.FDs(42)
	if err != nil || len(fds) != 3 {
		t.Fatalf("FDs = %v, %v", fds, err)
	}
	var w []string
	for _, f := range fds {
		if f.Writable() {
			w = append(w, f.Target)
		}
	}
	if !reflect.DeepEqual(w, []string{out, "pipe:[123]"}) {
		t.Fatalf("writable = %q", w)
	}
	st, err := fs.StartTime(42)
	if err != nil || !st.Equal(time.Unix(1700000002, 500_000_000)) {
		t.Fatalf("StartTime = %v, %v", st, err)
	}
	if id, err := fs.BootID(); err != nil || id != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" {
		t.Fatalf("BootID = %q, %v", id, err)
	}
	if up, err := fs.Uptime(); err != nil || up != 12345670*time.Millisecond {
		t.Fatalf("Uptime = %v, %v", up, err)
	}
	if _, err := fs.UID(99); err == nil {
		t.Fatal("UID of missing pid succeeded")
	}
}

func TestHostProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc")
	}
	fs := procfs.Default
	pid := os.Getpid()
	uid, err := fs.UID(pid)
	if err != nil || int(uid) != os.Getuid() {
		t.Fatalf("UID = %d, %v", uid, err)
	}
	st, err := fs.StartTime(pid)
	if err != nil || time.Since(st) > time.Hour || time.Since(st) < -time.Minute {
		t.Fatalf("StartTime = %v, %v", st, err)
	}
	if _, err := fs.FDs(pid); err != nil {
		t.Fatal(err)
	}
}
