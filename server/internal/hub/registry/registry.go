// Package registry keeps the hub's list of snoopy services: entries found
// over mDNS (fed by discovery events) and static entries from
// SNOOPY_HUB_STATIC, their health (Prober) and change notifications.
// Everything the registry holds about a service came from the network or
// the configuration; callers must treat names, hosts and TXT values as
// untrusted text.
package registry

import (
	"maps"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"snoopy/internal/hub/discovery"
)

// Source says where an entry came from.
type Source string

// Sources.
const (
	SourceMDNS   Source = "mdns"
	SourceStatic Source = "static"
)

// Defaults for Registry.
const (
	DefaultFailThreshold = 2
	DefaultForgetAfter   = time.Hour
)

// Service is a snapshot of one registry entry.
type Service struct {
	ID        string
	Name      string
	Host      string   // host name (mDNS: e.g. alice-pc.local; static: as configured)
	Addresses []string // IP literals, IPv4 first
	Port      int
	TXT       map[string]string
	RTSPPort  int
	Version   string
	Source    Source
	FirstSeen time.Time
	LastSeen  time.Time
	Online    bool
}

// DialAddr is the host:port the hub connects to: the preferred resolved
// address, else the configured host.
func (s Service) DialAddr() string {
	host := s.Host
	if len(s.Addresses) > 0 {
		host = s.Addresses[0]
	}
	return net.JoinHostPort(host, strconv.Itoa(s.Port))
}

// RTSPURL is the stream URL by host name (preferred: it works from the
// viewer's machine on a LAN with mDNS), or by address if the host name is
// not a valid DNS name. Empty if the service exports no stream.
func (s Service) RTSPURL() string {
	if s.RTSPPort == 0 {
		return ""
	}
	if ValidHost(s.Host) {
		return rtspURL(s.Host, s.RTSPPort)
	}
	return s.RTSPURLIP()
}

// RTSPURLIP is the stream URL by the first resolved IP address, or empty.
func (s Service) RTSPURLIP() string {
	if s.RTSPPort == 0 || len(s.Addresses) == 0 {
		return ""
	}
	return rtspURL(s.Addresses[0], s.RTSPPort)
}

func rtspURL(host string, port int) string {
	if strings.Contains(host, "%") {
		// A zoned link-local address is useless on another machine.
		return ""
	}
	return "rtsp://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}

// ChangeKind is the kind of a Change.
type ChangeKind string

// Change kinds.
const (
	Added   ChangeKind = "added"
	Removed ChangeKind = "removed"
	Online  ChangeKind = "online"
	Offline ChangeKind = "offline"
	Updated ChangeKind = "updated"
)

// Change is a registry change notification.
type Change struct {
	Kind    ChangeKind
	Service Service
}

type entry struct {
	svc       Service
	sightings map[discovery.Key]discovery.Sighting
	failures  int
	offAt     time.Time
}

// Registry is safe for concurrent use.
type Registry struct {
	// FailThreshold is the number of consecutive failed probes that mark an
	// online entry offline.
	FailThreshold int
	// ForgetAfter is how long an mDNS entry whose advertisement was
	// withdrawn stays listed as offline before Sweep removes it.
	ForgetAfter time.Duration

	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*entry
	byKey   map[discovery.Key]string
	subs    map[chan Change]struct{}
	logf    func(string, ...any)
}

// New returns an empty registry. now may be nil (time.Now); logf may be nil.
func New(now func() time.Time, logf func(string, ...any)) *Registry {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Registry{
		FailThreshold: DefaultFailThreshold,
		ForgetAfter:   DefaultForgetAfter,
		now:           now,
		entries:       map[string]*entry{},
		byKey:         map[discovery.Key]string{},
		subs:          map[chan Change]struct{}{},
		logf:          logf,
	}
}

// Subscribe returns a channel of changes. A subscriber that falls more than
// buf changes behind is dropped: its channel is closed. cancel unsubscribes
// (and closes the channel if still open).
func (r *Registry) Subscribe(buf int) (<-chan Change, func()) {
	ch := make(chan Change, buf)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// notify must be called with r.mu held.
func (r *Registry) notify(kind ChangeKind, e *entry) {
	c := Change{Kind: kind, Service: clone(e.svc)}
	for ch := range r.subs {
		select {
		case ch <- c:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func clone(s Service) Service {
	s.Addresses = slices.Clone(s.Addresses)
	s.TXT = maps.Clone(s.TXT)
	return s
}

// Get returns the entry with the given id.
func (r *Registry) Get(id string) (Service, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return Service{}, false
	}
	return clone(e.svc), true
}

// List returns every entry, sorted by name (case-insensitive), then id.
func (r *Registry) List() []Service {
	r.mu.Lock()
	out := make([]Service, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, clone(e.svc))
	}
	r.mu.Unlock()
	SortServices(out)
	return out
}

// SortServices sorts by name (case-insensitive), then id.
func SortServices(list []Service) {
	sort.Slice(list, func(i, j int) bool {
		a, b := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name)
		if a != b {
			return a < b
		}
		return list[i].ID < list[j].ID
	})
}

// ProbeTargets returns the entries worth probing: static entries and mDNS
// entries that are currently advertised.
func (r *Registry) ProbeTargets() []Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Service
	for _, e := range r.entries {
		if e.svc.Source == SourceStatic || len(e.sightings) > 0 {
			out = append(out, clone(e.svc))
		}
	}
	return out
}

// AddStatic adds static entries (offline until a probe succeeds).
func (r *Registry) AddStatic(list []StaticEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, s := range list {
		id := ID(s.Name, s.Host)
		if _, dup := r.entries[id]; dup {
			continue
		}
		var addrs []string
		if ip, err := netip.ParseAddr(s.Host); err == nil {
			addrs = []string{ip.String()}
		}
		e := &entry{svc: Service{
			ID: id, Name: s.Name, Host: s.Host, Addresses: addrs, Port: s.Port,
			TXT: map[string]string{}, RTSPPort: s.RTSPPort, Source: SourceStatic,
			FirstSeen: now,
		}, offAt: now}
		r.entries[id] = e
		r.notify(Added, e)
	}
}

// Apply applies a discovery event.
func (r *Registry) Apply(ev discovery.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case discovery.Found:
		r.found(ev.Sighting)
	case discovery.Lost:
		r.lost(ev.Key)
	case discovery.Reset:
		for k := range r.byKey {
			r.lost(k)
		}
	}
}

func (r *Registry) found(s discovery.Sighting) {
	id := ID(s.Name, s.Host)
	if old, ok := r.byKey[s.Key]; ok && old != id {
		r.lost(s.Key)
	}
	now := r.now()
	e, ok := r.entries[id]
	if ok && e.svc.Source != SourceMDNS {
		r.logf("Ignoring mDNS service %q on %q: a static entry has the same id %s", s.Name, s.Host, id)
		return
	}
	r.byKey[s.Key] = id
	if !ok {
		e = &entry{svc: Service{ID: id, Source: SourceMDNS, FirstSeen: now}, sightings: map[discovery.Key]discovery.Sighting{}}
		e.sightings[s.Key] = s
		r.entries[id] = e
		recompute(e)
		e.svc.Online, e.svc.LastSeen = true, now
		r.notify(Added, e)
		return
	}
	before := clone(e.svc)
	e.sightings[s.Key] = s
	recompute(e)
	e.svc.LastSeen = now
	if !e.svc.Online {
		e.svc.Online, e.failures = true, 0
		r.notify(Online, e)
		return
	}
	if !sameDetails(before, e.svc) {
		r.notify(Updated, e)
	}
}

func (r *Registry) lost(k discovery.Key) {
	id, ok := r.byKey[k]
	if !ok {
		return
	}
	delete(r.byKey, k)
	e := r.entries[id]
	if e == nil {
		return
	}
	before := clone(e.svc)
	delete(e.sightings, k)
	if len(e.sightings) > 0 {
		recompute(e)
		if !sameDetails(before, e.svc) {
			r.notify(Updated, e)
		}
		return
	}
	if e.svc.Online {
		e.svc.Online = false
		e.offAt = r.now()
		r.notify(Offline, e)
	}
}

// ReportProbe records a health probe result.
func (r *Registry) ReportProbe(id string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.entries[id]
	if !found {
		return
	}
	if e.svc.Source == SourceMDNS && len(e.sightings) == 0 {
		return // withdrawn: only a new advertisement brings it back
	}
	if ok {
		e.failures = 0
		e.svc.LastSeen = r.now()
		if !e.svc.Online {
			e.svc.Online = true
			r.notify(Online, e)
		}
		return
	}
	e.failures++
	threshold := r.FailThreshold
	if threshold < 1 {
		threshold = 1
	}
	if e.svc.Online && e.failures >= threshold {
		e.svc.Online = false
		e.offAt = r.now()
		r.notify(Offline, e)
	}
}

// Sweep removes mDNS entries that have been withdrawn for longer than
// ForgetAfter.
func (r *Registry) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, e := range r.entries {
		if e.svc.Source == SourceMDNS && len(e.sightings) == 0 && !e.svc.Online && now.Sub(e.offAt) > r.ForgetAfter {
			delete(r.entries, id)
			r.notify(Removed, e)
		}
	}
}

// preferred orders sightings: IPv4 first, then by interface, protocol and
// address, so the choice is deterministic.
func preferred(list []discovery.Sighting) {
	sort.Slice(list, func(i, j int) bool {
		a4, b4 := isIPv4(list[i].Address), isIPv4(list[j].Address)
		if a4 != b4 {
			return a4
		}
		if list[i].Key.Interface != list[j].Key.Interface {
			return list[i].Key.Interface < list[j].Key.Interface
		}
		if list[i].Key.Protocol != list[j].Key.Protocol {
			return list[i].Key.Protocol < list[j].Key.Protocol
		}
		return list[i].Address < list[j].Address
	})
}

func isIPv4(s string) bool {
	ip, err := netip.ParseAddr(s)
	return err == nil && ip.Unmap().Is4()
}

// recompute derives the service fields from its sightings.
func recompute(e *entry) {
	list := slices.Collect(maps.Values(e.sightings))
	if len(list) == 0 {
		return
	}
	preferred(list)
	best := list[0]
	var addrs []string
	for _, s := range list {
		if !slices.Contains(addrs, s.Address) {
			addrs = append(addrs, s.Address)
		}
	}
	e.svc.Name = best.Name
	e.svc.Host = best.Host
	e.svc.Addresses = addrs
	e.svc.Port = best.Port
	e.svc.TXT = maps.Clone(best.TXT)
	e.svc.Version = best.TXT["ver"]
	e.svc.RTSPPort = 0
	if p, err := strconv.Atoi(best.TXT["rtsp"]); err == nil && p > 0 && p <= 65535 {
		e.svc.RTSPPort = p
	}
}

func sameDetails(a, b Service) bool {
	return a.Name == b.Name && a.Host == b.Host && a.Port == b.Port &&
		a.RTSPPort == b.RTSPPort && a.Version == b.Version &&
		slices.Equal(a.Addresses, b.Addresses) && maps.Equal(a.TXT, b.TXT)
}
