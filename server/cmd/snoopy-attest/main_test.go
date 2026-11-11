package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := realMain(args, &out, &errb)
	return rc, out.String(), errb.String()
}

func TestKeygenPin(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "attest.conf")
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(conf, []byte("SNOOPY_ATTEST_DEVICE_ID=desk1\nSNOOPY_ATTEST_STATE_DIR="+state+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, _, errOut := run(t, "-config", conf, "pin")
	if rc != 1 || !strings.Contains(errOut, "keygen") {
		t.Fatalf("pin before keygen: rc=%d err=%q", rc, errOut)
	}
	rc, out, errOut := run(t, "-config", conf, "keygen")
	if rc != 0 || !strings.HasPrefix(out, "sha256:") || !strings.Contains(errOut, `device "desk1"`) {
		t.Fatalf("keygen: rc=%d out=%q err=%q", rc, out, errOut)
	}
	pin := out
	rc, out, _ = run(t, "-config", conf, "pin")
	if rc != 0 || out != pin {
		t.Fatalf("pin: rc=%d %q, want %q", rc, out, pin)
	}
	if rc, _, errOut = run(t, "-config", conf, "keygen"); rc != 1 || !strings.Contains(errOut, "-force") {
		t.Fatalf("second keygen: rc=%d err=%q", rc, errOut)
	}
	if rc, out, _ = run(t, "-config", conf, "keygen", "-force"); rc != 0 || out == pin {
		t.Fatalf("keygen -force: rc=%d out=%q", rc, out)
	}
}

func TestUsageAndErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.conf")
	if rc, out, _ := run(t, "-version"); rc != 0 || !strings.HasPrefix(out, "snoopy-attest ") {
		t.Fatalf("-version: %d %q", rc, out)
	}
	if rc, _, errOut := run(t, "-config", missing, "frobnicate"); rc != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: %d %q", rc, errOut)
	}
	if rc, _, _ := run(t, "-bogus"); rc != 2 {
		t.Fatalf("bad flag: %d", rc)
	}
	if rc, _, errOut := run(t, "-config", missing, "check"); rc != 2 || !strings.Contains(errOut, "SNOOPY_ATTEST_USER is required") {
		t.Fatalf("check without user: %d %q", rc, errOut)
	}
	if rc, _, errOut := run(t, "-config", missing, "run"); rc != 2 || !strings.Contains(errOut, "SNOOPY_ATTEST_USER") {
		t.Fatalf("run without user: %d %q", rc, errOut)
	}
	if rc, _, _ := run(t, "-config", missing, "pin", "extra"); rc != 2 {
		t.Fatalf("pin extra arg: %d", rc)
	}
	bad := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(bad, []byte("SNOOPY_ATTEST_CHECK_INTERVAL=often\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, _, errOut := run(t, "-config", bad, "pin"); rc != 2 || !strings.Contains(errOut, "CHECK_INTERVAL") {
		t.Fatalf("invalid config: %d %q", rc, errOut)
	}
	if rc, _, _ := run(t, "-config", missing, "help"); rc != 0 {
		t.Fatalf("help: %d", rc)
	}
}
