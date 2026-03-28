package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sol1/proxs3/internal/cache"
	"github.com/sol1/proxs3/internal/config"
	"github.com/sol1/proxs3/internal/s3client"
)

// Server is the Unix socket API server that the Perl plugin communicates with.
type Server struct {
	cfg      *config.DaemonConfig
	clients  map[string]s3client.S3Client
	cache    *cache.FileCache
	health   map[string]bool
	usage    map[string]int64
	healthMu sync.RWMutex
	clientMu sync.RWMutex
	listener net.Listener
	server   *http.Server
}

// New creates a new API server.
func New(cfg *config.DaemonConfig) (*Server, error) {
	fc, err := cache.New(cfg.CacheDir, cfg.CacheMaxMB)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		clients: make(map[string]s3client.S3Client),
		cache:   fc,
		health:  make(map[string]bool),
		usage:   make(map[string]int64),
	}

	if err := s.initClients(cfg); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) initClients(cfg *config.DaemonConfig) error {
	clients := make(map[string]s3client.S3Client)
	health := make(map[string]bool)

	for _, sc := range cfg.Storages {
		c, err := s3client.New(sc, cfg.Proxy)
		if err != nil {
			return fmt.Errorf("creating client for %s: %w", sc.StorageID, err)
		}
		clients[sc.StorageID] = c
		health[sc.StorageID] = false
	}

	s.clientMu.Lock()
	s.clients = clients
	s.clientMu.Unlock()

	s.healthMu.Lock()
	s.health = health
	s.healthMu.Unlock()

	return nil
}

// Reload re-reads config and reinitializes clients for added/changed storages.
func (s *Server) Reload(cfg *config.DaemonConfig) error {
	if err := s.initClients(cfg); err != nil {
		return err
	}
	s.cfg = cfg

	// Update cache if dir changed
	if cfg.CacheDir != s.cfg.CacheDir || cfg.CacheMaxMB != s.cfg.CacheMaxMB {
		fc, err := cache.New(cfg.CacheDir, cfg.CacheMaxMB)
		if err != nil {
			return fmt.Errorf("reinitializing cache: %w", err)
		}
		s.cache = fc
	}

	// Run immediate health check
	go s.checkAllHealth()
	return nil
}

// Start begins listening on the Unix socket and serving requests.
func (s *Server) Start() error {
	os.Remove(s.cfg.SocketPath)

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.SocketPath, err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0660); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/list", s.handleList)
	mux.HandleFunc("/v1/download", s.handleDownload)
	mux.HandleFunc("/v1/upload", s.handleUpload)
	mux.HandleFunc("/v1/delete", s.handleDelete)
	mux.HandleFunc("/v1/copy", s.handleCopy)
	mux.HandleFunc("/v1/rename", s.handleRename)
	mux.HandleFunc("/v1/get-attr", s.handleGetAttr)
	mux.HandleFunc("/v1/set-attr", s.handleSetAttr)
	mux.HandleFunc("/v1/path", s.handlePath)
	mux.HandleFunc("/v1/config", s.handleConfig)

	s.server = &http.Server{Handler: mux}

	go s.healthLoop()
	go s.watchCacheDirs()
	go s.cacheAgeLoop()

	return s.server.Serve(ln)
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) healthLoop() {
	// Initial check
	s.checkAllHealth()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.checkAllHealth()
	}
}

func (s *Server) checkAllHealth() {
	s.clientMu.RLock()
	clients := s.clients
	s.clientMu.RUnlock()

	results := make(map[string]bool)
	usageResults := make(map[string]int64)
	for id, c := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := c.HeadBucket(ctx)
		results[id] = (err == nil)
		if err != nil {
			log.Printf("health check failed for %s: %v", id, err)
			cancel()
			continue
		}

		// Sum object sizes to report used space
		objects, err := c.ListObjects(ctx, "")
		cancel()
		if err == nil {
			var total int64
			for _, obj := range objects {
				total += obj.Size
			}
			usageResults[id] = total
		}
	}

	s.healthMu.Lock()
	s.health = results
	s.usage = usageResults
	s.healthMu.Unlock()
}

func (s *Server) cacheAgeLoop() {
	// Run every hour
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Initial run after short delay
	time.Sleep(30 * time.Second)
	s.evictByAge()

	for range ticker.C {
		s.evictByAge()
	}
}

func (s *Server) evictByAge() {
	for _, sc := range s.cfg.Storages {
		if sc.CacheMaxAge <= 0 {
			continue
		}
		maxAge := time.Duration(sc.CacheMaxAge) * 24 * time.Hour
		removed := s.cache.EvictByAge(sc.StorageID, maxAge)
		if removed > 0 {
			log.Printf("cache age-evict: removed %d files from %s (max-age %dd)",
				removed, sc.StorageID, sc.CacheMaxAge)
		}
	}
}

func (s *Server) getClient(storageID string) (s3client.S3Client, bool) {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	c, ok := s.clients[storageID]
	return c, ok
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

	_, known := s.getClient(storageID)
	if !known {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	s.healthMu.RLock()
	online := s.health[storageID]
	s.healthMu.RUnlock()

	s.healthMu.RLock()
	used := s.usage[storageID]
	s.healthMu.RUnlock()

	// S3 has no real capacity limit — report used from actual object sizes
	// and always show headroom free so PVE never thinks the storage is full
	headroom := s.cfg.HeadroomGB * 1073741824 // convert GiB to bytes
	resp := StatusResponse{
		StorageID: storageID,
		Online:    online,
		Total:     used + headroom,
		Used:      used,
		Available: headroom,
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

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	prefix := contentToPrefix(content)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	objects, err := client.ListObjects(ctx, prefix)
	if err != nil {
		// S3 unreachable — still include locally cached files rather than blocking PVE
		log.Printf("list failed for %s/%s (S3 unreachable?): %v", storageID, content, err)
		var volumes []VolumeInfo
		localDir := filepath.Join(s.cfg.CacheDir, storageID, prefix)
		s.mergeLocalFiles(&volumes, localDir, prefix, content, storageID, nil)
		writeJSON(w, volumes)
		return
	}

	// Build a set of S3 keys for quick lookup
	s3Keys := make(map[string]bool, len(objects))
	var volumes []VolumeInfo
	for _, obj := range objects {
		// Skip directory markers
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		s3Keys[obj.Key] = true
		// images volids use "vmid/diskname" format; others use "content/filename"
		var volname string
		if content == "images" {
			volname = strings.TrimPrefix(obj.Key, prefix)
		} else {
			volname = content + "/" + filepath.Base(obj.Key)
		}
		vol := VolumeInfo{
			Volume:  fmt.Sprintf("%s:%s", storageID, volname),
			Key:     obj.Key,
			Size:    obj.Size,
			Format:  detectFormat(obj.Key),
			Content: content,
		}
		volumes = append(volumes, vol)
	}

	// Include locally cached files not yet uploaded to S3.
	// PVE (and Terraform providers) may write files directly to the cache
	// directory; the watcher uploads them asynchronously. Without this,
	// list_volumes returns empty until the upload completes.
	localDir := filepath.Join(s.cfg.CacheDir, storageID, prefix)
	s.mergeLocalFiles(&volumes, localDir, prefix, content, storageID, s3Keys)

	writeJSON(w, volumes)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	// Check if we have a cached entry
	if cached := s.cache.Path(storageID, key); cached != "" {
		// Validate against S3 metadata (HeadObject is cheap)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		info, err := client.HeadObject(ctx, key)
		cancel()
		if err != nil {
			// S3 unreachable — serve stale cache rather than failing
			log.Printf("download: S3 unreachable for %s, serving cached copy", key)
			writeJSON(w, map[string]string{"path": cached})
			return
		}
		if !s.cache.IsStale(storageID, key, info.ETag, info.LastModified) {
			writeJSON(w, map[string]string{"path": cached})
			return
		}
		// Stale — invalidate and re-download
		s.cache.Invalidate(storageID, key)
	}

	// Download from S3 (no tight timeout here — large files take time)
	result, err := client.GetObject(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	// Store in cache with metadata
	meta := cache.FileMeta{
		ETag:         result.ETag,
		LastModified: result.LastModified,
		Size:         result.Size,
	}
	localPath, err := s.cache.Store(storageID, key, result.Body, meta)
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

	client, ok := s.getClient(storageID)
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

	// Cache the uploaded file with current metadata
	uploadMeta := cache.FileMeta{
		Size:         info.Size(),
		LastModified: time.Now(),
	}
	s.cache.Link(storageID, key, localPath, uploadMeta)

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	client, ok := s.getClient(storageID)
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

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	srcKey := r.URL.Query().Get("src_key")
	dstKey := r.URL.Query().Get("dst_key")

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	if err := client.CopyObject(ctx, srcKey, dstKey); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If the source is cached locally, copy the cache entry too
	if src := s.cache.Path(storageID, srcKey); src != "" {
		dst := s.cache.ExpectedPath(storageID, dstKey)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err == nil {
			if f, err := os.Open(src); err == nil {
				if d, err := os.Create(dst); err == nil {
					io.Copy(d, f)
					d.Close()
				}
				f.Close()
			}
		}
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	srcKey := r.URL.Query().Get("src_key")
	dstKey := r.URL.Query().Get("dst_key")

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// Copy then delete in S3 (S3 has no native rename)
	if err := client.CopyObject(ctx, srcKey, dstKey); err != nil {
		http.Error(w, fmt.Sprintf("copy failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := client.DeleteObject(ctx, srcKey); err != nil {
		log.Printf("rename: copy succeeded but delete of %s failed: %v", srcKey, err)
	}

	// Rename in local cache if present
	src := s.cache.ExpectedPath(storageID, srcKey)
	dst := s.cache.ExpectedPath(storageID, dstKey)
	if _, err := os.Stat(src); err == nil {
		clearImmutable(src)
		os.MkdirAll(filepath.Dir(dst), 0755)
		os.Rename(src, dst)
	}
	s.cache.Invalidate(storageID, srcKey)

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleGetAttr(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	tags, err := client.GetObjectTagging(ctx, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, tags)
}

func (s *Server) handleSetAttr(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")
	attr := r.URL.Query().Get("attr")
	value := r.URL.Query().Get("value")

	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Get existing tags, update the one we want, write back
	tags, err := client.GetObjectTagging(ctx, key)
	if err != nil {
		// No existing tags — start fresh
		tags = make(map[string]string)
	}

	if value == "" {
		delete(tags, attr)
	} else {
		tags[attr] = value
	}

	if err := client.PutObjectTagging(ctx, key, tags); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handlePath(w http.ResponseWriter, r *http.Request) {
	// Return the expected cache path without downloading. PVE calls path() for
	// many operations (delete, config, template) that don't need the file on disk.
	// The actual download happens in activate_volume via /v1/download.
	storageID := r.URL.Query().Get("storage")
	key := r.URL.Query().Get("key")

	if _, ok := s.getClient(storageID); !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]string{"path": s.cache.ExpectedPath(storageID, key)})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"cache_dir": s.cfg.CacheDir})
}

// mergeLocalFiles scans a local cache directory and adds any files that
// exist locally but are not yet in S3 (pending watcher upload) to the
// volume list. This ensures files written directly by PVE (e.g. via
// Terraform or the upload API) appear immediately in list_volumes.
func (s *Server) mergeLocalFiles(volumes *[]VolumeInfo, localDir, prefix, content, storageID string, s3Keys map[string]bool) {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return // directory may not exist yet
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if content == "images" {
				if entry.Name() == ".meta" {
					continue
				}
				// Recurse into VM ID subdirectories for images
				subDir := filepath.Join(localDir, entry.Name())
				subEntries, err := os.ReadDir(subDir)
				if err != nil {
					continue
				}
				for _, subEntry := range subEntries {
					if subEntry.IsDir() {
						continue
					}
					key := prefix + entry.Name() + "/" + subEntry.Name()
					if s3Keys[key] {
						continue
					}
					info, err := subEntry.Info()
					if err != nil {
						continue
					}
					volname := entry.Name() + "/" + subEntry.Name()
					*volumes = append(*volumes, VolumeInfo{
						Volume:  fmt.Sprintf("%s:%s", storageID, volname),
						Key:     key,
						Size:    info.Size(),
						Format:  detectFormat(key),
						Content: content,
					})
				}
			}
			continue
		}
		// Skip temp and meta files
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".meta") {
			continue
		}
		key := prefix + name
		if s3Keys[key] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		volname := content + "/" + name
		*volumes = append(*volumes, VolumeInfo{
			Volume:  fmt.Sprintf("%s:%s", storageID, volname),
			Key:     key,
			Size:    info.Size(),
			Format:  detectFormat(key),
			Content: content,
		})
	}
}

// --- Helpers ---

func clearImmutable(path string) {
	exec.Command("chattr", "-i", path).Run()
}

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
	case "import":
		return "import/"
	case "images":
		return "images/"
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
