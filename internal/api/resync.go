package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sol1/proxs3/internal/cache"
)

// handleResync walks the local cache for the given storage and uploads any
// files that are missing from S3 or where the local copy is newer than S3.
// Streams a plain-text progress log to the client so `proxs3d --resync` can
// surface progress at the shell.
//
// Conflict handling:
//   - missing in S3                       -> upload
//   - size matches S3                     -> skip, write meta if absent
//   - size mismatch, local mtime newer    -> upload (warn)
//   - size mismatch, S3 LastModified newer -> skip unless force=1 (warn)
func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	storageID := r.URL.Query().Get("storage")
	force := r.URL.Query().Get("force") == "1"

	if storageID == "" {
		http.Error(w, "missing storage parameter", http.StatusBadRequest)
		return
	}
	client, ok := s.getClient(storageID)
	if !ok {
		http.Error(w, "unknown storage", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
		if flusher != nil {
			flusher.Flush()
		}
	}

	storageDir := filepath.Join(s.cfg.CacheDir, storageID)
	if _, err := os.Stat(storageDir); err != nil {
		emit("resync: cache dir %s does not exist — nothing to do", storageDir)
		return
	}

	emit("resync: scanning %s (force-latest=%v)", storageDir, force)
	log.Printf("resync: starting for %s (force-latest=%v)", storageID, force)

	var uploaded, skipped, conflicts, errs int

	walkErr := filepath.Walk(storageDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			emit("walk error at %s: %v", path, walkErr)
			errs++
			return nil
		}
		if info.IsDir() {
			// Skip metadata sidecar tree entirely
			if info.Name() == ".meta" {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".meta") {
			return nil
		}
		if fileInUse(path) {
			emit("skip (file open by another process): %s", path)
			skipped++
			return nil
		}

		rel, err := filepath.Rel(storageDir, path)
		if err != nil {
			return nil
		}
		s3Key := filepath.ToSlash(rel)

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		head, headErr := client.HeadObject(ctx, s3Key)
		cancel()

		var reason string
		shouldUpload := false

		switch {
		case headErr != nil:
			shouldUpload = true
			reason = "missing in S3"
		case info.Size() == head.Size:
			// Sizes match — assume in sync. Backfill metadata so the
			// watcher's same-size short-circuit works on future events.
			if s.cache.GetMeta(storageID, s3Key) == nil {
				s.cache.StoreMeta(storageID, s3Key, cache.FileMeta{
					ETag:         head.ETag,
					LastModified: head.LastModified,
					Size:         head.Size,
				})
			}
			return nil
		case info.ModTime().After(head.LastModified):
			shouldUpload = true
			reason = fmt.Sprintf("size mismatch, local newer (local=%d s3=%d)", info.Size(), head.Size)
			emit("WARNING: %s — %s — uploading local copy", s3Key, reason)
			log.Printf("resync warning: %s/%s — %s — uploading local copy", storageID, s3Key, reason)
		case force:
			shouldUpload = true
			reason = fmt.Sprintf("size mismatch, S3 newer but --force-latest set (local=%d s3=%d)", info.Size(), head.Size)
			emit("WARNING: %s — %s — overwriting", s3Key, reason)
			log.Printf("resync warning: %s/%s — %s — overwriting", storageID, s3Key, reason)
		default:
			emit("CONFLICT (skipped): %s — size mismatch, S3 newer (local=%d s3=%d) — pass --force-latest to overwrite",
				s3Key, info.Size(), head.Size)
			log.Printf("resync conflict: %s/%s — S3 newer than local — skipped", storageID, s3Key)
			conflicts++
			return nil
		}

		if !shouldUpload {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			emit("error opening %s: %v", path, err)
			errs++
			return nil
		}
		size := info.Size()
		emit("upload: %s (%.1f MB) — %s", s3Key, float64(size)/(1024*1024), reason)

		upCtx, upCancel := context.WithTimeout(r.Context(), 60*time.Minute)
		err = client.PutObject(upCtx, s3Key, f, size)
		upCancel()
		f.Close()

		if err != nil {
			emit("error uploading %s: %v", s3Key, err)
			log.Printf("resync upload failed for %s/%s: %v", storageID, s3Key, err)
			errs++
			return nil
		}

		// Record the ETag S3 assigned so the staleness check recognizes the
		// cached copy as current instead of re-downloading it on first access.
		syncedMeta := cache.FileMeta{
			Size:         size,
			LastModified: time.Now(),
		}
		hCtx, hCancel := context.WithTimeout(r.Context(), 10*time.Second)
		if head, err := client.HeadObject(hCtx, s3Key); err == nil {
			syncedMeta.ETag = head.ETag
			syncedMeta.LastModified = head.LastModified
		}
		hCancel()
		s.cache.StoreMeta(storageID, s3Key, syncedMeta)
		uploaded++
		return nil
	})

	if walkErr != nil {
		emit("walk failed: %v", walkErr)
	}

	summary := fmt.Sprintf("resync complete: uploaded=%d skipped=%d conflicts=%d errors=%d",
		uploaded, skipped, conflicts, errs)
	emit("%s", summary)
	log.Printf("resync %s: %s", storageID, summary)
}
