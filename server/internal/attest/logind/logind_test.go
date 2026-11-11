package logind

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSessionFromProps(t *testing.T) {
	props := map[string]dbus.Variant{
		"Id":         dbus.MakeVariant("3"),
		"Name":       dbus.MakeVariant("kid1"),
		"User":       dbus.MakeVariant([]any{uint32(1000), dbus.ObjectPath("/org/freedesktop/login1/user/_1000")}),
		"Seat":       dbus.MakeVariant([]any{"seat0", dbus.ObjectPath("/org/freedesktop/login1/seat/seat0")}),
		"Type":       dbus.MakeVariant("wayland"),
		"Class":      dbus.MakeVariant("user"),
		"State":      dbus.MakeVariant("active"),
		"Remote":     dbus.MakeVariant(false),
		"Active":     dbus.MakeVariant(true),
		"LockedHint": dbus.MakeVariant(true),
		"TTY":        dbus.MakeVariant("tty2"),
	}
	s, err := sessionFromProps(props)
	if err != nil {
		t.Fatal(err)
	}
	want := Session{ID: "3", UID: 1000, Name: "kid1", Seat: "seat0", TTY: "tty2", Type: "wayland", Class: "user", State: "active", Active: true, LockedHint: true}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
	delete(props, "User")
	if _, err := sessionFromProps(props); err == nil {
		t.Fatal("missing User accepted")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		sig      dbus.Signal
		want     Event
		relevant bool
	}{
		{dbus.Signal{Name: managerIfc + ".PrepareForSleep", Body: []any{false}}, EventResume, true},
		{dbus.Signal{Name: managerIfc + ".PrepareForSleep", Body: []any{true}}, 0, false},
		{dbus.Signal{Name: propsIfc + ".PropertiesChanged", Body: []any{sessionIfc}}, EventChanged, true},
		{dbus.Signal{Name: propsIfc + ".PropertiesChanged", Body: []any{seatIfc}}, EventChanged, true},
		{dbus.Signal{Name: propsIfc + ".PropertiesChanged", Body: []any{"org.freedesktop.login1.User"}}, 0, false},
		{dbus.Signal{Name: sessionIfc + ".Unlock"}, EventChanged, true},
		{dbus.Signal{Name: managerIfc + ".SessionNew"}, EventChanged, true},
		{dbus.Signal{Name: "org.example.Other"}, 0, false},
	}
	for _, tt := range tests {
		ev, ok := classify(&tt.sig)
		if ok != tt.relevant || (ok && ev != tt.want) {
			t.Errorf("%s %v: got %v,%v want %v,%v", tt.sig.Name, tt.sig.Body, ev, ok, tt.want, tt.relevant)
		}
	}
}
