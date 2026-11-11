// Package envfile parses systemd EnvironmentFile-style files.
//
// The supported syntax is the subset of systemd.exec(5) EnvironmentFile=
// that Snoopy's configuration files use: one KEY=VALUE assignment per line;
// blank lines and lines whose first non-blank character is '#' or ';' are
// ignored; whitespace around the key and the value is trimmed; a value
// enclosed in matching single or double quotes has the quotes removed; a key
// assigned more than once keeps its last value. A non-blank line without '='
// is an error naming its line number.
package envfile

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Parse reads EnvironmentFile-style assignments from r.
func Parse(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '=' in %q", n, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", n)
		}
		if strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d: invalid key %q", n, key)
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("line %d: %w", n+1, err)
	}
	return out, nil
}

// ParseFile opens path and parses it with Parse. Errors name the file.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is a configuration file chosen by the caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' || first == '\'') && first == last {
			return v[1 : len(v)-1]
		}
	}
	return v
}
