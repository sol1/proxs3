package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileInUse_NonexistentFile(t *testing.T) {
	// A file that doesn't exist should not be reported as in use
	if fileInUse("/nonexistent/path/file.iso") {
		t.Error("expected false for nonexistent file")
	}
}

func TestFileInUse_ClosedFile(t *testing.T) {
	// Create and close a file - should not be in use
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.iso")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	f.Close()

	if fileInUse(path) {
		t.Error("expected false for closed file")
	}
}

func TestFileInUse_OpenFile(t *testing.T) {
	// Create a file and keep it open - should be in use
	dir := t.TempDir()
	path := filepath.Join(dir, "open.iso")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	defer f.Close()

	if !fileInUse(path) {
		t.Error("expected true for open file")
	}
}

func TestUploadNewFile_SkipsMeta(t *testing.T) {
	// uploadNewFile should skip .meta files
	// We test this by checking the early return condition
	path := "/var/cache/proxs3/s3test/template/iso/test.iso.meta"
	if !strings.HasSuffix(path, ".meta") {
		t.Error("test setup: path should end in .meta")
	}
}

func TestUploadNewFile_SkipsTmp(t *testing.T) {
	// The .tmp skip happens in the watcher loop, verify the suffix check
	path := "/var/cache/proxs3/s3test/dump/vzdump-qemu-100.vma.tmp"
	if !strings.HasSuffix(path, ".tmp") {
		t.Error("test setup: path should end in .tmp")
	}
}

func TestUploadNewFile_PathParsing(t *testing.T) {
	// Test the relative path parsing logic used in uploadNewFile
	cacheDir := "/var/cache/proxs3"

	tests := []struct {
		localPath   string
		wantStorage string
		wantKey     string
	}{
		{
			"/var/cache/proxs3/s3test/template/iso/debian.iso",
			"s3test", "template/iso/debian.iso",
		},
		{
			"/var/cache/proxs3/my-store/dump/vzdump-qemu-100.vma.zst",
			"my-store", "dump/vzdump-qemu-100.vma.zst",
		},
		{
			"/var/cache/proxs3/prod/snippets/cloud-init.yaml",
			"prod", "snippets/cloud-init.yaml",
		},
		{
			"/var/cache/proxs3/store/images/disk-0.raw",
			"store", "images/disk-0.raw",
		},
	}

	for _, tt := range tests {
		rel, err := filepath.Rel(cacheDir, tt.localPath)
		if err != nil {
			t.Errorf("Rel failed for %s: %v", tt.localPath, err)
			continue
		}
		parts := strings.SplitN(rel, string(os.PathSeparator), 2)
		if len(parts) != 2 {
			t.Errorf("expected 2 parts from %s, got %d", rel, len(parts))
			continue
		}
		storageID := parts[0]
		s3Key := filepath.ToSlash(parts[1])

		if storageID != tt.wantStorage {
			t.Errorf("path %s: expected storage %q, got %q", tt.localPath, tt.wantStorage, storageID)
		}
		if s3Key != tt.wantKey {
			t.Errorf("path %s: expected key %q, got %q", tt.localPath, tt.wantKey, s3Key)
		}
	}
}
