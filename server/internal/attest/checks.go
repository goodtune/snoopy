package attest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"snoopy/internal/attest/heartbeat"
	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/procfs"
	"snoopy/internal/envfile"
)

// Reasons are stable, machine-readable identifiers sent to KProxy and shown
// by `snoopy-attest check`. docs/attestation.md lists them; keep in sync.
const (
	// Session (logind).
	ReasonLogindUnavailable  = "logind_unavailable"
	ReasonNoActiveSession    = "no_active_session"
	ReasonGreeter            = "greeter"
	ReasonOtherUser          = "other_user"
	ReasonUnsupportedSession = "unsupported_session"
	ReasonLocked             = "locked"
	ReasonExtraSession       = "extra_session"

	// Recorder integrity (/proc).
	ReasonProcUnavailable    = "proc_unavailable"
	ReasonRecorderNotRunning = "recorder_not_running"
	ReasonMultipleRecorders  = "multiple_recorders"
	ReasonExeMismatch        = "exe_mismatch"
	ReasonExeInsecure        = "exe_insecure"
	ReasonWrongCgroup        = "wrong_cgroup"
	ReasonLDPreload          = "ld_preload"
	ReasonNotManaged         = "not_managed"
	ReasonConfInsecure       = "conf_insecure"

	// Recording evidence.
	ReasonNoScreencastProcess = "no_screencast_process"
	ReasonScreencastInsecure  = "screencast_insecure"
	ReasonScreencastEnv       = "screencast_env"
	ReasonUserGstPlugins      = "user_gst_plugins"
	ReasonNoScreencastWriter  = "no_screencast_writer"
	ReasonNoSegments          = "no_segments"
	ReasonWriterNotNewest     = "writer_not_newest"
	ReasonSegmentStale        = "segment_stale"
	ReasonFrameCheckFailed    = "frame_check_failed"

	// Grace period after start, unlock or resume.
	ReasonStartupGrace = "startup_grace"
)

// Status of one check.
type Status string

// Check statuses.
const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// CheckResult is the outcome of one check, as shown by `check` and written
// to state.json.
type CheckResult struct {
	Name    string   `json:"name"`
	Status  Status   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
	Detail  string   `json:"detail"`
	Notes   []string `json:"notes,omitempty"`
}

func (r *CheckResult) fail(reason, format string, args ...any) {
	r.Status = StatusFail
	if !slices.Contains(r.Reasons, reason) {
		r.Reasons = append(r.Reasons, reason)
	}
	r.Notes = append(r.Notes, reason+": "+fmt.Sprintf(format, args...))
}

// SessionSource is the part of logind the session check needs.
type SessionSource interface {
	ActiveSession(seat string) (*logind.Session, error)
	UserSessions(uid uint32) ([]logind.Session, error)
}

// Checker runs the checks against injectable system interfaces.
type Checker struct {
	Cfg    *Config
	Proc   procfs.FS
	Logind SessionSource
	Now    func() time.Time
	// TrustedUID must own the recorder binary, its configuration and the
	// GNOME Shell files (0 in production; the test user in tests).
	TrustedUID uint32
}

// sessionResult is what the session check found.
type sessionResult struct {
	CheckResult
	Session   *logind.Session
	Err       bool // logind could not be queried
	NoSession bool // nobody (or not the supervised user) on the seat
	Locked    bool
	Extra     bool // disallowed extra sessions exist
}

var userClasses = []string{"user", "user-early", "user-incomplete", "user-light"}

func (c *Checker) checkSession() sessionResult {
	r := sessionResult{CheckResult: CheckResult{Name: "session", Status: StatusPass}}
	s, err := c.Logind.ActiveSession(c.Cfg.Seat)
	if err != nil {
		r.Err = true
		r.fail(ReasonLogindUnavailable, "%v", err)
		r.Detail = "cannot query systemd-logind"
		return r
	}
	if s == nil {
		r.NoSession = true
		r.fail(ReasonNoActiveSession, "no active session on %s", c.Cfg.Seat)
		r.Detail = "nobody is logged in on " + c.Cfg.Seat
		return r
	}
	r.Session = s
	switch {
	case s.Class == "greeter" || s.Class == "lock-screen":
		r.NoSession = true
		r.fail(ReasonGreeter, "active session %s is the %s (user %s)", s.ID, s.Class, s.Name)
		r.Detail = "login screen is active on " + c.Cfg.Seat
		return r
	case s.UID != c.Cfg.UID:
		r.NoSession = true
		r.fail(ReasonOtherUser, "active session %s belongs to %s (uid %d), not %s", s.ID, s.Name, s.UID, c.Cfg.User)
		r.Detail = fmt.Sprintf("another user (%s) is active on %s", s.Name, c.Cfg.Seat)
		return r
	case s.Class != "user" || (s.Type != "wayland" && s.Type != "x11"):
		r.NoSession = true
		r.fail(ReasonUnsupportedSession, "active session %s has class %q, type %q; want class user, type wayland or x11", s.ID, s.Class, s.Type)
		r.Detail = "the active session is not a graphical user session"
		return r
	}
	lock := "unlocked"
	if s.LockedHint {
		r.Locked = true
		lock = "locked"
		r.Notes = append(r.Notes, "session is locked")
	}
	r.Detail = fmt.Sprintf("%s session %s of %s (uid %d), %s", s.Type, s.ID, s.Name, s.UID, lock)

	all, err := c.Logind.UserSessions(c.Cfg.UID)
	if err != nil {
		r.Err = true
		r.fail(ReasonLogindUnavailable, "list sessions: %v", err)
		return r
	}
	for _, o := range all {
		if o.ID == s.ID || o.State == "closing" || !slices.Contains(userClasses, o.Class) {
			continue
		}
		if o.Type != "tty" && !o.Remote {
			continue
		}
		desc := fmt.Sprintf("session %s (type %s, tty %q, service %q, remote %v)", o.ID, o.Type, o.TTY, o.Service, o.Remote)
		if c.Cfg.AllowExtraSessions {
			r.Notes = append(r.Notes, "allowed extra "+desc)
			continue
		}
		r.Extra = true
		r.fail(ReasonExtraSession, "%s", desc)
	}
	return r
}

// managedSwitchOn mirrors the recorder's own parsing of SNOOPY_MANAGED
// (server/config.go): only 1, true and yes enable managed mode.
func managedSwitchOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// recorderResult is what the recorder integrity check found.
type recorderResult struct {
	CheckResult
	PID     int
	Start   time.Time
	OutDir  string // expected output directory, as configured
	Running bool   // a genuine-looking recorder process exists
}

func ownerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// trusted reports why fi is not a file only TrustedUID can change, or "".
func (c *Checker) untrusted(fi os.FileInfo) string {
	uid, ok := ownerUID(fi)
	if !ok {
		return "cannot determine owner"
	}
	if uid != c.TrustedUID {
		return fmt.Sprintf("owned by uid %d, want %d", uid, c.TrustedUID)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Sprintf("mode %04o is group- or world-writable", fi.Mode().Perm())
	}
	return ""
}

// userProcs returns the pids whose real uid is uid.
func (c *Checker) userProcs(uid uint32) ([]int, error) {
	pids, err := c.Proc.PIDs()
	if err != nil {
		return nil, err
	}
	var out []int
	for _, pid := range pids {
		if u, err := c.Proc.UID(pid); err == nil && u == uid {
			out = append(out, pid)
		}
	}
	return out, nil
}

const deletedSuffix = " (deleted)"

func (c *Checker) checkRecorder(pids []int) recorderResult {
	cfg := c.Cfg
	r := recorderResult{CheckResult: CheckResult{Name: "recorder", Status: StatusPass}}
	r.OutDir = filepath.Join(cfg.Home, ".cache", "snoopy", "video")

	// Managed-mode configuration: the genuine recorder ignores the user's
	// environment and flags when SNOOPY_MANAGED is set in this root-owned
	// file, so the effective settings are this file plus built-in defaults.
	managed := false
	if dir, err := os.Stat(filepath.Dir(cfg.RecorderConf)); err != nil {
		r.fail(ReasonNotManaged, "%v", err)
	} else if why := c.untrusted(dir); why != "" {
		r.fail(ReasonConfInsecure, "%s: %s", filepath.Dir(cfg.RecorderConf), why)
	}
	if fi, err := os.Stat(cfg.RecorderConf); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.fail(ReasonNotManaged, "%s does not exist; create it with SNOOPY_MANAGED=1", cfg.RecorderConf)
		} else {
			r.fail(ReasonNotManaged, "%v", err)
		}
	} else if !fi.Mode().IsRegular() {
		r.fail(ReasonConfInsecure, "%s is not a regular file", cfg.RecorderConf)
	} else if why := c.untrusted(fi); why != "" {
		r.fail(ReasonConfInsecure, "%s: %s", cfg.RecorderConf, why)
	} else if m, err := envfile.ParseFile(cfg.RecorderConf); err != nil {
		r.fail(ReasonNotManaged, "%v", err)
	} else {
		if !managedSwitchOn(m["SNOOPY_MANAGED"]) {
			r.fail(ReasonNotManaged, "%s does not set SNOOPY_MANAGED=1", cfg.RecorderConf)
		} else {
			managed = true
		}
		if out := strings.TrimSpace(m["SNOOPY_OUT"]); out != "" {
			if filepath.IsAbs(out) {
				r.OutDir = out
			} else {
				r.OutDir = filepath.Join(cfg.Home, out)
				r.Notes = append(r.Notes, fmt.Sprintf("SNOOPY_OUT=%q is relative; assuming %s", out, r.OutDir))
			}
		}
	}

	// The recorder binary on disk.
	exeInfo, err := os.Stat(cfg.RecorderExe)
	if err != nil {
		r.fail(ReasonRecorderNotRunning, "%v", err)
		r.Detail = cfg.RecorderExe + " is not installed"
		return r
	}
	if why := c.untrusted(exeInfo); why != "" {
		r.fail(ReasonExeInsecure, "%s: %s", cfg.RecorderExe, why)
	}

	// Processes of the user running it.
	var genuine []int
	for _, pid := range pids {
		target, err := c.Proc.Exe(pid)
		if err != nil {
			continue
		}
		path := strings.TrimSuffix(target, deletedSuffix)
		if path != cfg.RecorderExe {
			continue
		}
		running, err := c.Proc.StatExe(pid)
		if err != nil {
			continue
		}
		if !os.SameFile(running, exeInfo) {
			if strings.HasSuffix(target, deletedSuffix) {
				r.fail(ReasonExeMismatch, "pid %d runs a deleted or replaced %s (package upgraded? restart snoopy.service)", pid, path)
			} else {
				r.fail(ReasonExeMismatch, "pid %d runs a different file than %s", pid, path)
			}
			continue
		}
		genuine = append(genuine, pid)
	}
	switch len(genuine) {
	case 0:
		if !slices.Contains(r.Reasons, ReasonExeMismatch) {
			r.fail(ReasonRecorderNotRunning, "no process of uid %d runs %s", cfg.UID, cfg.RecorderExe)
		}
		r.Detail = "snoopy is not running for " + cfg.User
		return r
	case 1:
	default:
		r.fail(ReasonMultipleRecorders, "pids %v all run %s", genuine, cfg.RecorderExe)
	}
	pid := genuine[0]
	r.PID = pid
	r.Running = true
	if st, err := c.Proc.StartTime(pid); err == nil {
		r.Start = st
	}

	if cg, err := c.Proc.Cgroup(pid); err != nil {
		r.fail(ReasonWrongCgroup, "pid %d: %v", pid, err)
	} else {
		parts := strings.Split(strings.Trim(cg, "/"), "/")
		mgr := fmt.Sprintf("user@%d.service", cfg.UID)
		if !slices.Contains(parts, mgr) || parts[len(parts)-1] != cfg.RecorderUnit {
			r.fail(ReasonWrongCgroup, "pid %d is in cgroup %s, want .../%s/.../%s", pid, cg, mgr, cfg.RecorderUnit)
		}
	}

	if env, err := c.Proc.Environ(pid); err != nil {
		r.fail(ReasonLDPreload, "pid %d: cannot read environ: %v", pid, err)
	} else {
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "LD_") {
				r.fail(ReasonLDPreload, "pid %d has %s in its environment", pid, k)
			}
		}
	}

	mode := "managed"
	if !managed {
		mode = "NOT managed"
	}
	r.Detail = fmt.Sprintf("pid %d runs %s (%s), output %s", pid, cfg.RecorderExe, mode, r.OutDir)
	return r
}

// screencastResult is what the GNOME screencast check found.
type screencastResult struct {
	CheckResult
	PID     int
	Segment string // absolute path of the newest segment being written
	Size    int64
	Mtime   time.Time
}

// screencastEnvDenied reports whether an environment variable of the
// screencast service could load foreign code into it.
func screencastEnvDenied(key string) bool {
	if strings.HasPrefix(key, "LD_") || strings.HasPrefix(key, "GST_PLUGIN_") || strings.HasPrefix(key, "GST_REGISTRY") {
		return true
	}
	switch key {
	case "GJS_PATH", "GI_TYPELIB_PATH", "GIO_EXTRA_MODULES", "GTK_MODULES", "GTK_PATH":
		return true
	}
	return false
}

func isVideo(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".webm" || ext == ".mp4"
}

// newestSegment mirrors the recorder's findMostRecentVideo: the .webm/.mp4
// regular file with the latest mtime directly inside dir.
func newestSegment(dir string) (string, os.FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	var best string
	var bestInfo os.FileInfo
	for _, e := range entries {
		if !e.Type().IsRegular() || !isVideo(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if bestInfo == nil || fi.ModTime().After(bestInfo.ModTime()) {
			best, bestInfo = filepath.Join(dir, e.Name()), fi
		}
	}
	if bestInfo == nil {
		return "", nil, fmt.Errorf("no .webm or .mp4 files in %s", dir)
	}
	return best, bestInfo, nil
}

// cmdlineRunsScript accepts "gjs -m SCRIPT" and "gjs SCRIPT", the forms
// GNOME Shell's D-Bus service files use, with no further arguments.
func cmdlineRunsScript(argv []string, script string) bool {
	switch len(argv) {
	case 2:
		return argv[1] == script
	case 3:
		return argv[1] == "-m" && argv[2] == script
	}
	return false
}

func (c *Checker) checkScreencast(pids []int, outDir string) screencastResult {
	cfg := c.Cfg
	r := screencastResult{CheckResult: CheckResult{Name: "screencast", Status: StatusPass}}

	scriptInfo, err := os.Stat(cfg.ScreencastScript)
	if err != nil {
		r.fail(ReasonNoScreencastProcess, "%v", err)
		r.Detail = "GNOME Shell screencast service not installed"
		return r
	}
	if why := c.untrusted(scriptInfo); why != "" {
		r.fail(ReasonScreencastInsecure, "%s: %s", cfg.ScreencastScript, why)
	}
	gjs := map[string]os.FileInfo{}
	for _, p := range cfg.GjsPaths {
		if fi, err := os.Stat(p); err == nil {
			gjs[p] = fi
		}
	}

	pluginDir := filepath.Join(cfg.Home, ".local", "share", "gstreamer-1.0", "plugins")
	if entries, err := os.ReadDir(pluginDir); err == nil && len(entries) > 0 {
		r.fail(ReasonUserGstPlugins, "%s contains %d entries; GStreamer loads them into the screencast service", pluginDir, len(entries))
	}

	resolved, err := filepath.EvalSymlinks(outDir)
	if err != nil {
		resolved = filepath.Clean(outDir)
	}

	var services []int
	type writer struct {
		pid int
		fd  procfs.FD
	}
	var writers []writer
	for _, pid := range pids {
		target, err := c.Proc.Exe(pid)
		if err != nil {
			continue
		}
		onDisk, ok := gjs[target]
		if !ok {
			continue
		}
		argv, err := c.Proc.Cmdline(pid)
		if err != nil || !cmdlineRunsScript(argv, cfg.ScreencastScript) {
			continue
		}
		running, err := c.Proc.StatExe(pid)
		if err != nil || !os.SameFile(running, onDisk) {
			r.fail(ReasonScreencastInsecure, "pid %d runs a different file than %s", pid, target)
			continue
		}
		if why := c.untrusted(onDisk); why != "" {
			r.fail(ReasonScreencastInsecure, "%s: %s", target, why)
			continue
		}
		if env, err := c.Proc.Environ(pid); err != nil {
			r.fail(ReasonScreencastEnv, "pid %d: cannot read environ: %v", pid, err)
			continue
		} else {
			bad := false
			for _, kv := range env {
				if k, _, _ := strings.Cut(kv, "="); screencastEnvDenied(k) {
					r.fail(ReasonScreencastEnv, "pid %d has %s in its environment", pid, k)
					bad = true
				}
			}
			if bad {
				continue
			}
		}
		services = append(services, pid)
		fds, err := c.Proc.FDs(pid)
		if err != nil {
			r.Notes = append(r.Notes, fmt.Sprintf("pid %d: cannot list fds: %v", pid, err))
			continue
		}
		for _, fd := range fds {
			if !fd.Writable() || fd.Info == nil || !fd.Info.Mode().IsRegular() {
				continue
			}
			if strings.HasSuffix(fd.Target, deletedSuffix) || !isVideo(fd.Target) || filepath.Dir(fd.Target) != resolved {
				continue
			}
			writers = append(writers, writer{pid, fd})
		}
	}
	if len(services) == 0 {
		if r.Status == StatusPass {
			r.fail(ReasonNoScreencastProcess, "no process of uid %d runs %s -m %s", cfg.UID, strings.Join(cfg.GjsPaths, "|"), cfg.ScreencastScript)
		}
		r.Detail = "GNOME Shell screencast service is not running"
		return r
	}
	if len(writers) == 0 {
		r.fail(ReasonNoScreencastWriter, "screencast service pid(s) %v hold no file open for writing in %s", services, resolved)
		r.Detail = "the screencast service is not writing into " + outDir
		return r
	}
	newest, newestInfo, err := newestSegment(resolved)
	if err != nil {
		r.fail(ReasonNoSegments, "%v", err)
		r.Detail = "no recorded segments in " + outDir
		return r
	}
	var match *writer
	for i := range writers {
		if os.SameFile(writers[i].fd.Info, newestInfo) {
			match = &writers[i]
			break
		}
	}
	if match == nil {
		r.fail(ReasonWriterNotNewest, "newest segment %s is not the file being written (%s)", filepath.Base(newest), filepath.Base(writers[0].fd.Target))
		r.Detail = "the file being recorded is not the newest segment"
		return r
	}
	r.PID = match.pid
	r.Segment = newest
	r.Size = newestInfo.Size()
	r.Mtime = newestInfo.ModTime()
	age := c.Now().Sub(r.Mtime)
	if age > cfg.SegmentMaxAge {
		r.fail(ReasonSegmentStale, "%s last modified %v ago (limit %v)", filepath.Base(newest), age.Round(time.Second), cfg.SegmentMaxAge)
	}
	r.Detail = fmt.Sprintf("pid %d writing %s (%s, modified %v ago)", match.pid, filepath.Base(newest), humanSize(r.Size), age.Round(time.Second))
	return r
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// rawResults bundles one run of all checks.
type rawResults struct {
	Session    sessionResult
	Recorder   recorderResult
	Screencast screencastResult
	Frame      CheckResult
	FrameBad   bool // frame check failed often enough to count
	FrameWH    []int
	ProcErr    error
}

// run executes the session, recorder and screencast checks.
func (c *Checker) run() rawResults {
	var raw rawResults
	raw.Session = c.checkSession()
	pids, err := c.userProcs(c.Cfg.UID)
	if err != nil {
		raw.ProcErr = err
		raw.Recorder = recorderResult{CheckResult: CheckResult{Name: "recorder"}}
		raw.Recorder.fail(ReasonProcUnavailable, "%v", err)
		raw.Recorder.Detail = "cannot read /proc"
		raw.Screencast = screencastResult{CheckResult: CheckResult{Name: "screencast", Status: StatusSkip, Detail: "cannot read /proc"}}
		return raw
	}
	raw.Recorder = c.checkRecorder(pids)
	raw.Screencast = c.checkScreencast(pids, raw.Recorder.OutDir)
	return raw
}

func (raw *rawResults) evidence() heartbeat.Evidence {
	var e heartbeat.Evidence
	e.RecorderPID = raw.Recorder.PID
	if !raw.Recorder.Start.IsZero() {
		e.RecorderStart = raw.Recorder.Start.UTC().Format(time.RFC3339)
	}
	if raw.Screencast.Segment != "" {
		e.Segment = filepath.Base(raw.Screencast.Segment)
		e.SegmentSize = raw.Screencast.Size
		e.SegmentMtime = raw.Screencast.Mtime.UTC().Format(time.RFC3339)
		e.ScreencastPID = raw.Screencast.PID
	}
	e.FrameWH = raw.FrameWH
	return e
}
