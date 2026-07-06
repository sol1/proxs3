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

		// Files written while the daemon wasn't running have no fsnotify
		// event; queue anything on disk that isn't confirmed uploaded so
		// pending uploads survive restarts.
		s.scanPendingUploads(pending)

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
					continue
				}
				if s.consumeSelfWrite(event.Name) {
					// Our own download rename or upload link — already synced.
					continue
				}
				if _, seen := pending[event.Name]; !seen {
					// New pending episode: durably mark the local file as
					// authoritative before anything can act on the old state.
					s.markPendingUpload(event.Name)
				}
				pending[event.Name] = time.Now()

			case path := <-s.uploadRequests:
				// Upload requested outside fsnotify (e.g. handleDownload
				// served a pending file). Runs through the same debounce
				// and in-use checks as watcher-detected writes.
				if _, seen := pending[path]; !seen {
					pending[path] = time.Now()
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

// cacheRelKey maps a path inside the cache dir to its storage ID and S3 key.
// Parses: /var/cache/proxs3/<storageID>/<prefix>/<filename>
func (s *Server) cacheRelKey(localPath string) (storageID, s3Key string, ok bool) {
	rel, err := filepath.Rel(s.cfg.CacheDir, localPath)
	if err != nil {
		return "", "", false
	}
	// rel is like "s3test/template/iso/debian.iso"
	parts := strings.SplitN(rel, string(os.PathSeparator), 2)
	if len(parts) != 2 {
		return "", "", false
	}
	// Normalize path separators to forward slashes for S3
	return parts[0], filepath.ToSlash(parts[1]), true
}

// consumeSelfWrite reports whether the daemon itself just created this path
// (download rename, upload link). Entries expire so a stale one can't
// swallow a later genuine PVE write event for the same path.
func (s *Server) consumeSelfWrite(path string) bool {
	v, ok := s.selfWrites.LoadAndDelete(path)
	if !ok {
		return false
	}
	return time.Since(v.(time.Time)) < 10*time.Second
}

// markPendingUpload durably marks a file PVE just wrote into the cache dir,
// so the local copy is treated as authoritative — by handleDownload, by
// eviction, and across daemon restarts — until the upload completes.
func (s *Server) markPendingUpload(localPath string) {
	if strings.HasSuffix(localPath, ".tmp") || strings.HasSuffix(localPath, ".meta") {
		return
	}
	info, err := os.Stat(localPath)
	if err != nil || info.IsDir() {
		return
	}
	storageID, s3Key, ok := s.cacheRelKey(localPath)
	if !ok {
		return
	}
	s.cache.StoreMeta(storageID, s3Key, cache.FileMeta{
		Size:          info.Size(),
		LastModified:  info.ModTime(),
		PendingUpload: true,
	})
}

// scanPendingUploads walks the storage cache trees and queues files that are
// not confirmed uploaded to S3: no metadata (written locally, never
// processed) or an explicit pending-upload marker. fsnotify alone can't see
// files written while the daemon wasn't running.
func (s *Server) scanPendingUploads(pending map[string]time.Time) {
	s.clientMu.RLock()
	storages := make([]string, 0, len(s.clients))
	for id := range s.clients {
		storages = append(storages, id)
	}
	s.clientMu.RUnlock()

	for _, storageID := range storages {
		baseDir := filepath.Join(s.cfg.CacheDir, storageID)
		filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".tmp") || strings.HasSuffix(path, ".meta") {
				return nil
			}
			rel, err := filepath.Rel(baseDir, path)
			if err != nil {
				return nil
			}
			s3Key := filepath.ToSlash(rel)
			if meta := s.cache.GetMeta(storageID, s3Key); meta == nil || meta.PendingUpload {
				log.Printf("watcher: %s/%s not confirmed on S3, queueing upload", storageID, s3Key)
				pending[path] = time.Now()
			}
			return nil
		})
	}
}

// requestUpload queues a file for upload via the watcher's debounce loop, so
// it gets the same stability window and in-use check as fsnotify-detected
// writes. Non-blocking: if the queue is full (or the watcher isn't running),
// the file is still safe — it stays marked pending and the restart scan or
// the next write event picks it up.
func (s *Server) requestUpload(localPath string) {
	select {
	case s.uploadRequests <- localPath:
	default:
		log.Printf("watcher: upload queue full, dropping request for %s", localPath)
	}
}

// cancelUpload aborts any in-flight background upload for localPath and
// suppresses a queued rerun. Used by delete so a PutObject completing after
// the DeleteObject can't resurrect a just-deleted object.
func (s *Server) cancelUpload(localPath string) {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	if st := s.uploads[localPath]; st != nil {
		st.canceled = true
		st.rerun = false
		if st.cancel != nil {
			st.cancel()
		}
	}
}

// uploadNewFile uploads a local cache file to S3, deduplicating concurrent
// triggers. A trigger that arrives while an upload is already running (e.g.
// the file was rewritten mid-transfer) schedules a rerun after it finishes
// instead of being dropped.
func (s *Server) uploadNewFile(localPath string) {
	// Skip .meta sidecar files — these are cache metadata, not real content
	if strings.HasSuffix(localPath, ".meta") {
		return
	}

	s.uploadsMu.Lock()
	if st := s.uploads[localPath]; st != nil {
		st.rerun = true
		s.uploadsMu.Unlock()
		return
	}
	st := &uploadState{}
	s.uploads[localPath] = st
	s.uploadsMu.Unlock()

	for {
		s.uploadOnce(localPath, st)

		s.uploadsMu.Lock()
		if st.rerun && !st.canceled {
			st.rerun = false
			s.uploadsMu.Unlock()
			continue
		}
		delete(s.uploads, localPath)
		s.uploadsMu.Unlock()
		return
	}
}

// uploadOnce performs a single upload attempt for uploadNewFile.
func (s *Server) uploadOnce(localPath string, st *uploadState) {
	storageID, s3Key, ok := s.cacheRelKey(localPath)
	if !ok {
		return
	}

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
	// Files written by handleDownload (from S3) or handleUpload (already
	// pushed) have non-pending metadata with matching size.
	if meta := s.cache.GetMeta(storageID, s3Key); meta != nil && !meta.PendingUpload && meta.Size == info.Size() {
		log.Printf("watcher: skipping %s in %s (already synced to S3)", s3Key, storageID)
		return
	}

	log.Printf("watcher: uploading %s to s3://%s (%.1f MB)",
		s3Key, storageID, float64(info.Size())/(1024*1024))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s.uploadsMu.Lock()
	if st.canceled {
		s.uploadsMu.Unlock()
		return
	}
	st.cancel = cancel
	s.uploadsMu.Unlock()

	if err := client.PutObject(ctx, s3Key, f, info.Size()); err != nil {
		log.Printf("watcher: upload failed for %s: %v", s3Key, err)
		return
	}

	s.uploadsMu.Lock()
	canceled := st.canceled
	s.uploadsMu.Unlock()
	if canceled {
		// Deleted while the last bytes were in flight — don't write metadata
		// for a file delete is about to remove.
		return
	}

	// Update cache metadata; record the ETag S3 assigned so the staleness
	// check recognizes the cached copy as current instead of re-downloading.
	meta := cache.FileMeta{
		Size:         info.Size(),
		LastModified: time.Now(),
	}
	if head, err := client.HeadObject(ctx, s3Key); err == nil {
		meta.ETag = head.ETag
		meta.LastModified = head.LastModified
	}
	s.cache.StoreMeta(storageID, s3Key, meta)

	log.Printf("watcher: uploaded %s to %s successfully", s3Key, storageID)
}
