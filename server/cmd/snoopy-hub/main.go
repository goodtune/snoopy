// Command snoopy-hub is a web app, normally on the usual HTTPS port, that
// indexes the snoopy services advertising themselves on the network (mDNS
// via Avahi, or SNOOPY_HUB_STATIC) and shows their screens one at a time
// or several together, through its own reverse proxy. See docs/hub.md.
//
// Exit status: 0 on SIGINT/SIGTERM or -help/-version, 1 on runtime errors
// (e.g. a port that cannot be bound), 2 on invalid flags or SNOOPY_HUB_*
// values.
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

	"snoopy/internal/hub"
)

// Build metadata, set at link time by GoReleaser via -X main.version=... etc.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

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

func main() {
	os.Exit(realMain(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

func realMain(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	cfg, err := hub.ParseConfig(lookupEnv, args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "snoopy-hub: %v\n", err)
		return 2
	}
	if cfg.ShowVersion {
		_, _ = fmt.Fprintf(stdout, "snoopy-hub %s (commit %s, built %s)\n", buildVersion(), commit, date)
		return 0
	}

	logger := log.New(stderr, "", 0)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	logger.Printf("snoopy-hub %s starting", buildVersion())
	if err := hub.Run(ctx, cfg, hub.Options{Logf: logger.Printf, Hup: hup}); err != nil {
		logger.Printf("snoopy-hub: %v", err)
		return 1
	}
	logger.Printf("snoopy-hub stopped")
	return 0
}
