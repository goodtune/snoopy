package registry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// NewHTTPClient returns the client the hub uses to talk to snoopy
// desktops: plain HTTP, never following redirects (a desktop must not be
// able to point the hub at another host), with dial and header timeouts.
func NewHTTPClient(dialTimeout, headerTimeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // desktops are on the LAN; never send them through a proxy
	t.ResponseHeaderTimeout = headerTimeout
	t.MaxIdleConnsPerHost = 4
	t.DialContext = dialer(dialTimeout).DialContext
	return &http.Client{
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// dialer detects a vanished desktop on an idle stream within about 30s
// (TCP keepalive), since snoopy's SSE stream has no heartbeat.
func dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3,
		},
	}
}

// Prober checks every probe target each Interval and reports the result to
// the registry. Poke probes one entry at once (e.g. when its stream drops).
type Prober struct {
	Reg      *Registry
	Client   *http.Client
	Interval time.Duration
	Timeout  time.Duration

	pokeOnce sync.Once
	pokes    chan string
}

func (p *Prober) init() {
	p.pokeOnce.Do(func() { p.pokes = make(chan string, 64) })
}

// Poke asks for an immediate probe of id; it never blocks.
func (p *Prober) Poke(id string) {
	p.init()
	select {
	case p.pokes <- id:
	default:
	}
}

// Run probes until ctx is done, starting immediately.
func (p *Prober) Run(ctx context.Context) {
	p.init()
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	p.probeAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.probeAll(ctx)
		case id := <-p.pokes:
			if s, ok := p.Reg.Get(id); ok {
				p.Reg.ReportProbe(id, p.Probe(ctx, s))
			}
		}
	}
}

func (p *Prober) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, s := range p.Reg.ProbeTargets() {
		wg.Add(1)
		sem <- struct{}{}
		go func(s Service) {
			defer wg.Done()
			defer func() { <-sem }()
			ok := p.Probe(ctx, s)
			if ctx.Err() == nil {
				p.Reg.ReportProbe(s.ID, ok)
			}
		}(s)
	}
	wg.Wait()
}

// Probe reports whether s answers GET /images/waiting.jpg or GET / with a
// 2xx status.
func (p *Prober) Probe(ctx context.Context, s Service) bool {
	for _, path := range []string{"/images/waiting.jpg", "/"} {
		if err := p.get(ctx, s, path); err == nil {
			return true
		}
	}
	return false
}

var errStatus = errors.New("unexpected status")

func (p *Prober) get(ctx context.Context, s Service, path string) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.DialAddr()+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errStatus
	}
	return nil
}
