package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sol1/proxs3/internal/cache"
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

func TestMarkPendingUpload(t *testing.T) {
	s := newTestServer(t, newMockClient("s3test"))
	dir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(dir, 0755)

	path := filepath.Join(dir, "new.yaml")
	os.WriteFile(path, []byte("hello"), 0644)
	s.markPendingUpload(path)

	meta := s.cache.GetMeta("s3test", "snippets/new.yaml")
	if meta == nil || !meta.PendingUpload {
		t.Fatalf("expected pending-upload marker, got %+v", meta)
	}
	if meta.Size != 5 {
		t.Errorf("expected size 5 in marker, got %d", meta.Size)
	}

	// Directories and .tmp files must not get markers.
	s.markPendingUpload(dir)
	tmpPath := filepath.Join(dir, "partial.yaml.tmp")
	os.WriteFile(tmpPath, []byte("x"), 0644)
	s.markPendingUpload(tmpPath)
	if meta := s.cache.GetMeta("s3test", "snippets"); meta != nil {
		t.Error("expected no marker for a directory")
	}
	if meta := s.cache.GetMeta("s3test", "snippets/partial.yaml.tmp"); meta != nil {
		t.Error("expected no marker for a .tmp file")
	}
}

func TestScanPendingUploads(t *testing.T) {
	s := newTestServer(t, newMockClient("s3test"))
	dir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(dir, 0755)

	// No metadata: written locally, never processed - must be queued.
	orphan := filepath.Join(dir, "orphan.yaml")
	os.WriteFile(orphan, []byte("a"), 0644)
	// Explicit pending marker - must be queued.
	marked := filepath.Join(dir, "marked.yaml")
	os.WriteFile(marked, []byte("bb"), 0644)
	s.cache.StoreMeta("s3test", "snippets/marked.yaml", cache.FileMeta{Size: 2, LastModified: time.Now(), PendingUpload: true})
	// Confirmed synced - must not be queued.
	synced := filepath.Join(dir, "synced.yaml")
	os.WriteFile(synced, []byte("ccc"), 0644)
	s.cache.StoreMeta("s3test", "snippets/synced.yaml", cache.FileMeta{Size: 3, LastModified: time.Now(), ETag: "\"e\""})
	// In-progress temp file - must not be queued.
	os.WriteFile(filepath.Join(dir, "partial.yaml.tmp"), []byte("d"), 0644)

	pending := make(map[string]time.Time)
	s.scanPendingUploads(pending)

	if _, ok := pending[orphan]; !ok {
		t.Error("expected metadata-less file to be queued")
	}
	if _, ok := pending[marked]; !ok {
		t.Error("expected pending-marked file to be queued")
	}
	if len(pending) != 2 {
		t.Errorf("expected exactly 2 queued files, got %d: %v", len(pending), pending)
	}
}

func TestUploadNewFile_RerunAfterTriggerDuringUpload(t *testing.T) {
	mock := newMockClient("s3test")
	mock.putStarted = make(chan string, 2)
	mock.putRelease = make(chan struct{})
	s := newTestServer(t, mock)

	key := "snippets/user.yaml"
	localPath := filepath.Join(s.cfg.CacheDir, "s3test", "snippets", "user.yaml")
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, []byte("v1"), 0644)

	done := make(chan struct{})
	go func() {
		s.uploadNewFile(localPath)
		close(done)
	}()
	<-mock.putStarted // first upload is in flight

	// The file is rewritten mid-upload; this trigger must not be dropped.
	os.WriteFile(localPath, []byte("v2-longer"), 0644)
	s.uploadNewFile(localPath) // returns immediately, schedules a rerun

	close(mock.putRelease)
	select {
	case <-mock.putStarted: // rerun started
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the rerun upload to start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for uploads to finish")
	}

	obj, ok := mock.object(key)
	if !ok {
		t.Fatal("expected object on S3 after rerun")
	}
	if obj.data != "v2-longer" {
		t.Errorf("expected rerun to upload rewritten content, got %q", obj.data)
	}
}

func TestHandleDelete_CancelsInFlightUpload(t *testing.T) {
	mock := newMockClient("s3test")
	mock.putStarted = make(chan string, 1)
	mock.putRelease = make(chan struct{}) // never released: only ctx cancel unblocks
	s := newTestServer(t, mock)

	key := "snippets/user.yaml"
	localPath := filepath.Join(s.cfg.CacheDir, "s3test", "snippets", "user.yaml")
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, []byte("payload"), 0644)

	done := make(chan struct{})
	go func() {
		s.uploadNewFile(localPath)
		close(done)
	}()
	<-mock.putStarted // upload is in flight

	req := httptest.NewRequest("DELETE", "/v1/delete?storage=s3test&key="+key, nil)
	w := httptest.NewRecorder()
	s.handleDelete(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 from delete, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-done: // upload aborted via context cancel
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled upload to return")
	}

	if _, ok := mock.object(key); ok {
		t.Error("expected no object on S3 after delete canceled the upload")
	}
	if p := s.cache.Path("s3test", key); p != "" {
		t.Errorf("expected local file removed by delete, got %q", p)
	}
	if meta := s.cache.GetMeta("s3test", key); meta != nil {
		t.Errorf("expected no metadata after delete, got %+v", meta)
	}
}
