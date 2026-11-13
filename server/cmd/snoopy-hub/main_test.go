package main

import (
	"bytes"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if rc := realMain([]string{"-version"}, env(nil), &out, &errOut); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.HasPrefix(out.String(), "snoopy-hub ") {
		t.Fatalf("output %q", out.String())
	}
}

func TestHelpExitsZeroWithoutPassword(t *testing.T) {
	var out, errOut bytes.Buffer
	rc := realMain([]string{"-help"}, env(map[string]string{"SNOOPY_HUB_PASSWORD": "top-secret-pw"}), &out, &errOut)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(errOut.String()+out.String(), "top-secret-pw") {
		t.Fatal("password printed by -help")
	}
}

func TestInvalidConfigExitsTwo(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
	}{
		{nil, map[string]string{"SNOOPY_HUB_HTTPS_PORT": "many"}},
		{[]string{"-user", "only"}, nil},
		{[]string{"-static", "broken"}, nil},
		{[]string{"-bogus"}, nil},
	} {
		var out, errOut bytes.Buffer
		if rc := realMain(tc.args, env(tc.env), &out, &errOut); rc != 2 {
			t.Errorf("%v %v: rc = %d (%s)", tc.args, tc.env, rc, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	rc := realMain([]string{"-user", "a:b", "-probe-interval", "1ms"}, env(map[string]string{"SNOOPY_HUB_PASSWORD": "pw-in-env-123"}), &out, &errOut)
	if rc != 2 || strings.Contains(errOut.String(), "pw-in-env-123") {
		t.Fatalf("rc %d, output %q", rc, errOut.String())
	}
}
