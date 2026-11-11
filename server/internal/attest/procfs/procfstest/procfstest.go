// Package procfstest builds fake /proc trees for tests.
package procfstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tree is a fake proc root under a temporary directory.
type Tree struct {
	t    testing.TB
	Root string
}

// New creates an empty fake /proc with stat (btime) and a boot_id.
func New(t testing.TB) *Tree {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proc")
	tr := &Tree{t: t, Root: root}
	tr.write("stat", "cpu  1 2 3 4\nbtime 1700000000\n")
	tr.write("uptime", "12345.67 1000.00\n")
	tr.write("sys/kernel/random/boot_id", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0\n")
	return tr
}

func (tr *Tree) write(rel, content string) {
	tr.t.Helper()
	p := filepath.Join(tr.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

// OpenFile describes an open descriptor of a fake process.
type OpenFile struct {
	Num    int
	Target string // symlink target (usually an absolute path to a real file)
	Flags  int    // e.g. 0o100001 for O_WRONLY|O_LARGEFILE
}

// Proc describes a fake process.
type Proc struct {
	PID       int
	UID       uint32
	Exe       string // symlink target of exe
	Cmdline   []string
	Environ   []string
	Cgroup    string // cgroup v2 path, e.g. /user.slice/.../snoopy.service
	StartTick uint64 // field 22 of stat
	FDs       []OpenFile
}

// Add creates /proc/<pid> for p.
func (tr *Tree) Add(p Proc) {
	tr.t.Helper()
	pid := strconv.Itoa(p.PID)
	u := strconv.FormatUint(uint64(p.UID), 10)
	tr.write(pid+"/status", fmt.Sprintf("Name:\tx\nUid:\t%s\t%s\t%s\t%s\nGid:\t%s\t%s\t%s\t%s\n", u, u, u, u, u, u, u, u))
	tr.write(pid+"/cmdline", strings.Join(p.Cmdline, "\x00")+"\x00")
	env := ""
	if len(p.Environ) > 0 {
		env = strings.Join(p.Environ, "\x00") + "\x00"
	}
	tr.write(pid+"/environ", env)
	tr.write(pid+"/cgroup", "0::"+p.Cgroup+"\n")
	tr.write(pid+"/stat", fmt.Sprintf("%s (my proc) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, p.StartTick))
	if p.Exe != "" {
		if err := os.Symlink(p.Exe, filepath.Join(tr.Root, pid, "exe")); err != nil {
			tr.t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tr.Root, pid, "fd"), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	for _, f := range p.FDs {
		tr.AddFD(p.PID, f)
	}
}

// AddFD adds one open descriptor to an existing fake process.
func (tr *Tree) AddFD(pid int, f OpenFile) {
	tr.t.Helper()
	ps := strconv.Itoa(pid)
	n := strconv.Itoa(f.Num)
	if err := os.Symlink(f.Target, filepath.Join(tr.Root, ps, "fd", n)); err != nil {
		tr.t.Fatal(err)
	}
	tr.write(ps+"/fdinfo/"+n, fmt.Sprintf("pos:\t0\nflags:\t0%o\nmnt_id:\t1\n", f.Flags))
}

// Remove deletes /proc/<pid>.
func (tr *Tree) Remove(pid int) {
	tr.t.Helper()
	if err := os.RemoveAll(filepath.Join(tr.Root, strconv.Itoa(pid))); err != nil {
		tr.t.Fatal(err)
	}
}
