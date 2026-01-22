package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	dest        = "org.gnome.Shell.Screencast"
	objPath     = "/org/gnome/Shell/Screencast"
	iface       = "org.gnome.Shell.Screencast"
	startMethod = iface + ".Screencast"
	stopMethod  = iface + ".StopScreencast"
)

func main() {
	var (
		outDir   = flag.String("out", "", "Output directory (default: ~/Videos/screencapture)")
		segment  = flag.Duration("segment", 30*time.Minute, "Segment duration")
		pause    = flag.Duration("pause", 1*time.Second, "Pause between segments")
		template = flag.String("template", "screen-%d-%t.webm", "Filename template used by GNOME Shell")
	)
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("UserHomeDir: %v", err)
	}
	if *outDir == "" {
		*outDir = filepath.Join(home, "Videos", "screencapture")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	// Connect to the *session* bus (this must run in the logged-in user session).
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Fatalf("ConnectSessionBus: %v", err)
	}
	defer conn.Close()

	obj := conn.Object(dest, dbus.ObjectPath(objPath))
	ctx := context.Background()

	fullTemplate := filepath.Join(*outDir, *template)

	log.Printf("Starting screencast loop: out=%s segment=%s", *outDir, segment.String())

	// Start the first recording
	opts := map[string]dbus.Variant{}
	call := obj.CallWithContext(ctx, startMethod, 0, fullTemplate, opts)
	if call.Err != nil {
		log.Fatalf("Failed to start initial screencast: %v", call.Err)
	}
	log.Printf("Started initial recording")

	for {
		// Wait for the segment duration
		time.Sleep(*segment)

		// Start the next recording BEFORE stopping the current one
		// This ensures continuous coverage with no gaps
		call = obj.CallWithContext(ctx, startMethod, 0, fullTemplate, opts)
		if call.Err != nil {
			log.Printf("Start next screencast failed: %v", call.Err)
			// If we can't start the next one, try to stop and restart cleanly
			obj.CallWithContext(ctx, stopMethod, 0)
			time.Sleep(5 * time.Second)
			continue
		}

		// Now stop the previous recording
		// GNOME Shell may have already auto-stopped it when we started the new one
		call = obj.CallWithContext(ctx, stopMethod, 0)
		if call.Err != nil {
			log.Printf("Stop previous screencast failed (may already be stopped): %v", call.Err)
			// This is often okay - GNOME may auto-stop when starting a new one
		}

		// Brief pause to ensure clean transition
		time.Sleep(*pause)

		// Optional: print progress heartbeat
		fmt.Print(".")
	}
}
