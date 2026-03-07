package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daveio/proxs3/internal/cache"
	"github.com/daveio/proxs3/internal/config"
	"github.com/daveio/proxs3/internal/s3client"
)

// Server is the Unix socket API server that the Perl plugin communicates with.
type Server struct {
	cfg      *config.DaemonConfig
	clients  map[string]*s3client.Client
	cache    *cache.FileCache
	health   map[string]bool
	healthMu sync.RWMutex
	listener net.Listener
	server   *http.Server
}

// New creates a new API server.
func New(cfg *config.DaemonConfig) (*Server, error) {
	fc, err := cache.New(cfg.CacheDir, cfg.CacheMaxMB)
	if err != nil {
		return nil, err
	}

	clients := make(map[string]*s3client.Client)
	health := make(map[string]bool)
	for _, sc := range cfg.Storages {
		c, err := s3client.New(sc, cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("creating client for %s: %w", sc.StorageID, err)
		}
		clients[sc.StorageID] = c
		health[sc.StorageID] = false
	}

	return &Server{
		cfg:     cfg,
		clients: clients,
		cache:   fc,
		health:  health,
	}, nil
}

// Start begins listening on the Unix socket and serving requests.
func (s *Server) Start() error {
	os.Remove(s.cfg.SocketPath)

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.SocketPath, err)
	}
	os.Chmod(s.cfg.SocketPath, 0660)
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/list", s.handleList)
	mux.HandleFunc("/v1/download", s.handleDownload)
	mux.HandleFunc("/v1/upload", s.handleUpload)
	mux.HandleFunc("/v1/delete", s.handleDelete)
	mux.HandleFunc("/v1/path", s.handlePath)

	s.server = &http.Server{Handler: mux}

	go s.healthLoop()

	return s.server.Serve(ln)
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) healthLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Initial check
	s.checkAllHealth()

	for range ticker.C {
		s.checkAllHealth()
	}
}

func (s *Server) checkAllHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	for id, c := range s.clients {
		err := c.HeadBucket(ctx)
		s.health[id] = (err == nil)
	}
}

// --- HTTP Handlers ---

// StatusResponse is returned by the status endpoint.
type StatusResponse struct {
	StorageID string `json:"storage_id"`
	Online    bool   `json:"online"`
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
	Available int64  `json:"available"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	if storageID == "" {
		http.Error(w, "missing storage parameter", http.StatusBadRequest)
		return
	}

	s.healthMu.RLock()
	online := s.health[storageID]
	s.healthMu.RUnlock()

	// S3 doesn't have real capacity limits, report large values
	resp := StatusResponse{
		StorageID: storageID,
		Online:    online,
		Total:     1099511627776, // 1 TiB placeholder
		Used:      0,
		Available: 1099511627776,
	}

	writeJSON(w, resp)
}

// VolumeInfo represents a single volume (ISO, template, snippet, etc.)
type VolumeInfo struct {
	Volume  string `json:"volume"`
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	content := r.URL.Query().Get("content")

	client, ok := s.clients[storageID]
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	prefix := contentToPrefix(content)
	objects, err := client.ListObjects(r.Context(), prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var volumes []VolumeInfo
	for _, obj := range objects {
		// Skip directory markers
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		vol := VolumeInfo{
			Volume:  fmt.Sprintf("%s:%s/%s", storageID, content, filepath.Base(obj.Key)),
			Key:     obj.Key,
			Size:    obj.Size,
			Format:  detectFormat(obj.Key),
			Content: content,
		}
		volumes = append(volumes, vol)
	}

	writeJSON(w, volumes)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	client, ok := s.clients[storageID]
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	// Check cache first
	if cached := s.cache.Path(storageID, key); cached != "" {
		writeJSON(w, map[string]string{"path": cached})
		return
	}

	// Download from S3
	body, _, err := client.GetObject(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer body.Close()

	// Store in cache
	localPath, err := s.cache.Store(storageID, key, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"path": localPath})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")
	localPath := r.URL.Query().Get("path")

	client, ok := s.clients[storageID]
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	f, err := os.Open(localPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("opening file: %v", err), http.StatusBadRequest)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, fmt.Sprintf("stat file: %v", err), http.StatusInternalServerError)
		return
	}

	if err := client.PutObject(r.Context(), key, f, info.Size()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	client, ok := s.clients[storageID]
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	if err := client.DeleteObject(r.Context(), key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Also remove from cache
	s.cache.Remove(storageID, key)

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handlePath(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	// If cached, return cache path; otherwise download first
	if cached := s.cache.Path(storageID, key); cached != "" {
		writeJSON(w, map[string]string{"path": cached})
		return
	}

	// Trigger download
	s.handleDownload(w, r)
}

// --- Helpers ---

func contentToPrefix(content string) string {
	switch content {
	case "iso":
		return "template/iso/"
	case "vztmpl":
		return "template/cache/"
	case "snippets":
		return "snippets/"
	case "backup":
		return "dump/"
	default:
		return content + "/"
	}
}

func detectFormat(key string) string {
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".iso"):
		return "iso"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".tar.zst"):
		return "tgz"
	default:
		return "raw"
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
