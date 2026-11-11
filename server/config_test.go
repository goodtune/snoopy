package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func rootFile(values map[string]string) managedFile {
	return managedFile{path: "/etc/snoopy/snoopy.conf", values: values, uid: 0, mode: 0o644}
}

func TestResolveConfigManagedWins(t *testing.T) {
	mf := rootFile(map[string]string{
		"SNOOPY_MANAGED":       "Yes",
		"SNOOPY_OUT":           "/srv/snoopy",
		"SNOOPY_SEGMENT":       "10m",
		"SNOOPY_STALL_TIMEOUT": "2m",
	})
	env := map[string]string{
		// systemd loaded the managed file into the environment too.
		"SNOOPY_MANAGED": "Yes",
		"SNOOPY_SEGMENT": "10m",
		// ~/.config/snoopy/snoopy.conf overrides: must be ignored.
		"SNOOPY_OUT":          "/home/kid/tmp",
		"SNOOPY_ADDR":         "0.0.0.0",
		"SNOOPY_PAUSE":        "",               // empty counts as unset
		"SNOOPY_HEALTH_CHECK": "not-a-duration", // ignored, so not an error
	}
	res, err := resolveConfig(mf, lookupMap(env), []string{"-segment", "1h", "-port=1"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.managed {
		t.Fatal("managed = false, want true")
	}
	o := res.opts
	if o.outDir != "/srv/snoopy" || o.segment != 10*time.Minute || o.stallTimeout != 2*time.Minute {
		t.Errorf("file values not applied: %+v", o)
	}
	if o.addr != "127.0.0.1" || o.port != 8900 || o.healthCheck != 15*time.Second || o.pause != time.Second {
		t.Errorf("built-in defaults not applied: %+v", o)
	}
	want := []string{"-port", "-segment", "SNOOPY_ADDR", "SNOOPY_HEALTH_CHECK", "SNOOPY_OUT"}
	if !reflect.DeepEqual(res.ignored, want) {
		t.Errorf("ignored = %v, want %v", res.ignored, want)
	}
	if len(res.warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.warnings)
	}
}

func TestResolveConfigManagedNotTrusted(t *testing.T) {
	values := map[string]string{"SNOOPY_MANAGED": "1", "SNOOPY_OUT": "/srv/snoopy"}
	env := map[string]string{"SNOOPY_OUT": "/home/kid/tmp"}
	for name, mf := range map[string]managedFile{
		"group-writable": {path: "/etc/snoopy/snoopy.conf", values: values, uid: 0, mode: 0o664},
		"world-writable": {path: "/etc/snoopy/snoopy.conf", values: values, uid: 0, mode: 0o646},
		"not root":       {path: "/etc/snoopy/snoopy.conf", values: values, uid: 1000, mode: 0o644},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := resolveConfig(mf, lookupMap(env), nil, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if res.managed {
				t.Error("managed = true, want false")
			}
			if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "running unmanaged") {
				t.Errorf("warnings = %v, want one 'running unmanaged' warning", res.warnings)
			}
			if res.opts.outDir != "/home/kid/tmp" {
				t.Errorf("outDir = %q, want env value (unmanaged precedence)", res.opts.outDir)
			}
		})
	}
}

func TestResolveConfigUnmanaged(t *testing.T) {
	env := map[string]string{"SNOOPY_OUT": "/home/me/rec", "SNOOPY_SEGMENT": "5m", "SNOOPY_PORT": "9000"}
	for name, mf := range map[string]managedFile{
		"no file":      {path: "/etc/snoopy/snoopy.conf"},
		"managed off":  rootFile(map[string]string{"SNOOPY_MANAGED": "0", "SNOOPY_OUT": "/srv"}),
		"managed no":   rootFile(map[string]string{"SNOOPY_MANAGED": "no"}),
		"key absent":   rootFile(map[string]string{"SNOOPY_ADDR": "0.0.0.0"}),
		"empty switch": rootFile(map[string]string{"SNOOPY_MANAGED": ""}),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := resolveConfig(mf, lookupMap(env), []string{"-port", "9100"}, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if res.managed || len(res.ignored) != 0 || len(res.warnings) != 0 {
				t.Errorf("res = %+v, want plain unmanaged", res)
			}
			o := res.opts
			if o.outDir != "/home/me/rec" || o.segment != 5*time.Minute || o.port != 9100 || o.stallTimeout != 5*time.Minute {
				t.Errorf("flag > env > default not honoured: %+v", o)
			}
		})
	}
}

func TestResolveConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		mf   managedFile
		env  map[string]string
		args []string
		want string
	}{
		{"invalid managed value", rootFile(map[string]string{"SNOOPY_MANAGED": "1", "SNOOPY_SEGMENT": "soon"}), nil, nil, `/etc/snoopy/snoopy.conf: SNOOPY_SEGMENT="soon"`},
		{"invalid switch", rootFile(map[string]string{"SNOOPY_MANAGED": "maybe"}), nil, nil, `SNOOPY_MANAGED="maybe"`},
		{"invalid env", managedFile{}, map[string]string{"SNOOPY_STALL_TIMEOUT": "x"}, nil, "invalid environment"},
		{"negative stall timeout", managedFile{}, map[string]string{"SNOOPY_STALL_TIMEOUT": "-1m"}, nil, "-stall-timeout"},
		{"unparsable file", managedFile{path: "/etc/snoopy/snoopy.conf", err: errors.New("line 3: missing '='")}, nil, nil, "line 3"},
		{"bad flag", managedFile{}, nil, []string{"-nope"}, "-nope"},
		{"stray argument", managedFile{}, nil, []string{"extra"}, "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveConfig(tt.mf, lookupMap(tt.env), tt.args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if errors.Is(err, flag.ErrHelp) {
				t.Fatal("unexpected ErrHelp")
			}
		})
	}
}

func TestResolveConfigUnreadableFileWarns(t *testing.T) {
	mf := managedFile{path: "/etc/snoopy/snoopy.conf", err: os.ErrPermission}
	res, err := resolveConfig(mf, lookupMap(nil), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if res.managed || len(res.warnings) != 1 {
		t.Errorf("res = %+v, want unmanaged with one warning", res)
	}
}

func TestResolveConfigVersionAndHelp(t *testing.T) {
	bad := rootFile(map[string]string{"SNOOPY_MANAGED": "1", "SNOOPY_SEGMENT": "soon"})

	res, err := resolveConfig(bad, lookupMap(nil), []string{"-version"}, &bytes.Buffer{})
	if err != nil || !res.showVersion {
		t.Errorf("-version: res.showVersion=%v err=%v; want true, nil", res.showVersion, err)
	}

	var out bytes.Buffer
	_, err = resolveConfig(rootFile(map[string]string{"SNOOPY_MANAGED": "1", "SNOOPY_SEGMENT": "10m"}), lookupMap(nil), []string{"-help"}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-help: err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"managed by /etc/snoopy/snoopy.conf", "-stall-timeout", "SNOOPY_STALL_TIMEOUT", "10m0s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLoadManagedFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing", func(t *testing.T) {
		mf := loadManagedFile(filepath.Join(dir, "nope.conf"))
		if mf.values != nil || mf.err != nil {
			t.Errorf("mf = %+v, want empty", mf)
		}
	})

	t.Run("parsed with owner and mode", func(t *testing.T) {
		p := filepath.Join(dir, "snoopy.conf")
		content := "# managed\nSNOOPY_MANAGED=1\n; comment\n\nSNOOPY_OUT=\"/srv/snoopy\"\n"
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
		mf := loadManagedFile(p)
		if mf.err != nil {
			t.Fatal(mf.err)
		}
		if mf.values["SNOOPY_MANAGED"] != "1" || mf.values["SNOOPY_OUT"] != "/srv/snoopy" {
			t.Errorf("values = %v", mf.values)
		}
		if mf.mode.Perm() != 0o640 || !mf.mode.IsRegular() {
			t.Errorf("mode = %v, want regular 0640", mf.mode)
		}
		if mf.uid != uint32(os.Getuid()) {
			t.Errorf("uid = %d, want %d", mf.uid, os.Getuid())
		}
	})

	t.Run("unparsable", func(t *testing.T) {
		p := filepath.Join(dir, "bad.conf")
		if err := os.WriteFile(p, []byte("SNOOPY_MANAGED=1\nbogus line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if mf := loadManagedFile(p); mf.err == nil || mf.values != nil {
			t.Errorf("mf = %+v, want parse error", mf)
		}
	})
}

// TestParseManagedSwitchTable pins the SNOOPY_MANAGED truth table. The
// attestor's recorder check (internal/attest/checks.go, managedSwitchOn)
// has the same table in TestManagedSwitchOnAgreesWithRecorder; change both.
func TestParseManagedSwitchTable(t *testing.T) {
	on := []string{"1", "true", "yes", "TRUE", "Yes", " 1 "}
	off := []string{"", "0", "false", "no", "FALSE", " no "}
	invalid := []string{"on", "y", "2", "enabled"}
	for _, v := range on {
		if got, err := parseManagedSwitch(v); err != nil || !got {
			t.Errorf("parseManagedSwitch(%q) = %v, %v; want true, nil", v, got, err)
		}
	}
	for _, v := range off {
		if got, err := parseManagedSwitch(v); err != nil || got {
			t.Errorf("parseManagedSwitch(%q) = %v, %v; want false, nil", v, got, err)
		}
	}
	for _, v := range invalid {
		if _, err := parseManagedSwitch(v); err == nil {
			t.Errorf("parseManagedSwitch(%q): want error", v)
		}
	}
}
