package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/daveio/proxs3/internal/api"
	"github.com/daveio/proxs3/internal/config"
)

func main() {
	configPath := flag.String("config", "/etc/proxs3/proxs3d.json", "path to config file")
	flag.Parse()

	cfg, err := config.LoadDaemonConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	srv, err := api.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		srv.Stop()
	}()

	log.Printf("proxs3d starting, socket=%s", cfg.SocketPath)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
