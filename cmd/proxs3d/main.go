package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sol1/proxs3/internal/api"
	"github.com/sol1/proxs3/internal/config"
)

const pidFile = "/run/proxs3d.pid"

func main() {
	configPath := flag.String("config", "/etc/proxs3/proxs3d.json", "path to config file")
	flag.Parse()

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
				srv.Stop()
				return
			}
		}
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
