package attest

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"snoopy/internal/envfile"
)

func hostname() (string, error) { return "desk-host", nil }

func TestParseDefaults(t *testing.T) {
	c, err := Parse(map[string]string{"SNOOPY_ATTEST_USER": "kid1"}, hostname)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID != "desk-host" || c.User != "kid1" || c.URL != "" || c.StateDir != "/var/lib/snoopy-attest" ||
		c.RuntimeDir != "/run/snoopy-attest" || c.CheckInterval != 10*time.Second || c.HeartbeatInterval != 15*time.Second ||
		c.FrameCheck || c.AllowExtraSessions || c.Seat != "seat0" || c.RecorderExe != "/usr/bin/snoopy" ||
		c.RecorderConf != "/etc/snoopy/snoopy.conf" || c.RecorderUnit != "snoopy.service" ||
		len(c.GjsPaths) != 2 || c.GjsPaths[0] != "/usr/bin/gjs-console" ||
		c.ScreencastScript != "/usr/share/gnome-shell/org.gnome.Shell.Screencast" || c.SegmentMaxAge != 5*time.Minute ||
		c.StartupGrace != 30*time.Second || c.FrameInterval != 5*time.Minute || c.FFmpeg != "ffmpeg" || c.FrameUser != "nobody" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestParseValues(t *testing.T) {
	pin := "sha256:" + strings.Repeat("A", 43) + "="
	c, err := Parse(map[string]string{
		"SNOOPY_ATTEST_DEVICE_ID":            "kids-desk_1",
		"SNOOPY_ATTEST_URL":                  "https://kproxy.example.lan:8443/v1/presence",
		"SNOOPY_ATTEST_CA_FILE":              "/etc/snoopy/kproxy-ca.pem",
		"SNOOPY_ATTEST_SERVER_PIN":           pin,
		"SNOOPY_ATTEST_CHECK_INTERVAL":       "5s",
		"SNOOPY_ATTEST_HEARTBEAT_INTERVAL":   "20s",
		"SNOOPY_ATTEST_FRAME_CHECK":          "yes",
		"SNOOPY_ATTEST_ALLOW_EXTRA_SESSIONS": "1",
		"SNOOPY_ATTEST_GJS_PATHS":            " /opt/gjs , ",
		"SNOOPY_ATTEST_STATE_DIR":            "",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID != "kids-desk_1" || c.ServerPin != pin || c.CheckInterval != 5*time.Second || !c.FrameCheck ||
		!c.AllowExtraSessions || len(c.GjsPaths) != 1 || c.GjsPaths[0] != "/opt/gjs" || c.StateDir != "/var/lib/snoopy-attest" {
		t.Fatalf("%+v", c)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"SNOOPY_ATTEST_URL=http://x":                  "https://",
		"SNOOPY_ATTEST_CHECK_INTERVAL=soon":           "CHECK_INTERVAL",
		"SNOOPY_ATTEST_HEARTBEAT_INTERVAL=10ms":       "at least",
		"SNOOPY_ATTEST_FRAME_CHECK=maybe":             "not a boolean",
		"SNOOPY_ATTEST_SERVER_PIN=abc":                "SERVER_PIN",
		"SNOOPY_ATTEST_DEVICE_ID=has space":           "DEVICE_ID",
		"SNOOPY_ATTEST_STATE_DIR=relative":            "absolute",
		"SNOOPY_ATTEST_GJS_PATHS=gjs":                 "absolute",
		"SNOOPY_ATTEST_RECORDER_UNIT=a/b":             "RECORDER_UNIT",
		"SNOOPY_ATTEST_TYPO=1":                        "unknown setting",
		"SNOOPY_ATTEST_SEGMENT_MAX_AGE=1s":            "at least",
		"SNOOPY_ATTEST_ALLOW_EXTRA_SESSIONS=sometime": "ALLOW_EXTRA_SESSIONS",
	}
	for kv, want := range tests {
		k, v, _ := strings.Cut(kv, "=")
		_, err := Parse(map[string]string{k: v}, hostname)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want containing %q", kv, err, want)
		}
	}
}

func TestLoadSettings(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "attest.conf")
	writeFile(t, p, "# comment\nSNOOPY_ATTEST_USER=kid1\nSNOOPY_ATTEST_URL='https://a/'\nOTHER=x\n", 0o644)
	s, err := LoadSettings(p, []string{"SNOOPY_ATTEST_URL=https://b/", "HOME=/root"})
	if err != nil {
		t.Fatal(err)
	}
	if s["SNOOPY_ATTEST_USER"] != "kid1" || s["SNOOPY_ATTEST_URL"] != "https://b/" || len(s) != 2 {
		t.Fatalf("%v", s)
	}
	if s, err := LoadSettings(filepath.Join(dir, "missing"), nil); err != nil || len(s) != 0 {
		t.Fatalf("missing file: %v %v", s, err)
	}
	writeFile(t, p, "broken\n", 0o644)
	if _, err := LoadSettings(p, nil); err == nil {
		t.Fatal("broken file accepted")
	}
}

func TestResolve(t *testing.T) {
	lookup := func(name string) (*user.User, error) {
		switch name {
		case "kid1":
			return &user.User{Uid: "1000", Gid: "1000", HomeDir: "/home/kid1"}, nil
		case "root":
			return &user.User{Uid: "0", Gid: "0", HomeDir: "/root"}, nil
		}
		return nil, user.UnknownUserError(name)
	}
	c := &Config{User: "kid1"}
	if err := c.Resolve(lookup); err != nil || c.UID != 1000 || c.Home != "/home/kid1" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, name := range []string{"", "root", "ghost"} {
		if err := (&Config{User: name}).Resolve(lookup); err == nil {
			t.Errorf("Resolve(%q) succeeded", name)
		}
	}
	var unk user.UnknownUserError
	if err := (&Config{User: "ghost"}).Resolve(lookup); !errors.As(err, &unk) {
		t.Errorf("err = %v", err)
	}
	if err := (&Config{}).RequireDevice(); err == nil {
		t.Error("RequireDevice with empty id succeeded")
	}
}

// TestExampleConfig keeps server/attest.conf.example in sync with Keys: every
// key is documented (commented out, with its default) and the file parses.
func TestExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "attest.conf.example")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envfile.ParseFile(path); err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, k := range Keys {
		if !strings.Contains(text, EnvPrefix+k.name+"=") {
			t.Errorf("%s%s is not documented in %s", EnvPrefix, k.name, path)
		}
		if k.def != "" && !strings.HasPrefix(k.def, "(") && !strings.Contains(text, EnvPrefix+k.name+"="+k.def) {
			t.Errorf("%s%s: default %q not shown in %s", EnvPrefix, k.name, k.def, path)
		}
	}
	// Uncommented assignments must be valid settings.
	m, _ := envfile.ParseFile(path)
	if _, err := Parse(m, hostname); err != nil {
		t.Errorf("example settings invalid: %v", err)
	}
}
