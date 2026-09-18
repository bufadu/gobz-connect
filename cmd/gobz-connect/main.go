package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
)

// version is injected at build time via -ldflags "-X main.version=<tag>".
// Falls back to "dev" when built without the flag.
var version = "dev"

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "gobz-connect %s\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: gobz-connect [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	cfgPath := flag.String("config", "config.yaml", "path to configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Generate a stable 16-byte device UUID derived from the device name.
	// Using uuid.NewSHA1 ensures the same name always produces the same UUID,
	// which is important for Qobuz Connect device identity.
	deviceUUID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("gobz-connect."+cfg.DeviceName))
	sessionID := deviceUUID[:]

	log.Printf("gobz-connect: device=%q uuid=%s", cfg.DeviceName, deviceUUID)

	stream := NewQobuzStream(cfg, sessionID)
	if err := stream.Start(); err != nil {
		log.Fatalf("stream: %v", err)
	}

	// Wait for SIGINT or SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received %v, shutting down…", sig)

	stream.Stop()
	log.Println("done")
}
