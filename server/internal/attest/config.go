// Package attest implements snoopy-attest: a root service that inspects a
// supervised user's GNOME session from outside and reports whether Snoopy is
// genuinely recording the screen. See docs/attestation.md.
package attest

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"snoopy/internal/attest/pki"
	"snoopy/internal/envfile"
)

// DefaultConfigFile is read by every subcommand (and by the systemd unit as
// EnvironmentFile=).
const DefaultConfigFile = "/etc/snoopy/attest.conf"

// EnvPrefix prefixes every configuration key.
const EnvPrefix = "SNOOPY_ATTEST_"

// Config is the validated snoopy-attest configuration.
type Config struct {
	DeviceID           string
	User               string
	URL                string
	CAFile             string
	ServerPin          string
	StateDir           string
	RuntimeDir         string
	CheckInterval      time.Duration
	HeartbeatInterval  time.Duration
	FrameCheck         bool
	AllowExtraSessions bool

	// Advanced settings, normally left at their defaults.
	Seat             string        // logind seat to watch
	RecorderExe      string        // packaged recorder binary
	RecorderConf     string        // root-owned recorder configuration (managed mode)
	RecorderUnit     string        // systemd user unit the recorder runs as
	GjsPaths         []string      // accepted executables of the screencast service
	ScreencastScript string        // GNOME Shell screencast service script
	SegmentMaxAge    time.Duration // newest segment must be modified within this window
	StartupGrace     time.Duration // degraded instead of absent after unlock/resume/start
	FrameInterval    time.Duration
	FFmpeg           string
	FrameUser        string // unprivileged account ffmpeg runs as when started by root

	// Resolved from User by Resolve.
	UID  uint32
	GID  uint32
	Home string
}

// key is one configuration key with its default, for documentation and the
// example file test.
type key struct {
	name string
	def  string
}

// Keys lists every configuration key (without prefix) and its default.
var Keys = []key{
	{"DEVICE_ID", "(hostname)"},
	{"USER", ""},
	{"URL", ""},
	{"CA_FILE", ""},
	{"SERVER_PIN", ""},
	{"STATE_DIR", "/var/lib/snoopy-attest"},
	{"RUNTIME_DIR", "/run/snoopy-attest"},
	{"CHECK_INTERVAL", "10s"},
	{"HEARTBEAT_INTERVAL", "15s"},
	{"FRAME_CHECK", "false"},
	{"ALLOW_EXTRA_SESSIONS", "false"},
	{"SEAT", "seat0"},
	{"RECORDER_EXE", "/usr/bin/snoopy"},
	{"RECORDER_CONF", "/etc/snoopy/snoopy.conf"},
	{"RECORDER_UNIT", "snoopy.service"},
	{"GJS_PATHS", "/usr/bin/gjs-console,/usr/bin/gjs"},
	{"SCREENCAST_SCRIPT", "/usr/share/gnome-shell/org.gnome.Shell.Screencast"},
	{"SEGMENT_MAX_AGE", "5m"},
	{"STARTUP_GRACE", "30s"},
	{"FRAME_INTERVAL", "5m"},
	{"FFMPEG", "ffmpeg"},
	{"FRAME_USER", "nobody"},
}

// LoadSettings reads the configuration file at path (a missing file is not
// an error) and overlays SNOOPY_ATTEST_* variables from environ, so values
// passed by systemd's EnvironmentFile= and values read directly agree, and
// root can override a key from a shell for debugging.
func LoadSettings(path string, environ []string) (map[string]string, error) {
	settings := map[string]string{}
	if path != "" {
		m, err := envfile.ParseFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for k, v := range m {
			if strings.HasPrefix(k, EnvPrefix) {
				settings[k] = v
			}
		}
	}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, EnvPrefix) {
			settings[k] = v
		}
	}
	return settings, nil
}

// Parse validates settings into a Config. It does not resolve the user; call
// Resolve for subcommands that need it. Empty values count as unset.
func Parse(settings map[string]string, hostname func() (string, error)) (*Config, error) {
	known := map[string]bool{}
	for _, k := range Keys {
		known[EnvPrefix+k.name] = true
	}
	var errs []error
	for k := range settings {
		if !known[k] {
			errs = append(errs, fmt.Errorf("unknown setting %s", k))
		}
	}
	get := func(name, def string) string {
		if v := strings.TrimSpace(settings[EnvPrefix+name]); v != "" {
			return v
		}
		return def
	}
	dur := func(name, def string, minimum time.Duration) time.Duration {
		s := get(name, def)
		d, err := time.ParseDuration(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s=%q: %w", EnvPrefix, name, s, err))
			return 0
		}
		if d < minimum {
			errs = append(errs, fmt.Errorf("%s%s=%q: must be at least %v", EnvPrefix, name, s, minimum))
		}
		return d
	}
	boolean := func(name string) bool {
		s := get(name, "false")
		b, err := parseBool(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s=%q: %w", EnvPrefix, name, s, err))
		}
		return b
	}
	abs := func(name, def string) string {
		p := get(name, def)
		if p != "" && !filepath.IsAbs(p) {
			errs = append(errs, fmt.Errorf("%s%s=%q: must be an absolute path", EnvPrefix, name, p))
		}
		return p
	}

	c := &Config{
		User:               get("USER", ""),
		URL:                get("URL", ""),
		CAFile:             abs("CA_FILE", ""),
		ServerPin:          get("SERVER_PIN", ""),
		StateDir:           abs("STATE_DIR", "/var/lib/snoopy-attest"),
		RuntimeDir:         abs("RUNTIME_DIR", "/run/snoopy-attest"),
		CheckInterval:      dur("CHECK_INTERVAL", "10s", time.Second),
		HeartbeatInterval:  dur("HEARTBEAT_INTERVAL", "15s", time.Second),
		FrameCheck:         boolean("FRAME_CHECK"),
		AllowExtraSessions: boolean("ALLOW_EXTRA_SESSIONS"),
		Seat:               get("SEAT", "seat0"),
		RecorderExe:        abs("RECORDER_EXE", "/usr/bin/snoopy"),
		RecorderConf:       abs("RECORDER_CONF", "/etc/snoopy/snoopy.conf"),
		RecorderUnit:       get("RECORDER_UNIT", "snoopy.service"),
		ScreencastScript:   abs("SCREENCAST_SCRIPT", "/usr/share/gnome-shell/org.gnome.Shell.Screencast"),
		SegmentMaxAge:      dur("SEGMENT_MAX_AGE", "5m", 30*time.Second),
		StartupGrace:       dur("STARTUP_GRACE", "30s", 0),
		FrameInterval:      dur("FRAME_INTERVAL", "5m", 30*time.Second),
		FFmpeg:             get("FFMPEG", "ffmpeg"),
		FrameUser:          get("FRAME_USER", "nobody"),
	}
	for _, p := range strings.Split(get("GJS_PATHS", "/usr/bin/gjs-console,/usr/bin/gjs"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			errs = append(errs, fmt.Errorf("%sGJS_PATHS: %q must be an absolute path", EnvPrefix, p))
		}
		c.GjsPaths = append(c.GjsPaths, p)
	}
	if len(c.GjsPaths) == 0 {
		errs = append(errs, fmt.Errorf("%sGJS_PATHS: empty", EnvPrefix))
	}

	c.DeviceID = get("DEVICE_ID", "")
	if c.DeviceID == "" && hostname != nil {
		if h, err := hostname(); err == nil {
			c.DeviceID = h
		}
	}
	if c.DeviceID != "" && !validDeviceID(c.DeviceID) {
		errs = append(errs, fmt.Errorf("%sDEVICE_ID=%q: use 1-64 letters, digits, '.', '-' or '_'", EnvPrefix, c.DeviceID))
	}
	if c.URL != "" {
		if !strings.HasPrefix(c.URL, "https://") {
			errs = append(errs, fmt.Errorf("%sURL=%q: must be an https:// URL", EnvPrefix, c.URL))
		}
	}
	if c.ServerPin != "" {
		if err := pki.ValidatePin(c.ServerPin); err != nil {
			errs = append(errs, fmt.Errorf("%sSERVER_PIN: %w", EnvPrefix, err))
		}
	}
	if c.RecorderUnit == "" || strings.Contains(c.RecorderUnit, "/") {
		errs = append(errs, fmt.Errorf("%sRECORDER_UNIT=%q: must be a unit name", EnvPrefix, c.RecorderUnit))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func validDeviceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "", "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean (use true/false, yes/no, on/off or 1/0)")
}

// RequireDevice checks the settings keygen and run need.
func (c *Config) RequireDevice() error {
	if c.DeviceID == "" {
		return fmt.Errorf("%sDEVICE_ID is not set and the hostname is unavailable", EnvPrefix)
	}
	return nil
}

// UserLookup resolves a login name (os/user.Lookup in production).
type UserLookup func(name string) (*user.User, error)

// Resolve resolves User to uid, gid and home directory. The supervised user
// must not be root.
func (c *Config) Resolve(lookup UserLookup) error {
	if c.User == "" {
		return fmt.Errorf("%sUSER is required: the login name of the supervised user", EnvPrefix)
	}
	if lookup == nil {
		lookup = user.Lookup
	}
	u, err := lookup(c.User)
	if err != nil {
		return fmt.Errorf("%sUSER=%q: %w", EnvPrefix, c.User, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("%sUSER=%q: uid %q: %w", EnvPrefix, c.User, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("%sUSER=%q: gid %q: %w", EnvPrefix, c.User, u.Gid, err)
	}
	if uid == 0 {
		return fmt.Errorf("%sUSER=%q: the supervised user cannot be root", EnvPrefix, c.User)
	}
	if !filepath.IsAbs(u.HomeDir) {
		return fmt.Errorf("%sUSER=%q: home directory %q is not absolute", EnvPrefix, c.User, u.HomeDir)
	}
	c.UID, c.GID, c.Home = uint32(uid), uint32(gid), u.HomeDir
	return nil
}
