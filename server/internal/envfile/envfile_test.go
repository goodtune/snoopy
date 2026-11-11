package envfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string
	}{
		{name: "empty", in: "", want: map[string]string{}},
		{name: "simple", in: "A=1\nB=two\n", want: map[string]string{"A": "1", "B": "two"}},
		{name: "comments and blanks", in: "# c\n\n; c2\n   # indented\nA=1\n", want: map[string]string{"A": "1"}},
		{name: "whitespace trimmed", in: "  A  =  hello world  \n", want: map[string]string{"A": "hello world"}},
		{name: "double quotes", in: `A="x y"`, want: map[string]string{"A": "x y"}},
		{name: "single quotes", in: `A='x y'`, want: map[string]string{"A": "x y"}},
		{name: "quoted with spaces outside", in: `A = " padded " `, want: map[string]string{"A": " padded "}},
		{name: "mismatched quotes kept", in: `A="x'`, want: map[string]string{"A": `"x'`}},
		{name: "lone quote kept", in: `A="`, want: map[string]string{"A": `"`}},
		{name: "empty value", in: "A=\nB=''\n", want: map[string]string{"A": "", "B": ""}},
		{name: "later overrides", in: "A=1\nA=2\n", want: map[string]string{"A": "2"}},
		{name: "equals in value", in: "A=b=c\n", want: map[string]string{"A": "b=c"}},
		{name: "crlf", in: "A=1\r\nB=2\r\n", want: map[string]string{"A": "1", "B": "2"}},
		{name: "bom", in: "\xef\xbb\xbfA=1\n", want: map[string]string{"A": "1"}},
		{name: "missing equals", in: "A=1\n\nbogus\n", wantErr: "line 3"},
		{name: "empty key", in: "=1\n", wantErr: "line 1: empty key"},
		{name: "key with space", in: "A B=1\n", wantErr: "line 1: invalid key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.conf")
	if err := os.WriteFile(p, []byte("SNOOPY_MANAGED=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseFile(p)
	if err != nil || m["SNOOPY_MANAGED"] != "1" {
		t.Fatalf("got %v, %v", m, err)
	}
	if err := os.WriteFile(p, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(p); err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v, want file name and line", err)
	}
	if _, err := ParseFile(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}
