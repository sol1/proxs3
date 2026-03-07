package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileCache provides a local filesystem cache for S3 objects.
// Used primarily for ISOs and templates which are read-heavy.
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
	p := fc.path(storageID, key)
	_, err := os.Stat(p)
	return err == nil
}

// Path returns the local filesystem path for a cached object.
// Returns empty string if not cached.
func (fc *FileCache) Path(storageID, key string) string {
	p := fc.path(storageID, key)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Store writes data from a reader into the cache, returning the local path.
func (fc *FileCache) Store(storageID, key string, r io.Reader) (string, error) {
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
	return p, nil
}

// Remove deletes a cached file.
func (fc *FileCache) Remove(storageID, key string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return os.Remove(fc.path(storageID, key))
}

func (fc *FileCache) path(storageID, key string) string {
	return filepath.Join(fc.baseDir, storageID, key)
}
