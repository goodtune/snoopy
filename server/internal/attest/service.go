package attest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"snoopy/internal/attest/heartbeat"
	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/pki"
	"snoopy/internal/attest/procfs"
)

// StateFileName is written inside RuntimeDir.
const StateFileName = "state.json"

// Service evaluates the checks periodically and on logind events, publishes
// the result to state.json and hands it to the heartbeat sender.
type Service struct {
	Cfg      *Config
	Checker  *Checker
	Eval     Evaluator
	Frame    *FrameChecker     // nil when FRAME_CHECK is off
	Sender   *heartbeat.Sender // nil when URL is empty
	Now      func() time.Time
	Logf     func(format string, args ...any)
	Version  string
	DeviceID string

	mu     sync.Mutex
	last   *Evaluation
	lastHB *heartbeat.Result
}

// StateFile is the JSON document written to <RUNTIME_DIR>/state.json.
type StateFile struct {
	Evaluation
	DeviceID        string            `json:"device_id"`
	AttestorVersion string            `json:"attestor_version"`
	URL             string            `json:"url,omitempty"`
	Heartbeat       *heartbeat.Result `json:"heartbeat,omitempty"`
}

// Evaluate runs every check once and returns the resulting evaluation.
func (s *Service) Evaluate(ctx context.Context) *Evaluation {
	now := s.Now()
	raw := s.Checker.run()
	if s.Frame == nil {
		raw.Frame = CheckResult{Name: frameCheckLabel, Status: StatusSkip, Detail: "disabled (" + EnvPrefix + "FRAME_CHECK=false)"}
	} else {
		recordingExpected := !raw.Session.Err && !raw.Session.NoSession && !raw.Session.Locked &&
			raw.Recorder.Status == StatusPass && raw.Screencast.Segment != ""
		if recordingExpected && s.Frame.Due(now) {
			s.Frame.Run(ctx, now, raw.Screencast.Segment)
		}
		raw.Frame, raw.FrameBad, raw.FrameWH = s.Frame.Result()
		if !recordingExpected {
			raw.FrameBad = false
		}
	}
	st, reasons, grace := s.Eval.evaluate(now, &raw)
	return &Evaluation{
		Time:     now.UTC(),
		State:    st,
		Reasons:  reasons,
		User:     s.Cfg.User,
		UID:      s.Cfg.UID,
		Session:  raw.Session.Session,
		Checks:   []CheckResult{raw.Session.CheckResult, raw.Recorder.CheckResult, raw.Screencast.CheckResult, raw.Frame},
		Evidence: raw.evidence(),
		GraceEnd: grace,
	}
}

func (s *Service) report(ev *Evaluation) heartbeat.Report {
	return heartbeat.Report{State: string(ev.State), Reasons: ev.Reasons, User: ev.User, UID: ev.UID, Evidence: ev.Evidence}
}

// step evaluates, logs state changes, notifies the sender and writes the
// state file.
func (s *Service) step(ctx context.Context) {
	ev := s.Evaluate(ctx)
	s.mu.Lock()
	prev := s.last
	s.last = ev
	s.mu.Unlock()
	if prev == nil || prev.State != ev.State || !slices.Equal(prev.Reasons, ev.Reasons) {
		from := "start"
		if prev != nil {
			from = string(prev.State)
		}
		s.Logf("state %s -> %s %v", from, ev.State, ev.Reasons)
	}
	if s.Sender != nil {
		s.Sender.Update(s.report(ev))
	}
	s.writeState()
}

func (s *Service) recordHeartbeat(r heartbeat.Result) {
	s.mu.Lock()
	s.lastHB = &r
	s.mu.Unlock()
	s.writeState()
}

func (s *Service) writeState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return
	}
	doc := StateFile{Evaluation: *s.last, DeviceID: s.DeviceID, AttestorVersion: s.Version, URL: s.Cfg.URL, Heartbeat: s.lastHB}
	if err := writeJSONAtomic(filepath.Join(s.Cfg.RuntimeDir, StateFileName), doc); err != nil {
		s.Logf("write state file: %v", err)
	}
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Loop runs until ctx is done (returning nil) or the logind event channel
// closes (returning an error, so systemd restarts the service).
func (s *Service) Loop(ctx context.Context, events <-chan logind.Event) error {
	if s.Sender != nil {
		done := make(chan struct{})
		sctx, cancel := context.WithCancel(ctx)
		go func() { s.Sender.Run(sctx); close(done) }()
		defer func() { cancel(); <-done }()
	}
	s.step(ctx)
	tick := time.NewTicker(s.Cfg.CheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case ev, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("lost connection to systemd-logind")
			}
			if ev == logind.EventResume {
				s.Logf("resumed from sleep; re-evaluating")
				s.Eval.StartGrace(s.Now())
			}
		}
		s.step(ctx)
	}
}

// Deps are the production dependencies Run wires together; tests replace
// them.
type Deps struct {
	Logind interface {
		SessionSource
		Watch(ctx context.Context) (<-chan logind.Event, error)
	}
	Proc       procfs.FS
	TrustedUID uint32
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// NewChecker builds a Checker from cfg and deps.
func NewChecker(cfg *Config, d Deps) *Checker {
	return &Checker{Cfg: cfg, Proc: d.Proc, Logind: d.Logind, Now: d.Now, TrustedUID: d.TrustedUID}
}

// NewService builds a Service. When cfg.URL is set it loads the device key
// and prepares the heartbeat sender.
func NewService(cfg *Config, version string, d Deps) (*Service, error) {
	s := &Service{
		Cfg:      cfg,
		Checker:  NewChecker(cfg, d),
		Eval:     Evaluator{Grace: cfg.StartupGrace},
		Now:      d.Now,
		Logf:     d.Logf,
		Version:  version,
		DeviceID: cfg.DeviceID,
	}
	if cfg.FrameCheck {
		s.Frame = &FrameChecker{FFmpeg: cfg.FFmpeg, User: cfg.FrameUser, Interval: cfg.FrameInterval, Logf: d.Logf}
	}
	if cfg.URL == "" {
		d.Logf("%sURL is empty: evaluating and writing %s only, no heartbeats", EnvPrefix, filepath.Join(cfg.RuntimeDir, StateFileName))
		return s, nil
	}
	kp, err := pki.LoadKeyPair(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("load device key (run `snoopy-attest keygen` first): %w", err)
	}
	tlsCfg, err := heartbeat.TLSConfig(kp, cfg.CAFile, cfg.ServerPin)
	if err != nil {
		return nil, err
	}
	bootID, err := d.Proc.BootID()
	if err != nil {
		return nil, fmt.Errorf("boot id: %w", err)
	}
	up, err := d.Proc.Uptime()
	if err != nil {
		return nil, fmt.Errorf("uptime: %w", err)
	}
	s.Sender = heartbeat.New(heartbeat.Config{
		URL:             cfg.URL,
		Client:          heartbeat.NewHTTPClient(tlsCfg, 10*time.Second),
		DeviceID:        cfg.DeviceID,
		BootID:          bootID,
		AttestorVersion: version,
		FirstSeq:        heartbeat.SeqBase(up),
		Interval:        cfg.HeartbeatInterval,
		Now:             d.Now,
		Logf:            d.Logf,
		OnResult:        s.recordHeartbeat,
	})
	return s, nil
}

// Run is `snoopy-attest run`.
func Run(ctx context.Context, cfg *Config, version string, d Deps) error {
	s, err := NewService(cfg, version, d)
	if err != nil {
		return err
	}
	events, err := d.Logind.Watch(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to logind signals: %w", err)
	}
	d.Logf("snoopy-attest %s watching %s (uid %d) on %s, device %q", version, cfg.User, cfg.UID, cfg.Seat, cfg.DeviceID)
	return s.Loop(ctx, events)
}
