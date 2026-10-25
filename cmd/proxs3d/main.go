package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/sol1/proxs3/internal/api"
	"github.com/sol1/proxs3/internal/config"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

const pidFile = "/run/proxs3d.pid"

func main() {
	configPath := flag.String("config", "/etc/proxs3/proxs3d.json", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.BoolVar(showVersion, "v", false, "print version and exit")
	resyncStorage := flag.String("resync", "", "scan local cache for STORAGE and upload any files missing or newer in S3, then exit")
	forceLatest := flag.Bool("force-latest", false, "with --resync: also overwrite S3 objects that are newer than the local copy")
	flag.Parse()

	if *showVersion {
		fmt.Println("proxs3d " + version)
		return
	}

	if *resyncStorage != "" {
		cfg, err := config.LoadDaemonConfig(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		if err := runResync(cfg.SocketPath, *resyncStorage, *forceLatest); err != nil {
			log.Fatalf("resync failed: %v", err)
		}
		return
	}

	cfg, err := config.LoadDaemonConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("proxs3d starting, socket=%s, cache=%s, discovered %d storage(s)",
		cfg.SocketPath, cfg.CacheDir, len(cfg.Storages))
	for _, s := range cfg.Storages {
		log.Printf("  storage: %s bucket=%s endpoint=%s", s.StorageID, s.Bucket, s.Endpoint)
	}

	// Write PID file so the Perl plugin can signal us directly
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
		log.Printf("Warning: could not write PID file %s: %v", pidFile, err)
	}
	defer os.Remove(pidFile)

	srv, err := api.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stopped := make(chan struct{})

	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				log.Println("SIGHUP received, reloading configuration...")
				newCfg, err := config.LoadDaemonConfig(*configPath)
				if err != nil {
					log.Printf("Reload failed: %v (keeping current config)", err)
					continue
				}
				if err := srv.Reload(newCfg); err != nil {
					log.Printf("Reload failed: %v (keeping current config)", err)
					continue
				}
				log.Printf("Reloaded: %d storage(s)", len(newCfg.Storages))
			case syscall.SIGINT, syscall.SIGTERM:
				log.Println("Shutting down...")
				if err := srv.Stop(); err != nil {
					log.Printf("Shutdown: %v", err)
				}
				close(stopped)
				return
			}
		}
	}()

	// Start returns ErrServerClosed as soon as shutdown begins; wait for
	// Stop to finish draining requests, then exit 0 so systemd sees a clean
	// stop. Interrupted background uploads stay marked pending and resume
	// from the startup scan.
	if err := srv.Start(); !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Server error: %v", err)
		os.Remove(pidFile)
		os.Exit(1)
	}
	<-stopped
	log.Println("Stopped")
}

// runResync connects to the running daemon's Unix socket, calls /v1/resync,
// and streams the response to stdout. Used by the --resync CLI flag.
func runResync(socketPath, storage string, force bool) error {
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		// No client-side timeout: a large bucket may take an hour or more.
		// Cancellation comes from SIGINT propagating through the request context.
	}

	q := url.Values{}
	q.Set("storage", storage)
	if force {
		q.Set("force", "1")
	}

	req, err := http.NewRequest("GET", "http://daemon/v1/resync?"+q.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("contacting daemon at %s (is proxs3d running?): %w", socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(body))
	}

	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
