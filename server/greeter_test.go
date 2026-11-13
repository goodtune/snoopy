package main

import (
	"errors"
	"testing"

	"snoopy/internal/attest/logind"
)

type fakeSessionSource struct {
	sessions []logind.Session
	err      error
	gotUID   uint32
}

func (f *fakeSessionSource) UserSessions(uid uint32) ([]logind.Session, error) {
	f.gotUID = uid
	return f.sessions, f.err
}

func sess(id, class string) logind.Session {
	return logind.Session{ID: id, UID: 60578, Class: class}
}

func TestIsGreeterSession(t *testing.T) {
	tests := []struct {
		name     string
		sessions []logind.Session
		err      error
		want     bool
	}{
		{"greeter only", []logind.Session{sess("c1", "greeter")}, nil, true},
		{"greeter and its user manager", []logind.Session{sess("c1", "greeter"), sess("1", "manager")}, nil, true},
		{"two greeters", []logind.Session{sess("c1", "greeter"), sess("c2", "greeter")}, nil, true},
		{"user session", []logind.Session{sess("2", "user"), sess("1", "manager")}, nil, false},
		{"greeter and user", []logind.Session{sess("c1", "greeter"), sess("2", "user")}, nil, false},
		{"user-early", []logind.Session{sess("2", "user-early")}, nil, false},
		{"user-light", []logind.Session{sess("2", "user-light")}, nil, false},
		{"user-incomplete", []logind.Session{sess("2", "user-incomplete")}, nil, false},
		{"lock-screen is not a greeter", []logind.Session{sess("3", "lock-screen")}, nil, false},
		{"greeter plus background", []logind.Session{sess("c1", "greeter"), sess("4", "background"), sess("5", "background-light")}, nil, true},
		{"manager only", []logind.Session{sess("1", "manager"), sess("6", "manager-early")}, nil, false},
		{"background only", []logind.Session{sess("4", "background")}, nil, false},
		{"unknown class", []logind.Session{sess("7", "")}, nil, false},
		{"no sessions", nil, nil, false},
		{"logind error", []logind.Session{sess("c1", "greeter")}, errors.New("no system bus"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSessionSource{sessions: tt.sessions, err: tt.err}
			got, err := isGreeterSession(src, 60578)
			if got != tt.want {
				t.Errorf("isGreeterSession = %v, want %v", got, tt.want)
			}
			if (err != nil) != (tt.err != nil) {
				t.Errorf("err = %v, want %v", err, tt.err)
			}
			if src.gotUID != 60578 {
				t.Errorf("queried uid %d, want 60578", src.gotUID)
			}
		})
	}
}
