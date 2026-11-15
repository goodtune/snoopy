// Package discovery finds snoopy services on the local network. A Browser
// reports resolved sightings of `_snoopy._tcp` instances (one per interface
// and protocol an instance is seen on); Supervise keeps a Browser running
// and turns its failures into Reset events, so an unavailable mDNS daemon is
// never fatal. The Avahi D-Bus implementation is in avahi.go; tests use
// fakes.
package discovery

import (
	"context"
	"strings"
	"time"
)

// ServiceType is the DNS-SD service type snoopy advertises.
const ServiceType = "_snoopy._tcp"

// Key identifies one sighting: an instance name seen on one interface with
// one protocol. The same instance usually has several (IPv4 and IPv6, more
// than one interface).
type Key struct {
	Interface int32
	Protocol  int32
	Name      string
	Domain    string
}

// Sighting is a resolved sighting. Every string comes from the network and
// is untrusted.
type Sighting struct {
	Key     Key
	Name    string            // instance name, e.g. "alice"
	Host    string            // host name, e.g. "alice-pc.local"
	Address string            // IP literal (with zone for IPv6 link-local)
	Port    int               // HTTP port
	TXT     map[string]string // parsed TXT records (keys lower-cased)
}

// Kind is the kind of an Event.
type Kind int

const (
	// Found reports a new or re-resolved sighting.
	Found Kind = iota
	// Lost reports that a sighting was withdrawn.
	Lost
	// Reset reports that the browser stopped: every sighting it reported
	// before is gone.
	Reset
)

// Event is one discovery event.
type Event struct {
	Kind     Kind
	Key      Key      // Found, Lost
	Sighting Sighting // Found
}

// Browser browses for snoopy services until ctx is done or it fails. It
// calls emit (never concurrently) for every Found and Lost event. A nil
// error means ctx was cancelled.
type Browser interface {
	Browse(ctx context.Context, emit func(Event)) error
}

// Supervise runs b until ctx is done, restarting it with exponential
// backoff (minBackoff doubling to maxBackoff, reset after a run that lasted
// longer than maxBackoff). After every run it emits Reset. Errors are
// logged through logf, repeated identical errors only once.
func Supervise(ctx context.Context, b Browser, emit func(Event), logf func(string, ...any), minBackoff, maxBackoff time.Duration) {
	backoff := minBackoff
	lastErr := ""
	for {
		started := time.Now()
		err := b.Browse(ctx, emit)
		emit(Event{Kind: Reset})
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		msg := "stopped"
		if err != nil {
			msg = err.Error()
		}
		if msg != lastErr {
			logf("mDNS discovery unavailable (%s); retrying, static entries keep working", msg)
			lastErr = msg
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Limits applied to TXT records, which come from the network.
const (
	maxTXTRecords = 64
	maxTXTValue   = 255
)

// ParseTXT parses DNS-SD TXT records ("key=value", "key" alone means an
// empty value). Keys are case-insensitive and lower-cased; as RFC 6763
// section 6.4 requires, only the first occurrence of a key counts. Records
// with an empty key are ignored.
func ParseTXT(records [][]byte) map[string]string {
	m := make(map[string]string)
	for i, rec := range records {
		if i >= maxTXTRecords {
			break
		}
		k, v, _ := strings.Cut(string(rec), "=")
		k = strings.ToLower(k)
		if k == "" {
			continue
		}
		if _, dup := m[k]; dup {
			continue
		}
		if len(v) > maxTXTValue {
			v = v[:maxTXTValue]
		}
		m[k] = v
	}
	return m
}
