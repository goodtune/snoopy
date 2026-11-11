// Package procfs reads the few parts of Linux /proc that snoopy-attest needs
// to inspect another user's processes from outside.
//
// Everything is relative to FS.Root (normally "/proc") so tests can build a
// fake tree under a temporary directory: exe and fd/N are symlinks there, just
// as they are magic links in the real /proc, so the same os.Readlink/os.Stat
// calls work on both.
//
// Reading exe, environ, fd/ and fdinfo/ of a process owned by another uid
// requires ptrace read access (PTRACE_MODE_READ_FSCREDS), which root has
// through CAP_SYS_PTRACE; see snoopy-attest.service.
package procfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FS is a /proc mount.
type FS struct {
	Root string
}

// Default is the host's /proc.
var Default = FS{Root: "/proc"}

func (fs FS) path(pid int, elem ...string) string {
	return filepath.Join(append([]string{fs.Root, strconv.Itoa(pid)}, elem...)...)
}

// PIDs lists the numeric entries of the proc root.
func (fs FS) PIDs() ([]int, error) {
	entries, err := os.ReadDir(fs.Root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// UID returns the real uid of pid from the Uid: line of /proc/<pid>/status.
func (fs FS) UID(pid int) (uint32, error) {
	data, err := os.ReadFile(fs.path(pid, "status"))
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Uid:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			break
		}
		uid, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("pid %d: bad Uid line: %w", pid, err)
		}
		return uint32(uid), nil
	}
	return 0, fmt.Errorf("pid %d: no Uid line in status", pid)
}

// Exe returns the target of /proc/<pid>/exe. The kernel appends " (deleted)"
// when the executable was unlinked or replaced since the process started.
func (fs FS) Exe(pid int) (string, error) {
	return os.Readlink(fs.path(pid, "exe"))
}

// StatExe stats the executable the process is actually running (following
// the magic link to the mapped inode, even if the path was replaced).
func (fs FS) StatExe(pid int) (os.FileInfo, error) {
	return os.Stat(fs.path(pid, "exe"))
}

func splitNUL(data []byte) []string {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// Cmdline returns argv. It is controlled by the process itself and must be
// treated as a hint, not proof.
func (fs FS) Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(fs.path(pid, "cmdline"))
	if err != nil {
		return nil, err
	}
	return splitNUL(data), nil
}

// Environ returns the initial environment of the process (what the dynamic
// loader saw at exec time).
func (fs FS) Environ(pid int) ([]string, error) {
	data, err := os.ReadFile(fs.path(pid, "environ"))
	if err != nil {
		return nil, err
	}
	return splitNUL(data), nil
}

// Cgroup returns the cgroup v2 path (the "0::" line) of pid.
func (fs FS) Cgroup(pid int) (string, error) {
	data, err := os.ReadFile(fs.path(pid, "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("pid %d: no cgroup v2 entry", pid)
}

// FD is one open file descriptor of a process.
type FD struct {
	Num    int
	Target string      // readlink of fd/N, e.g. "/home/kid/.cache/snoopy/video/x.webm"
	Flags  int         // open(2) flags from fdinfo/N (O_ACCMODE bits etc.)
	Info   os.FileInfo // stat of the open file (following the magic link); nil if unavailable
}

// Writable reports whether the descriptor was opened for writing.
func (f FD) Writable() bool {
	acc := f.Flags & 0o3 // O_ACCMODE
	return acc == 0o1 || acc == 0o2
}

// FDs lists the open descriptors of pid. Descriptors that vanish while being
// read are skipped.
func (fs FS) FDs(pid int) ([]FD, error) {
	dir := fs.path(pid, "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []FD
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		flags, err := fs.fdFlags(pid, n)
		if err != nil {
			continue
		}
		fd := FD{Num: n, Target: target, Flags: flags}
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
			fd.Info = fi
		}
		out = append(out, fd)
	}
	return out, nil
}

func (fs FS) fdFlags(pid, fd int) (int, error) {
	data, err := os.ReadFile(fs.path(pid, "fdinfo", strconv.Itoa(fd)))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "flags:"); ok {
			f, err := strconv.ParseInt(strings.TrimSpace(v), 8, 64)
			if err != nil {
				return 0, fmt.Errorf("pid %d fd %d: bad flags: %w", pid, fd, err)
			}
			return int(f), nil
		}
	}
	return 0, errors.New("no flags in fdinfo")
}

// clockTicks is USER_HZ, which is 100 on every Linux architecture Snoopy
// targets (amd64, arm64).
const clockTicks = 100

// StartTime returns when pid started, from field 22 of /proc/<pid>/stat and
// btime in /proc/stat.
func (fs FS) StartTime(pid int) (time.Time, error) {
	data, err := os.ReadFile(fs.path(pid, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	// comm (field 2) may contain spaces and parentheses; fields resume after
	// the last ')'.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return time.Time{}, fmt.Errorf("pid %d: malformed stat", pid)
	}
	f := strings.Fields(string(data[i+1:]))
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return time.Time{}, fmt.Errorf("pid %d: short stat", pid)
	}
	ticks, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("pid %d: bad starttime: %w", pid, err)
	}
	btime, err := fs.bootTime()
	if err != nil {
		return time.Time{}, err
	}
	return btime.Add(time.Duration(ticks) * time.Second / clockTicks), nil
}

func (fs FS) bootTime() (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(fs.Root, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(s, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in stat")
}

// Uptime returns the first field of /proc/uptime (time since boot, which
// includes time spent suspended and never goes backwards).
func (fs FS) Uptime() (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join(fs.Root, "uptime"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, errors.New("empty uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("bad uptime: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// BootID returns /proc/sys/kernel/random/boot_id.
func (fs FS) BootID() (string, error) {
	data, err := os.ReadFile(filepath.Join(fs.Root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
