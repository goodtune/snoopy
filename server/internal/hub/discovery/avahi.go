package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Avahi D-Bus API (avahi-daemon/org.freedesktop.Avahi.Server.xml and
// org.freedesktop.Avahi.ServiceBrowser.xml in the Avahi sources; constants
// from avahi-common/defs.h).
const (
	avahiDest          = "org.freedesktop.Avahi"
	avahiServerIface   = "org.freedesktop.Avahi.Server"
	avahiBrowserIface  = "org.freedesktop.Avahi.ServiceBrowser"
	avahiIfUnspec      = int32(-1) // AVAHI_IF_UNSPEC
	avahiProtoUnspec   = int32(-1) // AVAHI_PROTO_UNSPEC
	dbusIface          = "org.freedesktop.DBus"
	nameOwnerChanged   = dbusIface + ".NameOwnerChanged"
	defaultResolveWait = 10 * time.Second
)

// resolved is the reply of org.freedesktop.Avahi.Server.ResolveService.
type resolved struct {
	Interface int32
	Protocol  int32
	Name      string
	Type      string
	Domain    string
	Host      string
	AProtocol int32
	Address   string
	Port      uint16
	TXT       [][]byte
	Flags     uint32
}

// avahiBus is the part of the system bus the browser uses, so the event
// loop can be tested with a fake.
type avahiBus interface {
	// Signals delivers ServiceBrowser signals from Avahi and
	// NameOwnerChanged for org.freedesktop.Avahi. The subscription exists
	// before ServiceBrowserNew is called, so no early ItemNew is lost. The
	// channel is closed when the connection is lost.
	Signals() <-chan *dbus.Signal
	ServiceBrowserNew(ctx context.Context, iface, proto int32, typ, domain string, flags uint32) (dbus.ObjectPath, error)
	ResolveService(ctx context.Context, iface, proto int32, name, typ, domain string, aproto int32, flags uint32) (resolved, error)
	Close() error
}

// Avahi browses for snoopy services through avahi-daemon on the system bus.
// It needs no privileges: Avahi's default D-Bus policy lets any user call
// its methods except SetHostName.
type Avahi struct {
	// Type is the service type; empty means ServiceType.
	Type string
	// Logf logs resolve failures; nil discards them.
	Logf func(string, ...any)

	dial           func() (avahiBus, error)
	ifName         func(index int) string
	resolveTimeout time.Duration
}

// NewAvahi returns a browser using the system bus.
func NewAvahi(logf func(string, ...any)) *Avahi {
	return &Avahi{Logf: logf, dial: dialSystemBus, ifName: interfaceName, resolveTimeout: defaultResolveWait}
}

func interfaceName(index int) string {
	if ifc, err := net.InterfaceByIndex(index); err == nil {
		return ifc.Name
	}
	return ""
}

type resolveResult struct {
	key Key
	gen uint64
	r   resolved
	err error
}

// Browse implements Browser.
func (a *Avahi) Browse(ctx context.Context, emit func(Event)) error {
	typ := a.Type
	if typ == "" {
		typ = ServiceType
	}
	bus, err := a.dial()
	if err != nil {
		return err
	}
	defer func() { _ = bus.Close() }()
	sigs := bus.Signals()

	path, err := bus.ServiceBrowserNew(ctx, avahiIfUnspec, avahiProtoUnspec, typ, "", 0)
	if err != nil {
		return fmt.Errorf("ServiceBrowserNew: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan resolveResult)
	live := map[Key]uint64{}   // announced items and their resolve generation
	reported := map[Key]bool{} // items emitted as Found
	var gen uint64
	timeout := a.resolveTimeout
	if timeout <= 0 {
		timeout = defaultResolveWait
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-results:
			if live[res.key] != res.gen {
				continue // removed or re-announced meanwhile
			}
			if res.err != nil {
				a.logf("Avahi: cannot resolve %q: %v", res.key.Name, res.err)
				continue
			}
			s, ok := a.sighting(res.key, res.r)
			if !ok {
				a.logf("Avahi: ignoring %q: unusable address or port", res.key.Name)
				continue
			}
			reported[res.key] = true
			emit(Event{Kind: Found, Key: res.key, Sighting: s})
		case sig, ok := <-sigs:
			if !ok {
				return errors.New("system bus connection lost")
			}
			if sig.Name == nameOwnerChanged {
				if len(sig.Body) >= 1 {
					if n, _ := sig.Body[0].(string); n == avahiDest {
						return errors.New("avahi-daemon restarted or stopped")
					}
				}
				continue
			}
			if sig.Path != path {
				continue
			}
			switch sig.Name {
			case avahiBrowserIface + ".ItemNew", avahiBrowserIface + ".ItemRemove":
				key, ok := itemKey(sig.Body)
				if !ok {
					continue
				}
				if sig.Name == avahiBrowserIface+".ItemRemove" {
					delete(live, key)
					if reported[key] {
						delete(reported, key)
						emit(Event{Kind: Lost, Key: key})
					}
					continue
				}
				gen++
				live[key] = gen
				go func(key Key, gen uint64) {
					rctx, rcancel := context.WithTimeout(ctx, timeout)
					defer rcancel()
					// aprotocol is the protocol the item was seen on,
					// so an IPv4 sighting yields an IPv4 address.
					r, err := bus.ResolveService(rctx, key.Interface, key.Protocol, key.Name, typ, key.Domain, key.Protocol, 0)
					select {
					case results <- resolveResult{key, gen, r, err}:
					case <-ctx.Done():
					}
				}(key, gen)
			case avahiBrowserIface + ".Failure":
				msg := "unknown error"
				if len(sig.Body) >= 1 {
					if s, ok := sig.Body[0].(string); ok {
						msg = s
					}
				}
				return fmt.Errorf("service browser failed: %s", msg)
			}
		}
	}
}

func (a *Avahi) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// itemKey parses the body of ItemNew/ItemRemove (i, i, s, s, s, u).
func itemKey(body []any) (Key, bool) {
	if len(body) < 5 {
		return Key{}, false
	}
	ifc, ok1 := body[0].(int32)
	proto, ok2 := body[1].(int32)
	name, ok3 := body[2].(string)
	domain, ok4 := body[4].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Key{}, false
	}
	return Key{Interface: ifc, Protocol: proto, Name: name, Domain: domain}, true
}

// sighting validates a ResolveService reply.
func (a *Avahi) sighting(key Key, r resolved) (Sighting, bool) {
	ip := net.ParseIP(r.Address)
	if ip == nil || r.Port == 0 {
		return Sighting{}, false
	}
	addr := ip.String()
	if ip.To4() == nil && ip.IsLinkLocalUnicast() && a.ifName != nil {
		// A link-local IPv6 address is only usable with its zone.
		zone := a.ifName(int(r.Interface))
		if zone == "" {
			return Sighting{}, false
		}
		addr += "%" + zone
	}
	return Sighting{
		Key:     key,
		Name:    key.Name,
		Host:    strings.TrimSuffix(r.Host, "."),
		Address: addr,
		Port:    int(r.Port),
		TXT:     ParseTXT(r.TXT),
	}, true
}

// systemBus implements avahiBus with godbus.
type systemBus struct {
	conn *dbus.Conn
	ch   chan *dbus.Signal
}

func dialSystemBus() (avahiBus, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect to system bus: %w", err)
	}
	if err := conn.AddMatchSignal(dbus.WithMatchSender(avahiDest), dbus.WithMatchInterface(avahiBrowserIface)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("subscribe to Avahi signals: %w", err)
	}
	if err := conn.AddMatchSignal(dbus.WithMatchSender(dbusIface), dbus.WithMatchInterface(dbusIface),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, avahiDest)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("subscribe to NameOwnerChanged: %w", err)
	}
	ch := make(chan *dbus.Signal, 256)
	conn.Signal(ch)
	return &systemBus{conn: conn, ch: ch}, nil
}

func (b *systemBus) Signals() <-chan *dbus.Signal { return b.ch }

func (b *systemBus) ServiceBrowserNew(ctx context.Context, iface, proto int32, typ, domain string, flags uint32) (dbus.ObjectPath, error) {
	var path dbus.ObjectPath
	err := b.conn.Object(avahiDest, "/").CallWithContext(ctx, avahiServerIface+".ServiceBrowserNew", 0,
		iface, proto, typ, domain, flags).Store(&path)
	return path, err
}

func (b *systemBus) ResolveService(ctx context.Context, iface, proto int32, name, typ, domain string, aproto int32, flags uint32) (resolved, error) {
	var r resolved
	err := b.conn.Object(avahiDest, "/").CallWithContext(ctx, avahiServerIface+".ResolveService", 0,
		iface, proto, name, typ, domain, aproto, flags).Store(
		&r.Interface, &r.Protocol, &r.Name, &r.Type, &r.Domain, &r.Host,
		&r.AProtocol, &r.Address, &r.Port, &r.TXT, &r.Flags)
	return r, err
}

// Close drops the connection; Avahi frees the browser object with it.
func (b *systemBus) Close() error { return b.conn.Close() }
