// Package heartbeat sends snoopy-attest presence reports to KProxy over
// mutually authenticated TLS. The wire protocol is specified in
// docs/attestation.md; keep the two in sync.
package heartbeat

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"

	"snoopy/internal/attest/pki"
)

// ProtocolVersion is the "v" field of every heartbeat.
const ProtocolVersion = 1

// Evidence is informational detail for audit and diagnostics. KProxy must not
// base its decision on it; the decision is "state" from a pinned device.
type Evidence struct {
	RecorderPID   int    `json:"recorder_pid,omitempty"`
	RecorderStart string `json:"recorder_start,omitempty"`
	Segment       string `json:"segment,omitempty"`
	SegmentSize   int64  `json:"segment_size,omitempty"`
	SegmentMtime  string `json:"segment_mtime,omitempty"`
	ScreencastPID int    `json:"screencast_pid,omitempty"`
	FrameWH       []int  `json:"frame_wh,omitempty"`
}

// Report is one evaluation to be sent.
type Report struct {
	State           string
	Reasons         []string
	User            string
	UID             uint32
	RecorderVersion string
	Evidence        Evidence
}

// Payload is the JSON body of a heartbeat.
type Payload struct {
	V               int      `json:"v"`
	DeviceID        string   `json:"device_id"`
	State           string   `json:"state"`
	Reasons         []string `json:"reasons"`
	Seq             uint64   `json:"seq"`
	TS              string   `json:"ts"`
	BootID          string   `json:"boot_id"`
	User            string   `json:"user"`
	UID             uint32   `json:"uid"`
	AttestorVersion string   `json:"attestor_version"`
	RecorderVersion string   `json:"recorder_version,omitempty"`
	Evidence        Evidence `json:"evidence"`
}

// Result is the outcome of one heartbeat request.
type Result struct {
	Time   time.Time `json:"time"`
	Seq    uint64    `json:"seq"`
	State  string    `json:"state"`
	Status int       `json:"status,omitempty"` // HTTP status, 0 if no response
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

// TLSConfig builds the client TLS configuration: TLS 1.3 only, the device
// certificate as client certificate, the server verified against caFile (PEM
// bundle; empty means the system roots) and, when serverPin is set, required
// to present a certificate in its verified chain whose SPKI pin matches.
func TLSConfig(cert tls.Certificate, caFile, serverPin string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // CA bundle path chosen by the administrator
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificates", caFile)
		}
		cfg.RootCAs = pool
	}
	if serverPin != "" {
		if err := pki.ValidatePin(serverPin); err != nil {
			return nil, err
		}
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			for _, chain := range cs.VerifiedChains {
				for _, c := range chain {
					if pki.Pin(c) == serverPin {
						return nil
					}
				}
			}
			return errors.New("server certificate does not match SNOOPY_ATTEST_SERVER_PIN")
		}
	}
	return cfg, nil
}

// NewHTTPClient returns a client with a single keep-alive connection to the
// server and a per-request timeout.
func NewHTTPClient(cfg *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // presence is bound to our own address; never go through a proxy
			TLSClientConfig:     cfg,
			ForceAttemptHTTP2:   false,
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
			MaxConnsPerHost:     1,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Config configures a Sender.
type Config struct {
	URL             string
	Client          *http.Client
	DeviceID        string
	BootID          string
	AttestorVersion string
	// FirstSeq is the sequence number of the first heartbeat. Pick it so it
	// exceeds anything an earlier instance sent in the same boot (see
	// SeqBase).
	FirstSeq  uint64
	Interval  time.Duration // regular send interval
	RetryBase time.Duration // first transient-failure retry delay (default 500ms)
	RetryMax  time.Duration // cap of the retry delay (default 5s)
	Now       func() time.Time
	Logf      func(format string, args ...any)
	OnResult  func(Result)
}

// SeqBase derives a starting sequence number from the time since boot, so
// sequence numbers keep increasing across attestor restarts within one boot
// (100000 per second of uptime, far more than are ever sent).
func SeqBase(uptime time.Duration) uint64 {
	if uptime < 0 {
		return 1
	}
	return uint64(uptime/(10*time.Microsecond)) + 1
}

// Sender delivers heartbeats: one immediately for the first report and on
// every state change, then every Interval; transient failures are retried
// with jittered exponential backoff capped at RetryMax. It never has more
// than one request in flight.
type Sender struct {
	cfg    Config
	notify chan struct{}

	mu     sync.Mutex
	latest *Report
	seq    uint64
}

// New returns a Sender. Run must be called to start sending.
func New(cfg Config) *Sender {
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 500 * time.Millisecond
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.FirstSeq == 0 {
		cfg.FirstSeq = 1
	}
	return &Sender{cfg: cfg, notify: make(chan struct{}, 1), seq: cfg.FirstSeq - 1}
}

// Update records the latest evaluation. A change of state (or of the reasons)
// triggers an immediate send; otherwise it goes out with the next regular
// heartbeat.
func (s *Sender) Update(r Report) {
	s.mu.Lock()
	changed := s.latest == nil || s.latest.State != r.State || !reflect.DeepEqual(s.latest.Reasons, r.Reasons)
	cp := r
	s.latest = &cp
	s.mu.Unlock()
	if changed {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeTransient
	outcomeRejected // 401/403: unknown or revoked pin
	outcomeError    // other non-retryable responses
)

// Run sends heartbeats until ctx is done.
func (s *Sender) Run(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	var retry *time.Timer
	var retryC <-chan time.Time
	attempt := 0
	rejectedLogged := false
	stopRetry := func() {
		if retry != nil {
			retry.Stop()
		}
		retryC = nil
	}
	defer stopRetry()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-tick.C:
		case <-retryC:
			retryC = nil
		}
		s.mu.Lock()
		r := s.latest
		s.mu.Unlock()
		if r == nil {
			continue
		}
		res, oc := s.send(ctx, *r)
		if ctx.Err() != nil {
			return
		}
		if s.cfg.OnResult != nil {
			s.cfg.OnResult(res)
		}
		switch oc {
		case outcomeOK:
			if attempt > 0 || rejectedLogged {
				s.cfg.Logf("heartbeat: delivered again (seq %d)", res.Seq)
			}
			attempt = 0
			rejectedLogged = false
			stopRetry()
		case outcomeTransient:
			stopRetry()
			d := s.backoff(attempt)
			attempt++
			if attempt == 1 || attempt%10 == 0 {
				s.cfg.Logf("heartbeat: %s; retrying in %v (attempt %d)", res.Error, d.Round(time.Millisecond), attempt)
			}
			retry = time.NewTimer(d)
			retryC = retry.C
		case outcomeRejected:
			stopRetry()
			attempt = 0
			if !rejectedLogged {
				s.cfg.Logf("heartbeat: WARNING: KProxy rejected this device (HTTP %d); is the pin registered? Retrying every %v", res.Status, s.cfg.Interval)
				rejectedLogged = true
			}
		case outcomeError:
			stopRetry()
			attempt = 0
			s.cfg.Logf("heartbeat: %s; next attempt in %v", res.Error, s.cfg.Interval)
		}
	}
}

func (s *Sender) backoff(attempt int) time.Duration {
	d := s.cfg.RetryBase
	for i := 0; i < attempt && d < s.cfg.RetryMax; i++ {
		d *= 2
	}
	d = min(d, s.cfg.RetryMax)
	// Full range jitter in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
}

func (s *Sender) send(ctx context.Context, r Report) (Result, outcome) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	now := s.cfg.Now().UTC()
	reasons := r.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	p := Payload{
		V:               ProtocolVersion,
		DeviceID:        s.cfg.DeviceID,
		State:           r.State,
		Reasons:         reasons,
		Seq:             seq,
		TS:              now.Format(time.RFC3339),
		BootID:          s.cfg.BootID,
		User:            r.User,
		UID:             r.UID,
		AttestorVersion: s.cfg.AttestorVersion,
		RecorderVersion: r.RecorderVersion,
		Evidence:        r.Evidence,
	}
	res := Result{Time: now, Seq: seq, State: r.State}
	body, err := json.Marshal(p)
	if err != nil {
		res.Error = err.Error()
		return res, outcomeError
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(body))
	if err != nil {
		res.Error = err.Error()
		return res, outcomeError
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "snoopy-attest/"+s.cfg.AttestorVersion)
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res, outcomeTransient
	}
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent:
		res.OK = true
		return res, outcomeOK
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Error = fmt.Sprintf("HTTP %d: device not authorised", resp.StatusCode)
		return res, outcomeRejected
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return res, outcomeTransient
	default:
		res.Error = fmt.Sprintf("HTTP %d: unexpected response", resp.StatusCode)
		return res, outcomeError
	}
}
