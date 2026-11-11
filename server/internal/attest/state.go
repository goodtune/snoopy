package attest

import (
	"slices"
	"time"

	"snoopy/internal/attest/heartbeat"
	"snoopy/internal/attest/logind"
)

// State is the attested state reported to KProxy.
type State string

// States, from most to least permissive. docs/attestation.md defines how
// KProxy treats each.
const (
	StateRecording State = "recording"  // genuine recorder running and recording the active, unlocked session
	StateLocked    State = "locked"     // supervised session locked; recorder integrity still verified
	StateNoSession State = "no_session" // nobody, the greeter or another user on the seat
	StateDegraded  State = "degraded"   // recorder genuine but recording evidence missing/stale, or extra session, or grace
	StateAbsent    State = "absent"     // recorder not running or integrity check failed
)

// Evaluation is one complete assessment.
type Evaluation struct {
	Time     time.Time          `json:"time"`
	State    State              `json:"state"`
	Reasons  []string           `json:"reasons"`
	User     string             `json:"user"`
	UID      uint32             `json:"uid"`
	Session  *logind.Session    `json:"session,omitempty"`
	Checks   []CheckResult      `json:"checks"`
	Evidence heartbeat.Evidence `json:"evidence"`
	GraceEnd *time.Time         `json:"grace_end,omitempty"`
}

// decide maps check results to a state and the reasons that determined it.
// inGrace is true within StartupGrace after the service started, the session
// was unlocked or became active, or the machine resumed.
func decide(raw *rawResults, inGrace bool) (State, []string) {
	s := &raw.Session
	if s.Err {
		return StateAbsent, []string{ReasonLogindUnavailable}
	}
	if s.NoSession {
		return StateNoSession, slices.Clone(s.Reasons)
	}
	if raw.ProcErr != nil {
		return StateAbsent, []string{ReasonProcUnavailable}
	}
	rec := &raw.Recorder
	if rec.Status == StatusFail {
		// A recorder that has not (re)started yet right after login, unlock
		// or resume is expected; anything that looks like tampering is not.
		if inGrace && !s.Locked && slices.Equal(rec.Reasons, []string{ReasonRecorderNotRunning}) {
			return StateDegraded, []string{ReasonRecorderNotRunning, ReasonStartupGrace}
		}
		return StateAbsent, slices.Clone(rec.Reasons)
	}
	if s.Locked {
		return StateLocked, []string{ReasonLocked}
	}
	var reasons []string
	if s.Extra {
		reasons = append(reasons, ReasonExtraSession)
	}
	if raw.Screencast.Status == StatusFail {
		reasons = append(reasons, raw.Screencast.Reasons...)
	}
	if raw.FrameBad {
		reasons = append(reasons, ReasonFrameCheckFailed)
	}
	if len(reasons) > 0 {
		if inGrace {
			reasons = append(reasons, ReasonStartupGrace)
		}
		return StateDegraded, reasons
	}
	return StateRecording, []string{}
}

// Evaluator turns successive check results into states, tracking the grace
// period that follows service start, unlock, session activation and resume.
type Evaluator struct {
	Grace      time.Duration
	prevActive bool // the supervised session was active and unlocked last time
	started    bool
	graceUntil time.Time
}

// StartGrace opens a grace period from now (used on resume from sleep).
func (e *Evaluator) StartGrace(now time.Time) {
	e.graceUntil = now.Add(e.Grace)
}

func (e *Evaluator) evaluate(now time.Time, raw *rawResults) (State, []string, *time.Time) {
	if !e.started {
		e.started = true
		e.StartGrace(now)
	}
	// Grace follows the session, not the reported state: a recorder that
	// restarted while the screen was locked (reported absent) still gets
	// time to catch up after unlock.
	active := !raw.Session.Err && !raw.Session.NoSession && !raw.Session.Locked
	if active && !e.prevActive {
		e.StartGrace(now)
	}
	e.prevActive = active
	inGrace := now.Before(e.graceUntil)
	st, reasons := decide(raw, inGrace)
	if inGrace {
		g := e.graceUntil
		return st, reasons, &g
	}
	return st, reasons, nil
}
