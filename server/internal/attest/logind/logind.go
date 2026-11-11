// Package logind queries systemd-logind (org.freedesktop.login1) on the
// system bus for the sessions snoopy-attest needs to judge.
//
// API reference:
// https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.login1.html
//
//   - Seat.ActiveSession (so): the session in the foreground on a seat, or
//     ("", "/") when none is.
//   - Manager.ListSessions() a(susso): id, uid, user name, seat id, path.
//   - Session properties: Id s, User (uo), Name s, Seat (so), TTY s,
//     Remote b, Service s, Type s ("x11", "wayland", "tty", ...), Class s
//     ("user", "greeter", "lock-screen", and on newer systemd also
//     "user-early", "user-incomplete", "user-light", "background",
//     "manager", ...), Active b, State s ("online", "active", "closing"),
//     LockedHint b.
//   - Manager signal PrepareForSleep(b): true before suspend, false after
//     resume. Session and seat property changes (LockedHint, Active,
//     ActiveSession) are announced with org.freedesktop.DBus.Properties
//     PropertiesChanged.
package logind

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	dest        = "org.freedesktop.login1"
	managerPath = dbus.ObjectPath("/org/freedesktop/login1")
	managerIfc  = "org.freedesktop.login1.Manager"
	seatIfc     = "org.freedesktop.login1.Seat"
	sessionIfc  = "org.freedesktop.login1.Session"
	propsIfc    = "org.freedesktop.DBus.Properties"
	callTimeout = 5 * time.Second
)

// Session is the subset of a logind session's properties that matters here.
type Session struct {
	ID         string `json:"id"`
	UID        uint32 `json:"uid"`
	Name       string `json:"name"`
	Seat       string `json:"seat,omitempty"`
	TTY        string `json:"tty,omitempty"`
	Service    string `json:"service,omitempty"`
	Type       string `json:"type"`
	Class      string `json:"class"`
	State      string `json:"state"`
	Remote     bool   `json:"remote"`
	Active     bool   `json:"active"`
	LockedHint bool   `json:"locked_hint"`
}

// Event is a logind change worth an immediate re-evaluation.
type Event int

// Events delivered by Client.Watch.
const (
	EventChanged Event = iota // a session/seat property changed or a session came or went
	EventResume               // PrepareForSleep(false): the machine resumed from sleep
)

// Client talks to logind over a system bus connection.
type Client struct {
	conn *dbus.Conn
}

// Connect opens a private system bus connection.
func Connect() (*Client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Close closes the bus connection.
func (c *Client) Close() error { return c.conn.Close() }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

// ActiveSession returns the active session on seat, or nil when the seat has
// no active session.
func (c *Client) ActiveSession(seat string) (*Session, error) {
	cx, cancel := ctx()
	defer cancel()
	var seatPath dbus.ObjectPath
	if err := c.conn.Object(dest, managerPath).CallWithContext(cx, managerIfc+".GetSeat", 0, seat).Store(&seatPath); err != nil {
		return nil, fmt.Errorf("GetSeat(%s): %w", seat, err)
	}
	v, err := c.conn.Object(dest, seatPath).GetProperty(seatIfc + ".ActiveSession")
	if err != nil {
		return nil, fmt.Errorf("%s ActiveSession: %w", seat, err)
	}
	var active struct {
		ID   string
		Path dbus.ObjectPath
	}
	if err := dbus.Store([]any{v.Value()}, &active); err != nil {
		return nil, fmt.Errorf("%s ActiveSession: %w", seat, err)
	}
	if active.ID == "" || active.Path == "/" {
		return nil, nil
	}
	s, err := c.session(active.Path)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// UserSessions returns all sessions of uid.
func (c *Client) UserSessions(uid uint32) ([]Session, error) {
	cx, cancel := ctx()
	defer cancel()
	var list []struct {
		ID   string
		UID  uint32
		User string
		Seat string
		Path dbus.ObjectPath
	}
	if err := c.conn.Object(dest, managerPath).CallWithContext(cx, managerIfc+".ListSessions", 0).Store(&list); err != nil {
		return nil, fmt.Errorf("ListSessions: %w", err)
	}
	var out []Session
	for _, e := range list {
		if e.UID != uid {
			continue
		}
		s, err := c.session(e.Path)
		if err != nil {
			var dbusErr dbus.Error
			if errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.DBus.Error.UnknownObject" {
				continue // session ended between the list and the lookup
			}
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *Client) session(path dbus.ObjectPath) (Session, error) {
	cx, cancel := ctx()
	defer cancel()
	var props map[string]dbus.Variant
	if err := c.conn.Object(dest, path).CallWithContext(cx, propsIfc+".GetAll", 0, sessionIfc).Store(&props); err != nil {
		return Session{}, fmt.Errorf("session %s: %w", path, err)
	}
	return sessionFromProps(props)
}

func sessionFromProps(p map[string]dbus.Variant) (Session, error) {
	var s Session
	str := func(k string) string {
		v, _ := p[k].Value().(string)
		return v
	}
	boolean := func(k string) bool {
		v, _ := p[k].Value().(bool)
		return v
	}
	s.ID = str("Id")
	s.Name = str("Name")
	s.TTY = str("TTY")
	s.Service = str("Service")
	s.Type = str("Type")
	s.Class = str("Class")
	s.State = str("State")
	s.Remote = boolean("Remote")
	s.Active = boolean("Active")
	s.LockedHint = boolean("LockedHint")
	if v, ok := p["User"]; ok {
		var u struct {
			UID  uint32
			Path dbus.ObjectPath
		}
		if err := dbus.Store([]any{v.Value()}, &u); err != nil {
			return s, fmt.Errorf("session %s User: %w", s.ID, err)
		}
		s.UID = u.UID
	} else {
		return s, fmt.Errorf("session %s: no User property", s.ID)
	}
	if v, ok := p["Seat"]; ok {
		var seat struct {
			ID   string
			Path dbus.ObjectPath
		}
		if err := dbus.Store([]any{v.Value()}, &seat); err == nil {
			s.Seat = seat.ID
		}
	}
	return s, nil
}

// Watch subscribes to logind signals and sends an Event for each relevant
// one until ctx is done. The returned channel is closed when ctx ends or the
// bus connection is lost; a closed channel before ctx is done means the
// caller should treat logind as unavailable.
func (c *Client) Watch(ctx context.Context) (<-chan Event, error) {
	rules := [][]dbus.MatchOption{
		{dbus.WithMatchSender(dest), dbus.WithMatchObjectPath(managerPath), dbus.WithMatchInterface(managerIfc), dbus.WithMatchMember("PrepareForSleep")},
		{dbus.WithMatchSender(dest), dbus.WithMatchObjectPath(managerPath), dbus.WithMatchInterface(managerIfc), dbus.WithMatchMember("SessionNew")},
		{dbus.WithMatchSender(dest), dbus.WithMatchObjectPath(managerPath), dbus.WithMatchInterface(managerIfc), dbus.WithMatchMember("SessionRemoved")},
		{dbus.WithMatchSender(dest), dbus.WithMatchPathNamespace(managerPath), dbus.WithMatchInterface(propsIfc), dbus.WithMatchMember("PropertiesChanged")},
		{dbus.WithMatchSender(dest), dbus.WithMatchPathNamespace(managerPath), dbus.WithMatchInterface(sessionIfc), dbus.WithMatchMember("Lock")},
		{dbus.WithMatchSender(dest), dbus.WithMatchPathNamespace(managerPath), dbus.WithMatchInterface(sessionIfc), dbus.WithMatchMember("Unlock")},
	}
	for _, r := range rules {
		if err := c.conn.AddMatchSignal(r...); err != nil {
			return nil, fmt.Errorf("add match: %w", err)
		}
	}
	sigs := make(chan *dbus.Signal, 32)
	c.conn.Signal(sigs)
	out := make(chan Event, 8)
	go func() {
		defer close(out)
		defer c.conn.RemoveSignal(sigs)
		for {
			select {
			case <-ctx.Done():
				return
			case s, ok := <-sigs:
				if !ok {
					return
				}
				ev, relevant := classify(s)
				if !relevant {
					continue
				}
				select {
				case out <- ev:
				default: // a pending event already forces a re-evaluation
				}
			}
		}
	}()
	return out, nil
}

func classify(s *dbus.Signal) (Event, bool) {
	switch s.Name {
	case managerIfc + ".PrepareForSleep":
		if len(s.Body) == 1 {
			if start, ok := s.Body[0].(bool); ok && !start {
				return EventResume, true
			}
		}
		return 0, false
	case propsIfc + ".PropertiesChanged":
		if len(s.Body) > 0 {
			if ifc, ok := s.Body[0].(string); ok && ifc != sessionIfc && ifc != seatIfc {
				return 0, false
			}
		}
		return EventChanged, true
	case managerIfc + ".SessionNew", managerIfc + ".SessionRemoved", sessionIfc + ".Lock", sessionIfc + ".Unlock":
		return EventChanged, true
	}
	return 0, false
}
