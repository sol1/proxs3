package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileMeta tracks the S3 object metadata for a cached file,
// so we can detect when the remote object has changed.
type FileMeta struct {
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified"`
	Size         int64     `json:"size"`
	CachedAt     time.Time `json:"cached_at"`
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

// GetMeta returns the stored metadata for a cached object, or nil if not cached.
func (fc *FileCache) GetMeta(storageID, key string) *FileMeta {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

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
	os.Remove(p)
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

	// Write metadata to .meta/ subdirectory
	meta.CachedAt = time.Now()
	metaData, _ := json.Marshal(meta)
	mp := fc.metaPath(storageID, key)
	os.MkdirAll(filepath.Dir(mp), 0755)
	os.WriteFile(mp, metaData, 0644)

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

	// Write metadata to .meta/ subdirectory
	meta.CachedAt = time.Now()
	metaData, _ := json.Marshal(meta)
	mp := fc.metaPath(storageID, key)
	os.MkdirAll(filepath.Dir(mp), 0755)
	os.WriteFile(mp, metaData, 0644)
}

// Remove deletes a cached file and its metadata.
func (fc *FileCache) Remove(storageID, key string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := fc.path(storageID, key)
	os.Remove(fc.metaPath(storageID, key))
	os.Remove(p + ".meta") // clean up legacy location
	return os.Remove(p)
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

func (fc *FileCache) path(storageID, key string) string {
	return filepath.Join(fc.baseDir, storageID, key)
}

// metaPath returns the path for the metadata sidecar file,
// stored in a .meta/ subdirectory to avoid polluting content directories.
func (fc *FileCache) metaPath(storageID, key string) string {
	return filepath.Join(fc.baseDir, ".meta", storageID, key+".json")
}

type cachedFile struct {
	path    string
	size    int64
	modTime int64
}

// evictIfNeeded removes oldest files until cache is under the size limit.
func (fc *FileCache) evictIfNeeded() {
	if fc.maxMB <= 0 {
		return
	}

	var files []cachedFile
	var totalSize int64

	filepath.Walk(fc.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Skip the .meta directory entirely
		if strings.Contains(path, string(os.PathSeparator)+".meta"+string(os.PathSeparator)) {
			return nil
		}
		// Skip legacy .meta sidecar files
		if filepath.Ext(path) == ".meta" {
			return nil
		}
		files = append(files, cachedFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime().Unix(),
		})
		totalSize += info.Size()
		return nil
	})

	maxBytes := fc.maxMB * 1024 * 1024
	if totalSize <= maxBytes {
		return
	}

	// Sort oldest first
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
			continue
		}
		os.Remove(f.path + ".meta") // legacy cleanup
		// Clean up .meta/ directory entry
		if rel, err := filepath.Rel(fc.baseDir, f.path); err == nil {
			os.Remove(filepath.Join(fc.baseDir, ".meta", rel+".json"))
		}
		totalSize -= f.size
		log.Printf("cache evict: %s (%d MB remaining)", f.path, totalSize/(1024*1024))
	}
}
