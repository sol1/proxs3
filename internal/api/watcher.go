package api

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sol1/proxs3/internal/cache"
)

// prefixToContent maps S3 key prefixes back to content types.
var prefixDirs = map[string]string{
	"template/iso":   "template/iso/",
	"template/cache": "template/cache/",
	"snippets":       "snippets/",
	"dump":           "dump/",
	"import":         "import/",
	"images":         "images/",
}

// watchCacheDirs watches the cache subdirectories for all configured storages.
// When PVE uploads a file to the local cache path, this detects it and
// uploads to S3 in the background.
func (s *Server) watchCacheDirs() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Warning: could not start file watcher: %v", err)
		return
	}

	go func() {
		defer watcher.Close()

		// Debounce: track files we've seen recently to avoid uploading
		// partial writes. We wait for the file to be stable.
		pending := make(map[string]time.Time)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
					pending[event.Name] = time.Now()
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("watcher error: %v", err)

			case <-ticker.C:
				// Process files that have been stable for 3+ seconds
				now := time.Now()
				for path, seen := range pending {
					if now.Sub(seen) < 3*time.Second {
						continue
					}

					// Check file still exists and is a regular file
					info, err := os.Stat(path)
					if err != nil || info.IsDir() {
						delete(pending, path)
						continue
					}

					// Skip .tmp files (vzdump writes to .tmp then renames)
					if strings.HasSuffix(path, ".tmp") {
						continue
					}

					// Check no process has the file open (prevents uploading partial writes)
					if fileInUse(path) {
						pending[path] = now // reset timer, check again later
						continue
					}

					delete(pending, path)
					go s.uploadNewFile(path)
				}
			}
		}
	}()

	// Add watches for all storage cache subdirs
	s.addWatchDirs(watcher)

	// Re-add watches when storages change (after reload)
	go func() {
		for {
			time.Sleep(10 * time.Second)
			s.addWatchDirs(watcher)
		}
	}()
}

func (s *Server) addWatchDirs(watcher *fsnotify.Watcher) {
	s.clientMu.RLock()
	clients := s.clients
	s.clientMu.RUnlock()

	for storageID := range clients {
		baseDir := filepath.Join(s.cfg.CacheDir, storageID)
		for subDir := range prefixDirs {
			dir := filepath.Join(baseDir, subDir)
			if _, err := os.Stat(dir); err == nil {
				_ = watcher.Add(dir)
				// Also watch immediate subdirectories (e.g., images/9001/)
				// fsnotify doesn't recurse, and images uses vmid subdirs
				entries, err := os.ReadDir(dir)
				if err == nil {
					for _, e := range entries {
						if e.IsDir() {
							_ = watcher.Add(filepath.Join(dir, e.Name()))
						}
					}
				}
			}
		}
	}
}

// fileInUse checks if any process has the file open (via /proc).
// Returns true if the file is still being written to.
func fileInUse(path string) bool {
	// Read /proc/*/fd to find open file descriptors pointing to this path
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only check numeric dirs (PIDs)
		pid := entry.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		fdDir := filepath.Join("/proc", pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && link == path {
				return true
			}
		}
	}
	return false
}

// uploadNewFile detects the storage ID and S3 key from a local cache path
// and uploads the file to S3.
func (s *Server) uploadNewFile(localPath string) {
	// Skip .meta sidecar files — these are cache metadata, not real content
	if strings.HasSuffix(localPath, ".meta") {
		return
	}

	// Parse: /var/cache/proxs3/<storageID>/<prefix>/<filename>
	rel, err := filepath.Rel(s.cfg.CacheDir, localPath)
	if err != nil {
		log.Printf("watcher: can't determine relative path for %s: %v", localPath, err)
		return
	}

	// rel is like "s3test/template/iso/debian.iso"
	parts := strings.SplitN(rel, string(os.PathSeparator), 2)
	if len(parts) != 2 {
		return
	}
	storageID := parts[0]
	s3Key := parts[1]

	// Normalize path separators to forward slashes for S3
	s3Key = filepath.ToSlash(s3Key)

	client, ok := s.getClient(storageID)
	if !ok {
		log.Printf("watcher: unknown storage %s for file %s", storageID, localPath)
		return
	}

	f, err := os.Open(localPath)
	if err != nil {
		log.Printf("watcher: can't open %s: %v", localPath, err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("watcher: can't stat %s: %v", localPath, err)
		return
	}

	// Skip if cache metadata shows this file is already in sync with S3.
	// Files written by handleDownload (from S3) or handleUpload (already pushed)
	// have metadata with matching size — no need to re-upload.
	if meta := s.cache.GetMeta(storageID, s3Key); meta != nil && meta.Size == info.Size() {
		log.Printf("watcher: skipping %s in %s (already synced to S3)", s3Key, storageID)
		return
	}

	log.Printf("watcher: uploading %s to s3://%s (%.1f MB)",
		s3Key, storageID, float64(info.Size())/(1024*1024))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := client.PutObject(ctx, s3Key, f, info.Size()); err != nil {
		log.Printf("watcher: upload failed for %s: %v", s3Key, err)
		return
	}

	// Update cache metadata
	meta := cache.FileMeta{
		Size:         info.Size(),
		LastModified: time.Now(),
	}
	s.cache.StoreMeta(storageID, s3Key, meta)

	log.Printf("watcher: uploaded %s to %s successfully", s3Key, storageID)
}
