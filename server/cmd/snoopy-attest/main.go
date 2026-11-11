// Command snoopy-attest is a root system service that verifies, from outside
// the user session, that Snoopy is genuinely recording a supervised user's
// GNOME session, and reports that state to KProxy over mutually authenticated
// TLS. See docs/attestation.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"snoopy/internal/attest"
	"snoopy/internal/attest/logind"
	"snoopy/internal/attest/pki"
	"snoopy/internal/attest/procfs"
)

// Build metadata, set at link time by GoReleaser via -X main.version=... etc.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildVersion returns the version stamped at link time, falling back to the
// module version recorded by `go install` when built without ldflags.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return version
}

const usageText = `Usage: snoopy-attest [-config FILE] [command] [flags]

Commands:
  run            run the attestation service (default)
  keygen [-force] create the device key and self-signed client certificate
  pin            print the device certificate's SPKI pin to register in KProxy
  check [-json]  run all checks once and print the result (exit 0 if recording)

Global flags:
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snoopy-attest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("config", attest.DefaultConfigFile, "configuration file (EnvironmentFile syntax)")
	showVersion := fs.Bool("version", false, "print version information and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "snoopy-attest %s (commit %s, built %s)\n", buildVersion(), commit, date)
		return 0
	}
	cmd, rest := "run", fs.Args()
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}

	settings, err := attest.LoadSettings(*configFile, os.Environ())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest: %v\n", err)
		return 2
	}
	cfg, err := attest.Parse(settings, os.Hostname)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest: invalid configuration (%s):\n%v\n", *configFile, err)
		return 2
	}

	switch cmd {
	case "run":
		return cmdRun(cfg, rest, stderr)
	case "keygen":
		return cmdKeygen(cfg, rest, stdout, stderr)
	case "pin":
		return cmdPin(cfg, rest, stdout, stderr)
	case "check":
		return cmdCheck(cfg, rest, stdout, stderr)
	case "help":
		fs.Usage()
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "snoopy-attest: unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}
}

func newSub(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("snoopy-attest "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseSub parses subcommand flags; it returns an exit code to stop with, or
// -1 to continue.
func parseSub(fs *flag.FlagSet, args []string, stderr io.Writer) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s: unexpected arguments %q\n", fs.Name(), fs.Args())
		return 2
	}
	return -1
}

func warnNotRoot(stderr io.Writer, what string) {
	if os.Geteuid() != 0 {
		_, _ = fmt.Fprintf(stderr, "warning: not running as root; %s\n", what)
	}
}

func cmdRun(cfg *attest.Config, args []string, stderr io.Writer) int {
	fs := newSub("run", stderr)
	if rc := parseSub(fs, args, stderr); rc >= 0 {
		return rc
	}
	logger := log.New(stderr, "", 0)
	if err := cfg.RequireDevice(); err != nil {
		logger.Printf("invalid configuration: %v", err)
		return 2
	}
	if err := cfg.Resolve(nil); err != nil {
		logger.Printf("invalid configuration: %v", err)
		return 2
	}
	warnNotRoot(stderr, "other users' processes cannot be inspected and every check will fail")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	lc, err := logind.Connect()
	if err != nil {
		logger.Printf("%v", err)
		return 1
	}
	defer func() { _ = lc.Close() }()
	err = attest.Run(ctx, cfg, buildVersion(), attest.Deps{
		Logind: lc,
		Proc:   procfs.Default,
		Now:    time.Now,
		Logf:   logger.Printf,
	})
	if err != nil {
		logger.Printf("fatal: %v", err)
		return 1
	}
	return 0
}

func cmdKeygen(cfg *attest.Config, args []string, stdout, stderr io.Writer) int {
	fs := newSub("keygen", stderr)
	force := fs.Bool("force", false, "replace an existing key and certificate (the old pin stops working)")
	if rc := parseSub(fs, args, stderr); rc >= 0 {
		return rc
	}
	if err := cfg.RequireDevice(); err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest keygen: %v\n", err)
		return 2
	}
	warnNotRoot(stderr, "the key must be readable by root only; run keygen as root")
	cert, err := pki.Generate(cfg.StateDir, cfg.DeviceID, *force, time.Now())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest keygen: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "wrote %s/%s and %s/%s for device %q (valid until %s)\n",
		cfg.StateDir, pki.KeyFile, cfg.StateDir, pki.CertFile, cfg.DeviceID, cert.NotAfter.Format("2006-01-02"))
	_, _ = fmt.Fprintf(stderr, "register this pin in KProxy:\n")
	_, _ = fmt.Fprintln(stdout, pki.Pin(cert))
	return 0
}

func cmdPin(cfg *attest.Config, args []string, stdout, stderr io.Writer) int {
	fs := newSub("pin", stderr)
	if rc := parseSub(fs, args, stderr); rc >= 0 {
		return rc
	}
	cert, err := pki.LoadCertificate(cfg.StateDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest pin: %v (run `snoopy-attest keygen` first)\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, pki.Pin(cert))
	return 0
}

func cmdCheck(cfg *attest.Config, args []string, stdout, stderr io.Writer) int {
	fs := newSub("check", stderr)
	asJSON := fs.Bool("json", false, "print the evaluation as JSON")
	if rc := parseSub(fs, args, stderr); rc >= 0 {
		return rc
	}
	if err := cfg.Resolve(nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest check: invalid configuration: %v\n", err)
		return 2
	}
	warnNotRoot(stderr, "/proc of other users is unreadable, so the recorder checks will fail")
	lc, err := logind.Connect()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snoopy-attest check: %v\n", err)
		return 1
	}
	defer func() { _ = lc.Close() }()
	logger := log.New(stderr, "", 0)
	// No URL: check never sends heartbeats. No grace either: an admin wants
	// to see the plain result.
	checkCfg := *cfg
	checkCfg.URL = ""
	checkCfg.StartupGrace = 0
	svc, err := attest.NewService(&checkCfg, buildVersion(), attest.Deps{
		Logind: lc,
		Proc:   procfs.Default,
		Now:    time.Now,
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		logger.Printf("snoopy-attest check: %v", err)
		return 1
	}
	if svc.Frame != nil {
		svc.Frame.Logf = logger.Printf
	}
	ev := svc.Evaluate(context.Background())
	if *asJSON {
		err = attest.WriteJSON(stdout, ev)
	} else {
		err = attest.WriteText(stdout, ev, cfg.DeviceID)
	}
	if err != nil {
		return 1
	}
	if ev.State != attest.StateRecording {
		return 1
	}
	return 0
}
