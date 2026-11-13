package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"snoopy/internal/envfile"
)

// managedConfigPath is the system-wide configuration file. Snoopy reads it
// itself (besides systemd's EnvironmentFile=) to decide on managed mode. It
// is a variable only so tests can point it elsewhere; there is deliberately
// no flag or environment variable for it.
var managedConfigPath = "/etc/snoopy/snoopy.conf"

// managedKey enables managed mode when set to a true value in
// managedConfigPath.
const managedKey = envPrefix + "MANAGED"

// envPrefix is prepended to a flag's name (upper-cased, "-" replaced by "_")
// to form the environment variable that supplies its default.
const envPrefix = "SNOOPY_"

// envName returns the environment variable for flag name, e.g.
// "image-interval" -> "SNOOPY_IMAGE_INTERVAL".
func envName(name string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// usage appends the flag's environment variable to its help text.
func usage(name, text string) string {
	return fmt.Sprintf("%s (env %s)", text, envName(name))
}

// envDefaults computes flag defaults from the environment, falling back to
// the built-in default when the variable is unset or empty. Parse errors are
// collected and reported together by err, so the caller can exit with the
// invalid-flag status.
type envDefaults struct {
	lookup func(string) (string, bool)
	errs   []error
}

func newEnvDefaults(lookup func(string) (string, bool)) *envDefaults {
	return &envDefaults{lookup: lookup}
}

func (e *envDefaults) value(name string) (string, string, bool) {
	key := envName(name)
	v, ok := e.lookup(key)
	v = strings.TrimSpace(v)
	return key, v, ok && v != ""
}

func (e *envDefaults) string(name, def string) string {
	if _, v, ok := e.value(name); ok {
		return v
	}
	return def
}

func (e *envDefaults) duration(name string, def time.Duration) time.Duration {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q: not a duration (e.g. 30s, 5m, 1h)", key, v))
		return def
	}
	return d
}

func (e *envDefaults) int(name string, def int) int {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q: not an integer", key, v))
		return def
	}
	return n
}

// bool parses a boolean variable with the same spellings as SNOOPY_MANAGED
// (1, true, yes / 0, false, no, any case).
func (e *envDefaults) bool(name string, def bool) bool {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	b, valid := parseBool(v)
	if !valid {
		e.errs = append(e.errs, fmt.Errorf("%s=%q: not a boolean (1, true, yes / 0, false, no)", key, v))
		return def
	}
	return b
}

// parseBool accepts 1, true, yes and 0, false, no (any case, surrounding
// space ignored). The empty string is false.
func parseBool(v string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true, true
	case "", "0", "false", "no":
		return false, true
	}
	return false, false
}

// err reports every invalid variable on a single line, or nil.
func (e *envDefaults) err() error {
	if len(e.errs) == 0 {
		return nil
	}
	msgs := make([]string, len(e.errs))
	for i, err := range e.errs {
		msgs[i] = err.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}

// managedFile is the parsed system-wide configuration file and the metadata
// that decides whether it may enable managed mode.
type managedFile struct {
	path   string
	values map[string]string // nil: no file
	uid    uint32
	mode   fs.FileMode
	err    error // read or parse failure (reported after -version/-help)
}

// loadManagedFile opens path, takes ownership and permissions from the open
// file (so they describe exactly what was parsed) and parses it. A missing
// file yields an empty managedFile; read and parse failures are kept in err
// for resolveConfig to report.
func loadManagedFile(path string) managedFile {
	mf := managedFile{path: path}
	f, err := os.Open(path) //nolint:gosec // fixed system configuration path
	if errors.Is(err, fs.ErrNotExist) {
		return mf
	}
	if err != nil {
		mf.err = err
		return mf
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		mf.err = err
		return mf
	}
	mf.mode = fi.Mode()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		mf.uid = st.Uid
	} else {
		mf.uid = ^uint32(0)
	}
	values, err := envfile.Parse(f)
	if err != nil {
		mf.err = fmt.Errorf("%s: %w", path, err)
		return mf
	}
	mf.values = values
	return mf
}

// resolved is the effective configuration chosen by resolveConfig.
type resolved struct {
	opts        options
	showVersion bool
	managed     bool
	ignored     []string // user-supplied settings ignored in managed mode (names only)
	warnings    []string
}

// parseManagedSwitch parses the SNOOPY_MANAGED value.
func parseManagedSwitch(v string) (bool, error) {
	if b, ok := parseBool(v); ok {
		return b, nil
	}
	return false, fmt.Errorf("%s=%q: not a boolean (1, true, yes / 0, false, no)", managedKey, v)
}

// resolveConfig builds the effective options from the system-wide file, the
// environment and the command line. It is pure apart from writing -help
// output to out.
//
// Unmanaged (the default): flag > SNOOPY_* environment > built-in default.
// The environment is what systemd built from /etc/snoopy/snoopy.conf and
// then ~/.config/snoopy/snoopy.conf.
//
// Managed (SNOOPY_MANAGED true in mf, and mf owned by root and not group- or
// world-writable): only the SNOOPY_* keys of mf and built-in defaults count.
// Environment variables whose value differs from the file and flags given
// on the command line are reported in ignored and have no effect; -version
// and -help still work.
//
// Errors wrap flag.ErrHelp for -help; any other error means exit status 2.
func resolveConfig(mf managedFile, lookupEnv func(string) (string, bool), args []string, out io.Writer) (resolved, error) {
	var res resolved

	managed := false
	var managedErr error
	if mf.values != nil {
		on, err := parseManagedSwitch(mf.values[managedKey])
		switch {
		case err != nil:
			managedErr = fmt.Errorf("invalid %s: %w", mf.path, err)
		case on && mf.uid != 0:
			res.warnings = append(res.warnings, fmt.Sprintf("%s sets %s but is not owned by root (uid %d); running unmanaged", mf.path, managedKey, mf.uid))
		case on && mf.mode&0o022 != 0:
			res.warnings = append(res.warnings, fmt.Sprintf("%s sets %s but is group- or world-writable (mode %04o); running unmanaged", mf.path, managedKey, mf.mode.Perm()))
		case on && !mf.mode.IsRegular():
			res.warnings = append(res.warnings, fmt.Sprintf("%s sets %s but is not a regular file; running unmanaged", mf.path, managedKey))
		default:
			managed = on
		}
	}

	lookup := lookupEnv
	if managed {
		lookup = func(k string) (string, bool) { v, ok := mf.values[k]; return v, ok }
	}
	env := newEnvDefaults(lookup)

	fset := flag.NewFlagSet("snoopy", flag.ContinueOnError)
	fset.SetOutput(out)
	var (
		o           options
		showVersion bool
	)
	fset.StringVar(&o.outDir, "out", env.string("out", ""), usage("out", "Output directory (empty: ~/.cache/snoopy/video)"))
	fset.DurationVar(&o.segment, "segment", env.duration("segment", 30*time.Minute), usage("segment", "Segment duration"))
	fset.DurationVar(&o.pause, "pause", env.duration("pause", 1*time.Second), usage("pause", "Pause between segments"))
	fset.StringVar(&o.fileTemplate, "template", env.string("template", "screen-%d-%t.webm"), usage("template", "Filename template used by GNOME Shell"))
	fset.StringVar(&o.addr, "addr", env.string("addr", "127.0.0.1"), usage("addr", "HTTP server bind address (0.0.0.0 exposes it on the LAN)"))
	fset.IntVar(&o.port, "port", env.int("port", 8900), usage("port", "HTTP server port"))
	fset.DurationVar(&o.imageInterval, "image-interval", env.duration("image-interval", 5*time.Second), usage("image-interval", "Interval between screen captures for web streaming"))
	fset.IntVar(&o.imageCacheSize, "image-cache-size", env.int("image-cache-size", 100), usage("image-cache-size", "Maximum number of images to keep in cache"))
	fset.DurationVar(&o.healthCheck, "health-check", env.duration("health-check", 15*time.Second), usage("health-check", "Interval for DBus connection health checks"))
	fset.DurationVar(&o.stallTimeout, "stall-timeout", env.duration("stall-timeout", 5*time.Minute), usage("stall-timeout", "Restart the segment if its file has not grown for this long (0 disables)"))
	fset.BoolVar(&o.rtsp.enabled, "rtsp", env.bool("rtsp", false), usage("rtsp", "Broadcast the captured frames as an RTSP stream (MPEG-4) and advertise it over mDNS"))
	fset.StringVar(&o.rtsp.addr, "rtsp-addr", env.string("rtsp-addr", "0.0.0.0"), usage("rtsp-addr", "RTSP server bind address (independent of -addr)"))
	fset.IntVar(&o.rtsp.port, "rtsp-port", env.int("rtsp-port", 8554), usage("rtsp-port", "RTSP server port (TCP)"))
	fset.StringVar(&o.rtsp.codec, "rtsp-codec", env.string("rtsp-codec", rtspCodecAuto), usage("rtsp-codec", "RTSP video codec: auto (MPEG-4 Part 2 if available, else H.264), mpeg4 or h264"))
	fset.StringVar(&o.rtsp.user, "rtsp-user", env.string("rtsp-user", ""), usage("rtsp-user", "RTSP user name readers must authenticate with (requires -rtsp-password)"))
	// The password's default is applied after parsing so that -help never
	// prints it.
	fset.StringVar(&o.rtsp.password, "rtsp-password", "", usage("rtsp-password", "RTSP password (requires -rtsp-user; prefer the environment variable, command lines are visible to other local users)"))
	envPassword := env.string("rtsp-password", "")
	fset.IntVar(&o.rtsp.maxHeight, "rtsp-max-height", env.int("rtsp-max-height", 1080), usage("rtsp-max-height", "Downscale RTSP frames taller than this many pixels (0 keeps the source size)"))
	fset.BoolVar(&showVersion, "version", false, "Print version information and exit")
	if managed {
		fset.Usage = func() {
			_, _ = fmt.Fprintf(out, "Usage of snoopy (managed by %s: flags and SNOOPY_* environment are ignored):\n", mf.path)
			fset.PrintDefaults()
		}
	}

	if err := fset.Parse(args); err != nil {
		return res, err
	}
	if showVersion {
		res.showVersion = true
		return res, nil
	}
	if fset.NArg() > 0 {
		return res, fmt.Errorf("unexpected arguments: %s", strings.Join(fset.Args(), " "))
	}
	if managedErr != nil {
		return res, managedErr
	}
	if mf.err != nil {
		if errors.Is(mf.err, fs.ErrPermission) {
			// systemd (running as the user) cannot read it either.
			res.warnings = append(res.warnings, fmt.Sprintf("cannot read %s: %v; running unmanaged", mf.path, mf.err))
		} else {
			return res, fmt.Errorf("invalid %s: %w", mf.path, mf.err)
		}
	}

	if managed {
		// Undo any flag given on the command line.
		fset.Visit(func(f *flag.Flag) {
			if f.Name == "version" {
				return
			}
			res.ignored = append(res.ignored, "-"+f.Name)
			_ = f.Value.Set(f.DefValue)
		})
		// Report environment settings that would have changed something.
		fset.VisitAll(func(f *flag.Flag) {
			if f.Name == "version" {
				return
			}
			key := envName(f.Name)
			v, ok := lookupEnv(key)
			v = strings.TrimSpace(v)
			if !ok || v == "" {
				return
			}
			if fv, inFile := mf.values[key]; inFile && strings.TrimSpace(fv) == v {
				return // systemd loaded the same value from the managed file
			}
			res.ignored = append(res.ignored, key)
		})
		sort.Strings(res.ignored)
	}

	if o.rtsp.password == "" {
		o.rtsp.password = envPassword
	}

	if err := env.err(); err != nil {
		if managed {
			return res, fmt.Errorf("invalid %s: %w", mf.path, err)
		}
		return res, fmt.Errorf("invalid environment: %w", err)
	}
	if o.segment <= 0 || o.healthCheck <= 0 || o.imageInterval <= 0 || o.pause < 0 || o.stallTimeout < 0 {
		return res, errors.New("-segment, -health-check and -image-interval must be positive and -pause and -stall-timeout must not be negative")
	}

	if err := o.rtsp.validate(); err != nil {
		return res, err
	}

	res.opts = o
	res.managed = managed
	return res, nil
}
