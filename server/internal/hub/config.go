// Package hub implements snoopy-hub: a web app, normally on the usual
// HTTPS port, that lists the snoopy services advertising themselves on the
// network (or configured statically) and shows their screens, one at a
// time or several together, through a reverse proxy.
package hub

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"snoopy/internal/hub/registry"
)

// EnvPrefix is prepended to a flag's name (upper-cased, "-" replaced by
// "_") to form the environment variable that supplies its default.
const EnvPrefix = "SNOOPY_HUB_"

// DefaultStateDir holds the generated certificate (systemd StateDirectory).
const DefaultStateDir = "/var/lib/snoopy-hub"

// Config is the validated snoopy-hub configuration.
type Config struct {
	Addr          string
	HTTPSPort     int
	HTTPPort      int
	TLSCert       string
	TLSKey        string
	PlainHTTP     bool
	User          string
	Password      string
	Static        []registry.StaticEntry
	ProbeInterval time.Duration
	StateDir      string
	ShowVersion   bool
}

// EnvName returns the environment variable for flag name, e.g.
// "probe-interval" -> "SNOOPY_HUB_PROBE_INTERVAL".
func EnvName(name string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func usage(name, text string) string {
	return fmt.Sprintf("%s (env %s)", text, EnvName(name))
}

// envDefaults mirrors the recorder's envDefaults (server/config.go) with the
// SNOOPY_HUB_ prefix: unset or empty variables fall back to the built-in
// default, invalid ones are collected and reported together.
type envDefaults struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (e *envDefaults) value(name string) (string, string, bool) {
	key := EnvName(name)
	v, ok := e.lookup(key)
	v = strings.TrimSpace(v)
	return key, v, ok && v != ""
}

func (e *envDefaults) string(name, def string) string {
	if _, v, ok := e.value(name); ok {
		return v
	}
	return def
}

func (e *envDefaults) duration(name string, def time.Duration) time.Duration {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q: not a duration (e.g. 15s, 1m)", key, v))
		return def
	}
	return d
}

func (e *envDefaults) int(name string, def int) int {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q: not an integer", key, v))
		return def
	}
	return n
}

func (e *envDefaults) bool(name string, def bool) bool {
	key, v, ok := e.value(name)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	e.errs = append(e.errs, fmt.Errorf("%s=%q: not a boolean (1, true, yes / 0, false, no)", key, v))
	return def
}

func (e *envDefaults) err() error {
	if len(e.errs) == 0 {
		return nil
	}
	msgs := make([]string, len(e.errs))
	for i, err := range e.errs {
		msgs[i] = err.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}

// ParseConfig builds the configuration from the environment and the
// command line (flag > SNOOPY_HUB_* > built-in default). It writes -help
// output to out. Errors wrap flag.ErrHelp for -help; any other error means
// exit status 2. The password never appears in the help text.
func ParseConfig(lookupEnv func(string) (string, bool), args []string, out io.Writer) (Config, error) {
	env := &envDefaults{lookup: lookupEnv}
	fs := flag.NewFlagSet("snoopy-hub", flag.ContinueOnError)
	fs.SetOutput(out)
	var c Config
	var static string
	fs.StringVar(&c.Addr, "addr", env.string("addr", "0.0.0.0"), usage("addr", "Bind address for the web listeners"))
	fs.IntVar(&c.HTTPSPort, "https-port", env.int("https-port", 443), usage("https-port", "HTTPS port"))
	fs.IntVar(&c.HTTPPort, "http-port", env.int("http-port", 80), usage("http-port", "HTTP port: redirects to HTTPS (0 disables); with -plain-http it serves the app"))
	fs.StringVar(&c.TLSCert, "tls-cert", env.string("tls-cert", ""), usage("tls-cert", "PEM certificate chain file (with -tls-key; reloaded on change or SIGHUP). Empty: a self-signed certificate in -state-dir"))
	fs.StringVar(&c.TLSKey, "tls-key", env.string("tls-key", ""), usage("tls-key", "PEM private key file for -tls-cert"))
	fs.BoolVar(&c.PlainHTTP, "plain-http", env.bool("plain-http", false), usage("plain-http", "Serve the app over plain HTTP on -http-port, for a reverse proxy that terminates TLS"))
	fs.StringVar(&c.User, "user", env.string("user", ""), usage("user", "HTTP Basic user name (with -password); empty: no authentication"))
	// The password's default is applied after parsing so -help never shows it.
	fs.StringVar(&c.Password, "password", "", usage("password", "HTTP Basic password (with -user; prefer the environment variable, command lines are visible to other local users)"))
	envPassword := env.string("password", "")
	fs.StringVar(&static, "static", env.string("static", ""), usage("static", "Desktops to list without mDNS: name=host[:port][;rtsp=PORT],..."))
	fs.DurationVar(&c.ProbeInterval, "probe-interval", env.duration("probe-interval", 15*time.Second), usage("probe-interval", "Interval between health checks of each desktop"))
	fs.StringVar(&c.StateDir, "state-dir", env.string("state-dir", DefaultStateDir), usage("state-dir", "Directory for the generated self-signed certificate"))
	fs.BoolVar(&c.ShowVersion, "version", false, "Print version information and exit")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.ShowVersion {
		return c, nil
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if c.Password == "" {
		c.Password = envPassword
	}
	if err := env.err(); err != nil {
		return c, fmt.Errorf("invalid environment: %w", err)
	}
	var err error
	if c.Static, err = registry.ParseStatic(static); err != nil {
		return c, fmt.Errorf("invalid -static: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var errs []string
	if c.Addr == "" {
		errs = append(errs, "-addr must not be empty")
	}
	if c.HTTPPort < 0 || c.HTTPPort > 65535 {
		errs = append(errs, "-http-port must be 0-65535")
	}
	if c.PlainHTTP {
		if c.HTTPPort == 0 {
			errs = append(errs, "-plain-http needs a non-zero -http-port")
		}
		if c.TLSCert != "" || c.TLSKey != "" {
			errs = append(errs, "-tls-cert and -tls-key cannot be used with -plain-http")
		}
	} else {
		if c.HTTPSPort < 1 || c.HTTPSPort > 65535 {
			errs = append(errs, "-https-port must be 1-65535")
		}
		if c.HTTPPort != 0 && c.HTTPPort == c.HTTPSPort {
			errs = append(errs, "-http-port and -https-port must differ")
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, "-tls-cert and -tls-key must be set together")
	}
	if (c.User == "") != (c.Password == "") {
		errs = append(errs, "-user and -password must be set together")
	}
	if strings.Contains(c.User, ":") {
		errs = append(errs, "-user must not contain \":\"")
	}
	if c.ProbeInterval < time.Second {
		errs = append(errs, "-probe-interval must be at least 1s")
	}
	if !c.PlainHTTP && c.TLSCert == "" && c.StateDir == "" {
		errs = append(errs, "-state-dir must not be empty when generating a certificate")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
