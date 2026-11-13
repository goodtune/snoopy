package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DefaultPort is snoopy's HTTP port, used when a static entry names none.
const DefaultPort = 8900

const maxSlug = 40

// ID returns the stable, URL-safe id of the service called name on host:
// a readable slug of both (lower-case a-z, 0-9 and "-", at most 40
// characters) plus 6 hex digits of a hash of the exact name and host, so
// different services never share an id however their names are mangled.
func ID(name, host string) string {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	short := strings.TrimSuffix(h, ".local")
	s := slugify(name)
	if hs := slugify(short); hs != "" && hs != s {
		if s != "" {
			s += "-"
		}
		s += hs
	}
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "-")
	}
	if s == "" {
		s = "screen"
	}
	sum := sha256.Sum256([]byte(name + "\x00" + h))
	return s + "-" + hex.EncodeToString(sum[:3])
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidHost reports whether h is an IP literal or a syntactically valid DNS
// host name (letters, digits and hyphens in labels of at most 63
// characters, at most 253 in all).
func ValidHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// StaticEntry is one configured service.
type StaticEntry struct {
	Name     string
	Host     string
	Port     int
	RTSPPort int
}

// ParseStatic parses SNOOPY_HUB_STATIC: a comma-separated list of
// name=host[:port][;rtsp=PORT]. The port defaults to 8900; an IPv6 address
// needs brackets ([fd00::5]:8900). Names must be unique, 1-63 characters
// without control characters, ",", "=" or ";".
func ParseStatic(s string) ([]StaticEntry, error) {
	var out []StaticEntry
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		e, err := parseStaticEntry(item)
		if err != nil {
			return nil, fmt.Errorf("static entry %q: %w", item, err)
		}
		if seen[strings.ToLower(e.Name)] {
			return nil, fmt.Errorf("static entry %q: duplicate name", item)
		}
		seen[strings.ToLower(e.Name)] = true
		out = append(out, e)
	}
	return out, nil
}

func parseStaticEntry(item string) (StaticEntry, error) {
	var e StaticEntry
	name, rest, ok := strings.Cut(item, "=")
	if !ok {
		return e, errors.New("want name=host[:port][;rtsp=PORT]")
	}
	e.Name = strings.TrimSpace(name)
	if e.Name == "" || len(e.Name) > 63 || strings.ContainsAny(e.Name, ";") ||
		strings.IndexFunc(e.Name, unicode.IsControl) >= 0 {
		return e, errors.New("name must be 1-63 characters without control characters, \",\", \"=\" or \";\"")
	}
	parts := strings.Split(rest, ";")
	hostport := strings.TrimSpace(parts[0])
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port given.
		host, port = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"), ""
		if strings.Count(host, ":") == 1 {
			return e, fmt.Errorf("invalid host:port %q", hostport)
		}
	}
	if strings.Contains(host, "%") || !ValidHost(host) {
		return e, fmt.Errorf("invalid host %q", host)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	e.Host = host
	e.Port = DefaultPort
	if port != "" {
		if e.Port, err = parsePort(port); err != nil {
			return e, err
		}
	}
	for _, opt := range parts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(opt), "=")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "rtsp":
			if e.RTSPPort, err = parsePort(strings.TrimSpace(v)); err != nil {
				return e, fmt.Errorf("rtsp: %w", err)
			}
		default:
			return e, fmt.Errorf("unknown option %q (only rtsp=PORT)", k)
		}
	}
	return e, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}
