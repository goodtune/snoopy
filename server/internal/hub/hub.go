package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"snoopy/internal/hub/discovery"
	"snoopy/internal/hub/registry"
	"snoopy/internal/hub/upstream"
	"snoopy/internal/hub/web"
)

// Options are the inputs of Run besides the configuration.
type Options struct {
	// Logf logs one line (required).
	Logf func(string, ...any)
	// Browser discovers services; nil means Avahi on the system bus.
	Browser discovery.Browser
	// Hup delivers SIGHUP (reload the TLS certificate); may be nil.
	Hup <-chan os.Signal
	// Ready, if set, is called with the bound listener addresses (app
	// first, then the redirect listener if any) once serving.
	Ready func(app net.Addr, redirect net.Addr)
	// CertCheckInterval is how often user-supplied certificate files are
	// checked for changes (default 30s).
	CertCheckInterval time.Duration
}

const shutdownGrace = 5 * time.Second

// Run serves until ctx is done (nil error) or a listener fails.
func Run(ctx context.Context, c Config, o Options) error {
	logf := o.Logf
	// Background goroutines stop (cancel) before Run waits for them (wg),
	// also on an early error return.
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.User == "" {
		logf("Warning: no -user/-password set: anyone who can reach the hub can watch every screen")
	}

	// Registry, health probes, discovery and upstream streams.
	reg := registry.New(nil, logf)
	client := registry.NewHTTPClient(5*time.Second, 10*time.Second)
	prober := &registry.Prober{Reg: reg, Client: client, Interval: c.ProbeInterval, Timeout: min(c.ProbeInterval, 5*time.Second)}
	frames := upstream.New(reg.Get, client, logf)
	frames.OnDrop = prober.Poke
	reg.AddStatic(c.Static)
	for _, s := range c.Static {
		logf("Static entry %q at %s (port %d)", s.Name, s.Host, s.Port)
	}

	goRun := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	goRun(func() { logChanges(ctx, reg, logf) })
	goRun(func() { prober.Run(ctx) })
	goRun(func() { frames.Follow(ctx, reg) })
	goRun(func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reg.Sweep()
			}
		}
	})
	browser := o.Browser
	if browser == nil {
		browser = discovery.NewAvahi(logf)
	}
	goRun(func() { discovery.Supervise(ctx, browser, reg.Apply, logf, time.Second, time.Minute) })

	// Web listeners.
	appHandler := web.New(web.Options{
		Registry: reg, Frames: frames, User: c.User, Password: c.Password,
		HSTS: !c.PlainHTTP && c.TLSCert != "",
	})
	var appSrv, redirSrv *http.Server
	var appLn, redirLn net.Listener
	var err error

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    32 << 10,
			// No ReadTimeout or WriteTimeout: they would cut event streams
			// (an expired read deadline cancels the request context). The
			// app only serves GETs, and the handler bounds the write time of
			// every response that is not a stream.
			ErrorLog:    log.New(serverLogWriter{logf}, "", 0),
			BaseContext: func(net.Listener) context.Context { return baseCtx },
		}
	}

	if c.PlainHTTP {
		appLn, err = net.Listen("tcp", net.JoinHostPort(c.Addr, strconv.Itoa(c.HTTPPort)))
		if err != nil {
			return err
		}
		appSrv = newServer(appHandler)
		logf("Serving the hub over plain HTTP on %s (expecting a TLS-terminating reverse proxy)", appLn.Addr())
	} else {
		var src *certSource
		if c.TLSCert != "" {
			src, err = newFileCertSource(c.TLSCert, c.TLSKey, logf)
			if err != nil {
				return err
			}
			interval := o.CertCheckInterval
			if interval <= 0 {
				interval = 30 * time.Second
			}
			goRun(func() { src.watch(ctx, interval, o.Hup) })
			logf("Using TLS certificate %s (SHA-256 %s)", c.TLSCert, Fingerprint(src.cert.Certificate[0]))
		} else {
			names, ips := localNames()
			cert, created, err := loadOrCreateSelfSigned(c.StateDir, names, ips, time.Now())
			if err != nil {
				return fmt.Errorf("self-signed certificate in %s: %w", c.StateDir, err)
			}
			src = &certSource{cert: cert}
			verb := "Using"
			if created {
				verb = "Generated"
			}
			logf("%s self-signed TLS certificate %s for %s (SHA-256 fingerprint %s); browsers will warn until you trust it, see docs/hub.md",
				verb, c.StateDir+"/"+selfCertFile, strings.Join(names, ", "), Fingerprint(cert.Certificate[0]))
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(c.Addr, strconv.Itoa(c.HTTPSPort)))
		if err != nil {
			return err
		}
		appSrv = newServer(appHandler)
		appSrv.TLSConfig = tlsConfig(src.get)
		appLn = ln
		logf("Serving the hub over HTTPS on %s", ln.Addr())
		if c.HTTPPort != 0 {
			redirLn, err = net.Listen("tcp", net.JoinHostPort(c.Addr, strconv.Itoa(c.HTTPPort)))
			if err != nil {
				_ = ln.Close()
				return err
			}
			redirSrv = newServer(RedirectHandler(c.HTTPSPort))
			logf("Redirecting HTTP on %s to HTTPS", redirLn.Addr())
		}
	}

	errc := make(chan error, 2)
	go func() {
		var err error
		if appSrv.TLSConfig != nil {
			err = appSrv.ServeTLS(appLn, "", "")
		} else {
			err = appSrv.Serve(appLn)
		}
		errc <- err
	}()
	if redirSrv != nil {
		go func() { errc <- redirSrv.Serve(redirLn) }()
	}
	if o.Ready != nil {
		var ra net.Addr
		if redirLn != nil {
			ra = redirLn.Addr()
		}
		o.Ready(appLn.Addr(), ra)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	cancel()
	cancelBase() // ends event streams so Shutdown does not wait for them
	sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer scancel()
	for _, s := range []*http.Server{appSrv, redirSrv} {
		if s != nil {
			if err := s.Shutdown(sctx); err != nil {
				_ = s.Close()
			}
		}
	}
	return runErr
}

// RedirectHandler redirects every request to the same host and path over
// HTTPS on httpsPort.
func RedirectHandler(httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if !registry.ValidHost(host) || strings.Contains(host, "%") {
			http.Error(w, "invalid Host header", http.StatusBadRequest)
			return
		}
		if httpsPort != 443 {
			host = net.JoinHostPort(host, strconv.Itoa(httpsPort))
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		w.Header().Set("Content-Security-Policy", web.ContentSecurityPolicy)
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// logChanges logs services appearing, disappearing and changing state.
func logChanges(ctx context.Context, reg *registry.Registry, logf func(string, ...any)) {
	for ctx.Err() == nil {
		ch, cancel := reg.Subscribe(1024)
		logFrom(ctx, ch, logf)
		cancel()
	}
}

func logFrom(ctx context.Context, ch <-chan registry.Change, logf func(string, ...any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			s := c.Service
			switch c.Kind {
			case registry.Added:
				state := "offline"
				if s.Online {
					state = "online"
				}
				logf("Service %q (%s, %s, %s) added, %s", s.Name, s.Host, s.DialAddr(), s.Source, state)
			case registry.Removed:
				logf("Service %q (%s) removed", s.Name, s.Host)
			case registry.Online, registry.Offline:
				logf("Service %q (%s) is %s", s.Name, s.Host, c.Kind)
			}
		}
	}
}

// serverLogWriter forwards net/http's error log, minus TLS handshake
// failures: every browser that has not trusted the self-signed certificate
// yet causes one.
type serverLogWriter struct{ logf func(string, ...any) }

func (w serverLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if !strings.Contains(line, "TLS handshake error") {
		w.logf("%s", line)
	}
	return len(p), nil
}
