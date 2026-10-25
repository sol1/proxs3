package api

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sol1/proxs3/internal/cache"
	"github.com/sol1/proxs3/internal/s3client"
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
				if _, seen := pending[event.Name]; !seen {
					// New pending episode: durably mark the local file as
					// authoritative before anything can act on the old state.
					// The daemon's own writes (download, upload link, copy,
					// rename) are already covered by in-sync metadata and
					// don't start an episode.
					if !s.markPendingUpload(event.Name) {
						continue
					}
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

					// Check no process has the file open (prevents uploading
					// partial writes), and that delete/rename isn't working on it
					if s.uploadBlocked(path) || fileInUse(path) {
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

// markPendingUpload durably marks a file PVE just wrote into the cache dir,
// so the local copy is treated as authoritative — by handleDownload, by
// eviction, and across daemon restarts — until the upload completes. Returns
// false if there's nothing to upload: not a content file, gone, or the write
// was the daemon's own and is already recorded as in sync.
func (s *Server) markPendingUpload(localPath string) bool {
	if strings.HasSuffix(localPath, ".tmp") || strings.HasSuffix(localPath, ".meta") {
		return false
	}
	storageID, s3Key, ok := s.cacheRelKey(localPath)
	if !ok {
		return false
	}
	return s.cache.MarkPending(storageID, s3Key)
}

// scanPendingUploads walks the storage cache trees and queues files that are
// not confirmed uploaded to S3: an explicit pending-upload marker, modified
// since their metadata was recorded, or no metadata at all (uploadOnce
// reconciles those against S3 rather than blindly uploading). fsnotify alone
// can't see files written while the daemon wasn't running.
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
			if s.cache.GetMeta(storageID, s3Key) == nil || s.cache.MarkPending(storageID, s3Key) {
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

// blockUploads stops background uploads of localPath while delete or rename
// works on it: any in-flight upload is canceled and waited for, so its
// PutObject can't land after the caller's DeleteObject and resurrect the
// object, and no new upload starts until the returned func is called. Writes
// seen meanwhile stay queued in the watcher and are handled after unblocking.
func (s *Server) blockUploads(localPath string) (unblock func()) {
	s.uploadsMu.Lock()
	s.blocked[localPath]++
	st := s.uploads[localPath]
	if st != nil {
		st.canceled = true
		st.rerun = false
		if st.cancel != nil {
			st.cancel()
		}
	}
	s.uploadsMu.Unlock()

	if st != nil {
		<-st.done
	}
	return func() {
		s.uploadsMu.Lock()
		if s.blocked[localPath]--; s.blocked[localPath] <= 0 {
			delete(s.blocked, localPath)
		}
		s.uploadsMu.Unlock()
	}
}

func (s *Server) uploadBlocked(localPath string) bool {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	return s.blocked[localPath] > 0
}

// uploadNewFile uploads a local cache file to S3, deduplicating concurrent
// triggers. A trigger that arrives while an upload is already running (e.g.
// the file was rewritten mid-transfer) schedules a rerun after it finishes
// instead of being dropped. A failed upload is retried with backoff.
func (s *Server) uploadNewFile(localPath string) {
	// Skip .meta sidecar files — these are cache metadata, not real content
	if strings.HasSuffix(localPath, ".meta") {
		return
	}

	s.uploadsMu.Lock()
	if s.blocked[localPath] > 0 {
		s.uploadsMu.Unlock()
		return
	}
	if st := s.uploads[localPath]; st != nil {
		st.rerun = true
		s.uploadsMu.Unlock()
		return
	}
	st := &uploadState{done: make(chan struct{})}
	s.uploads[localPath] = st
	s.uploadsMu.Unlock()

	for {
		retry := s.uploadOnce(localPath, st)

		s.uploadsMu.Lock()
		if st.rerun && !st.canceled {
			st.rerun = false
			s.uploadsMu.Unlock()
			continue
		}
		delete(s.uploads, localPath)
		if retry && !st.canceled {
			s.scheduleRetryLocked(localPath)
		} else {
			delete(s.retries, localPath)
		}
		close(st.done)
		s.uploadsMu.Unlock()
		return
	}
}

// scheduleRetryLocked re-queues a failed upload with exponential backoff
// (base, 2x, 4x, ... capped at 30m). The file stays marked pending, so it is
// safe from purge and eviction meanwhile. Caller holds uploadsMu.
func (s *Server) scheduleRetryLocked(localPath string) {
	attempt := s.retries[localPath]
	s.retries[localPath] = attempt + 1
	delay := s.retryBase
	for i := 0; i < attempt && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	log.Printf("watcher: retrying upload of %s in %s", localPath, delay)
	time.AfterFunc(delay, func() { s.requestUpload(localPath) })
}

// uploadOnce performs a single upload attempt for uploadNewFile. Returns true
// if the attempt failed in a way worth retrying.
func (s *Server) uploadOnce(localPath string, st *uploadState) (retry bool) {
	storageID, s3Key, ok := s.cacheRelKey(localPath)
	if !ok {
		return false
	}

	client, ok := s.getClient(storageID)
	if !ok {
		log.Printf("watcher: unknown storage %s for file %s", storageID, localPath)
		return false
	}

	started := time.Now()
	f, err := os.Open(localPath)
	if err != nil {
		log.Printf("watcher: can't open %s: %v", localPath, err)
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("watcher: can't stat %s: %v", localPath, err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s.uploadsMu.Lock()
	if st.canceled {
		s.uploadsMu.Unlock()
		return false
	}
	st.cancel = cancel
	s.uploadsMu.Unlock()

	meta := s.cache.GetMeta(storageID, s3Key)
	switch {
	case meta == nil:
		// No metadata: written while the daemon was down, or left by an
		// older version. Don't assume local wins — check S3 first, the
		// same way --resync does, so we don't push a stale copy over a
		// newer object another node uploaded.
		headCtx, headCancel := context.WithTimeout(ctx, 60*time.Second)
		head, headErr := client.HeadObject(headCtx, s3Key)
		headCancel()
		switch reconcileLocal(info, head, headErr) {
		case localUnknown:
			log.Printf("watcher: can't check %s in %s on S3: %v", s3Key, storageID, headErr)
			return true
		case localInSync:
			s.cache.MarkSynced(storageID, s3Key, info, started, cache.FileMeta{
				ETag: head.ETag, LastModified: head.LastModified, Size: head.Size,
			})
			log.Printf("watcher: %s in %s already on S3, recorded as synced", s3Key, storageID)
			return false
		case localOlder:
			// S3 has a newer, different object. Record the local copy
			// without an ETag so the next access re-downloads it.
			s.cache.MarkSynced(storageID, s3Key, info, started, cache.FileMeta{
				LastModified: info.ModTime(), Size: info.Size(),
			})
			log.Printf("watcher: %s in %s is newer on S3 (local=%d s3=%d), not uploading local copy",
				s3Key, storageID, info.Size(), head.Size)
			return false
		case localNewer:
			log.Printf("watcher: %s in %s differs from S3 and local is newer (local=%d s3=%d), uploading",
				s3Key, storageID, info.Size(), head.Size)
		}
		// Local wins: make that durable before uploading. Our own mark
		// mustn't count as a rewrite against this upload.
		s.cache.MarkPending(storageID, s3Key)
		started = time.Now()
	case !meta.PendingUpload && meta.Size == info.Size() && !info.ModTime().After(meta.CachedAt):
		// Metadata recorded after the last write says it's in sync:
		// written by handleDownload (from S3) or already uploaded.
		log.Printf("watcher: skipping %s in %s (already synced to S3)", s3Key, storageID)
		return false
	}

	log.Printf("watcher: uploading %s to s3://%s (%.1f MB)",
		s3Key, storageID, float64(info.Size())/(1024*1024))

	etag, err := client.PutObject(ctx, s3Key, f, info.Size())
	if err != nil {
		log.Printf("watcher: upload failed for %s: %v", s3Key, err)
		return true
	}

	s.uploadsMu.Lock()
	canceled := st.canceled
	s.uploadsMu.Unlock()
	if canceled {
		// Deleted while the last bytes were in flight — don't write metadata
		// for a file delete is about to remove.
		return false
	}

	// Record the ETag S3 assigned so the staleness check recognizes the
	// cached copy as current instead of re-downloading. Only if the file is
	// still what we uploaded: a rewrite mid-upload must stay pending.
	if !s.cache.MarkSynced(storageID, s3Key, info, started, cache.FileMeta{
		ETag: etag, LastModified: time.Now(), Size: info.Size(),
	}) {
		// Normally already marked by the write event; make sure.
		s.cache.MarkPending(storageID, s3Key)
		log.Printf("watcher: %s in %s changed during upload, newer content still pending", s3Key, storageID)
		return false
	}

	log.Printf("watcher: uploaded %s to %s successfully", s3Key, storageID)
	return false
}

// localState is how a cached file with no metadata relates to S3.
type localState int

const (
	localUnknown localState = iota // S3 couldn't be checked
	localMissing                   // not on S3: local is the only copy
	localInSync                    // same size on S3: assume same content
	localNewer                     // differs, local modified after the S3 object
	localOlder                     // differs, S3 object is newer
)

// reconcileLocal classifies a metadata-less cached file against its S3 HEAD,
// using the same rules as --resync.
func reconcileLocal(info os.FileInfo, head *s3client.ObjectInfo, headErr error) localState {
	switch {
	case errors.Is(headErr, s3client.ErrNotFound):
		return localMissing
	case headErr != nil:
		return localUnknown
	case info.Size() == head.Size:
		return localInSync
	case info.ModTime().After(head.LastModified):
		return localNewer
	default:
		return localOlder
	}
}
