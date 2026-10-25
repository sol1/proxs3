package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// clearImmutable removes the immutable flag (chattr -i) from a file.
// PVE sets this on downloaded ISOs, templates, and base images.
func clearImmutable(path string) {
	if err := exec.Command("/usr/bin/chattr", "-i", path).Run(); err != nil {
		// Fallback to PATH lookup in case /usr/bin isn't the right location
		exec.Command("chattr", "-i", path).Run()
	}
}

// FileMeta tracks the S3 object metadata for a cached file,
// so we can detect when the remote object has changed.
type FileMeta struct {
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified"`
	Size         int64     `json:"size"`
	CachedAt     time.Time `json:"cached_at"`
	// PendingUpload marks a file whose local content is authoritative: it was
	// created or rewritten locally (by PVE) and has not been uploaded to S3
	// yet. Such files must never be purged or overwritten from S3. The zero
	// value means "in sync with S3", so metadata written by older versions
	// keeps its meaning.
	PendingUpload bool `json:"pending_upload,omitempty"`
}

// FileCache provides a local filesystem cache for S3 objects.
// Metadata is stored in a .meta/ subdirectory to keep content
// directories clean for the filesystem watcher.
type FileCache struct {
	baseDir string
	maxMB   int64
	mu      sync.RWMutex
}

// New creates a new file cache at the given directory.
func New(baseDir string, maxMB int64) (*FileCache, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("creating cache dir %s: %w", baseDir, err)
	}
	return &FileCache{
		baseDir: baseDir,
		maxMB:   maxMB,
	}, nil
}

// Has checks if a cached file exists for the given storage and key.
func (fc *FileCache) Has(storageID, key string) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	_, err := os.Stat(fc.path(storageID, key))
	return err == nil
}

// Path returns the local filesystem path for a cached object.
// Returns empty string if not cached.
func (fc *FileCache) Path(storageID, key string) string {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	p := fc.path(storageID, key)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// StoreMeta writes metadata for an already-cached file (e.g., after uploading to S3).
func (fc *FileCache) StoreMeta(storageID, key string, meta FileMeta) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.writeMeta(storageID, key, meta)
}

// writeMeta stamps CachedAt and writes the metadata file. Caller holds fc.mu.
// CachedAt doubles as the "recorded after the last write" marker MarkPending
// uses to tell the daemon's own writes from PVE's.
func (fc *FileCache) writeMeta(storageID, key string, meta FileMeta) {
	meta.CachedAt = time.Now()
	mp := fc.metaPath(storageID, key)
	if err := os.MkdirAll(filepath.Dir(mp), 0755); err != nil {
		return
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	os.WriteFile(mp, data, 0644)
}

// MarkPending records that the local file was written outside the daemon (by
// PVE) and is now authoritative until uploaded. Returns false, changing
// nothing, if the file is gone or its current state is already covered by
// in-sync metadata recorded after its last modification — i.e. the write was
// the daemon's own (download, upload link, copy, rename).
func (fc *FileCache) MarkPending(storageID, key string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	info, err := os.Stat(fc.path(storageID, key))
	if err != nil || info.IsDir() {
		return false
	}
	if meta := fc.getMeta(storageID, key); meta != nil && !meta.PendingUpload &&
		meta.Size == info.Size() && !info.ModTime().After(meta.CachedAt) {
		return false
	}
	fc.writeMeta(storageID, key, FileMeta{
		Size:          info.Size(),
		LastModified:  info.ModTime(),
		PendingUpload: true,
	})
	return true
}

// MarkSynced records meta for a file that was just uploaded (or found to
// match S3), but only if the file is still exactly what was uploaded: same
// inode, size and mtime, and not marked pending again since started. If it
// changed, the pending state is left alone so the newer content still gets
// uploaded. Atomic with MarkPending, so a rewrite can't slip in between the
// check and the write.
func (fc *FileCache) MarkSynced(storageID, key string, uploaded os.FileInfo, started time.Time, meta FileMeta) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	info, err := os.Stat(fc.path(storageID, key))
	if err != nil || !os.SameFile(info, uploaded) ||
		info.Size() != uploaded.Size() || !info.ModTime().Equal(uploaded.ModTime()) {
		return false
	}
	if cur := fc.getMeta(storageID, key); cur != nil && cur.PendingUpload && cur.CachedAt.After(started) {
		return false // a write was seen after we started, even if mtime didn't tick
	}
	fc.writeMeta(storageID, key, meta)
	return true
}

// GetMeta returns the stored metadata for a cached object, or nil if not cached.
func (fc *FileCache) GetMeta(storageID, key string) *FileMeta {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.getMeta(storageID, key)
}

// getMeta reads metadata. Caller holds fc.mu (read or write).
func (fc *FileCache) getMeta(storageID, key string) *FileMeta {
	mp := fc.metaPath(storageID, key)
	data, err := os.ReadFile(mp)
	if err != nil {
		// Fall back to legacy .meta sidecar location
		data, err = os.ReadFile(fc.path(storageID, key) + ".meta")
		if err != nil {
			return nil
		}
	}
	var meta FileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	return &meta
}

// IsStale returns true if the cached object doesn't match the remote metadata.
// If we have no cache or no meta, it's considered stale.
func (fc *FileCache) IsStale(storageID, key, remoteETag string, remoteModified time.Time) bool {
	meta := fc.GetMeta(storageID, key)
	if meta == nil {
		return true
	}
	// ETag is the strongest signal
	if remoteETag != "" {
		return meta.ETag != remoteETag
	}
	// No ETag available — fall back to LastModified comparison
	return remoteModified.After(meta.LastModified)
}

// Invalidate removes a cached file and its metadata.
func (fc *FileCache) Invalidate(storageID, key string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := fc.path(storageID, key)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return // keep meta: a file without it would look like a pending PVE write
	}
	os.Remove(fc.metaPath(storageID, key))
	os.Remove(p + ".meta") // clean up legacy location
}

// Store writes data from a reader into the cache with metadata, returning the local path.
func (fc *FileCache) Store(storageID, key string, r io.Reader, meta FileMeta) (string, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	p := fc.path(storageID, key)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return "", fmt.Errorf("creating cache subdir: %w", err)
	}

	f, err := os.Create(p)
	if err != nil {
		return "", fmt.Errorf("creating cache file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		os.Remove(p)
		return "", fmt.Errorf("writing cache file: %w", err)
	}

	f.Close() // flush before meta so CachedAt is after the last write
	fc.writeMeta(storageID, key, meta)

	// Evict if over limit (async, best-effort)
	go fc.evictIfNeeded()

	return p, nil
}

// Link creates a hard link or copy of an existing local file into the cache.
func (fc *FileCache) Link(storageID, key, srcPath string, meta FileMeta) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	p := fc.path(storageID, key)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return
	}

	// If srcPath is already the canonical cache path (same inode), there is
	// nothing to link or copy — the file is already in place. Skipping is
	// critical: the copy fallback below would os.Create(p) before reading
	// src, which truncates the file we're trying to register.
	sameInode := false
	if sa, err := os.Stat(srcPath); err == nil {
		if sb, err := os.Stat(p); err == nil && os.SameFile(sa, sb) {
			sameInode = true
		}
	}

	if !sameInode {
		// Try hard link first, fall back to copy
		if err := os.Link(srcPath, p); err != nil {
			src, err := os.Open(srcPath)
			if err != nil {
				return
			}
			defer src.Close()
			dst, err := os.Create(p)
			if err != nil {
				return
			}
			defer dst.Close()
			io.Copy(dst, src)
		}
	}

	fc.writeMeta(storageID, key, meta)
}

// Remove deletes a cached file and its metadata.
// Clears the immutable flag first — PVE sets chattr +i on ISOs, templates, and base images.
func (fc *FileCache) Remove(storageID, key string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := fc.path(storageID, key)
	clearImmutable(p)
	err := os.Remove(p)
	if err != nil && !os.IsNotExist(err) {
		// Retry after a second clearImmutable attempt — the flag may have
		// been re-set between our clear and remove (unlikely but defensive).
		clearImmutable(p)
		err = os.Remove(p)
	}
	if err != nil && !os.IsNotExist(err) {
		// Keep the metadata: a content file without it would look like a
		// pending PVE write and get re-uploaded, resurrecting the object.
		log.Printf("cache: failed to remove %s: %v", p, err)
		return err
	}
	os.Remove(fc.metaPath(storageID, key))
	os.Remove(p + ".meta") // clean up legacy location
	return err
}

// SizeMB returns the current total cache size in megabytes.
func (fc *FileCache) SizeMB() int64 {
	var total int64
	filepath.Walk(fc.baseDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total / (1024 * 1024)
}

// ExpectedPath returns where a cached file would be stored, without checking
// if it exists. Used by the path API to answer "where would this file be?"
func (fc *FileCache) ExpectedPath(storageID, key string) string {
	return fc.path(storageID, key)
}

func (fc *FileCache) path(storageID, key string) string {
	return filepath.Join(fc.baseDir, storageID, key)
}

// metaPath returns the path for the metadata sidecar file,
// stored in a .meta/ subdirectory to avoid polluting content directories.
func (fc *FileCache) metaPath(storageID, key string) string {
	return filepath.Join(fc.baseDir, ".meta", storageID, key+".json")
}

// EvictByAge removes cached files for a specific storage that are older than maxAge.
// Only content files are removed (not metadata-only entries).
func (fc *FileCache) EvictByAge(storageID string, maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}

	cutoff := time.Now().Add(-maxAge)
	storageDir := filepath.Join(fc.baseDir, storageID)
	removed := 0

	filepath.Walk(storageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			// Never evict a file that hasn't been uploaded to S3 — the local
			// copy is the only copy. No metadata means the same thing: the
			// file was written locally and not yet processed. Legacy .meta
			// sidecars are cleaned up alongside their content file as before.
			if !strings.HasSuffix(path, ".meta") {
				if rel, err := filepath.Rel(storageDir, path); err == nil {
					if meta := fc.GetMeta(storageID, filepath.ToSlash(rel)); meta == nil || meta.PendingUpload {
						return nil
					}
				}
			}
			fc.mu.Lock()
			err := os.Remove(path)
			if err != nil && !os.IsNotExist(err) {
				clearImmutable(path)
				err = os.Remove(path)
			}
			if err == nil || os.IsNotExist(err) {
				removed++
				// Clean up metadata
				if rel, err := filepath.Rel(fc.baseDir, path); err == nil {
					os.Remove(filepath.Join(fc.baseDir, ".meta", rel+".json"))
				}
				os.Remove(path + ".meta") // legacy cleanup
				log.Printf("cache age-evict: %s (older than %s)", path, maxAge)
			}
			fc.mu.Unlock()
		}
		return nil
	})
	return removed
}

type cachedFile struct {
	path    string
	size    int64
	modTime int64
}

// evictMinAge is how recently a file must have been modified to be exempt
// from size-based eviction. Without this protection, a single download
// larger than the cache size would self-evict the moment it completed,
// causing PVE's restore flow to loop indefinitely (download → evict →
// retry).
const evictMinAge = 30 * time.Minute

// evictIfNeeded removes oldest files until cache is under the size limit.
// Files modified within evictMinAge are protected — they're likely in
// active use (just downloaded for a restore, just uploaded by PVE, etc.).
func (fc *FileCache) evictIfNeeded() {
	if fc.maxMB <= 0 {
		return
	}

	maxBytes := fc.maxMB * 1024 * 1024
	youngCutoff := time.Now().Add(-evictMinAge)

	var files []cachedFile
	var totalSize, protectedSize int64

	filepath.Walk(fc.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+".meta"+string(os.PathSeparator)) {
			return nil
		}
		if filepath.Ext(path) == ".meta" {
			return nil
		}
		totalSize += info.Size()
		if info.ModTime().After(youngCutoff) {
			protectedSize += info.Size()
			return nil
		}
		// Never size-evict a file that hasn't been uploaded to S3 — the
		// local copy is the only copy. Partial downloads under .downloads/
		// stay evictable (they never have metadata by design).
		if rel, err := filepath.Rel(fc.baseDir, path); err == nil {
			parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
			if len(parts) == 2 && parts[0] != ".downloads" {
				if meta := fc.GetMeta(parts[0], parts[1]); meta == nil || meta.PendingUpload {
					protectedSize += info.Size()
					return nil
				}
			}
		}
		files = append(files, cachedFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime().Unix(),
		})
		return nil
	})

	if totalSize <= maxBytes {
		return
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime < files[j].modTime
	})

	fc.mu.Lock()
	defer fc.mu.Unlock()

	for _, f := range files {
		if totalSize <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			clearImmutable(f.path)
			if err := os.Remove(f.path); err != nil {
				continue
			}
		}
		os.Remove(f.path + ".meta")
		if rel, err := filepath.Rel(fc.baseDir, f.path); err == nil {
			os.Remove(filepath.Join(fc.baseDir, ".meta", rel+".json"))
		}
		totalSize -= f.size
		log.Printf("cache evict: %s (%d MB remaining)", f.path, totalSize/(1024*1024))
	}

	if totalSize > maxBytes {
		log.Printf("cache: %d MB used exceeds limit %d MB; %d MB protected as recently-modified — consider raising cache_max_mb",
			totalSize/(1024*1024), maxBytes/(1024*1024), protectedSize/(1024*1024))
	}
}
