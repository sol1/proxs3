package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sol1/proxs3/internal/cache"
	"github.com/sol1/proxs3/internal/config"
	"github.com/sol1/proxs3/internal/s3client"
)

// mockS3Client implements s3client.S3Client for testing.
type mockS3Client struct {
	id      string
	objects map[string]mockObject // key -> object
	healthy bool
	listErr error
	headErr error
	getErr  error
	putErr  error
	delErr  error
}

type mockObject struct {
	data         string
	size         int64
	etag         string
	lastModified time.Time
}

func newMockClient(id string) *mockS3Client {
	return &mockS3Client{
		id:      id,
		objects: make(map[string]mockObject),
		healthy: true,
	}
}

func (m *mockS3Client) StorageID() string { return m.id }

func (m *mockS3Client) HeadBucket(ctx context.Context) error {
	if !m.healthy {
		return fmt.Errorf("bucket unreachable")
	}
	return nil
}

func (m *mockS3Client) ListObjects(ctx context.Context, prefix string) ([]s3client.ObjectInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var objects []s3client.ObjectInfo
	for key, obj := range m.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, s3client.ObjectInfo{
				Key:          key,
				Size:         obj.size,
				ETag:         obj.etag,
				LastModified: obj.lastModified,
			})
		}
	}
	return objects, nil
}

func (m *mockS3Client) HeadObject(ctx context.Context, key string) (*s3client.ObjectInfo, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return &s3client.ObjectInfo{
		Key:          key,
		Size:         obj.size,
		ETag:         obj.etag,
		LastModified: obj.lastModified,
	}, nil
}

func (m *mockS3Client) GetObject(ctx context.Context, key string) (*s3client.GetObjectResult, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return &s3client.GetObjectResult{
		Body:         io.NopCloser(strings.NewReader(obj.data)),
		Size:         obj.size,
		ETag:         obj.etag,
		LastModified: obj.lastModified,
	}, nil
}

func (m *mockS3Client) PutObject(ctx context.Context, key string, body io.Reader, size int64) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, _ := io.ReadAll(body)
	m.objects[key] = mockObject{
		data:         string(data),
		size:         size,
		etag:         fmt.Sprintf("\"%x\"", len(data)),
		lastModified: time.Now(),
	}
	return nil
}

func (m *mockS3Client) DeleteObject(ctx context.Context, key string) error {
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objects, key)
	return nil
}

func (m *mockS3Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	obj, ok := m.objects[srcKey]
	if !ok {
		return fmt.Errorf("source key %s not found", srcKey)
	}
	m.objects[dstKey] = obj
	return nil
}

func (m *mockS3Client) GetObjectTagging(ctx context.Context, key string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (m *mockS3Client) PutObjectTagging(ctx context.Context, key string, tags map[string]string) error {
	return nil
}

// newTestServer creates a Server with a mock client for testing.
func newTestServer(t *testing.T, mock *mockS3Client) *Server {
	t.Helper()
	dir := t.TempDir()
	fc, err := cache.New(dir, 100)
	if err != nil {
		t.Fatalf("cache.New failed: %v", err)
	}

	s := &Server{
		cfg: &config.DaemonConfig{
			CacheDir:   dir,
			CacheMaxMB: 100,
			HeadroomGB: 100,
		},
		clients: map[string]s3client.S3Client{
			mock.id: mock,
		},
		cache:  fc,
		health: map[string]bool{mock.id: mock.healthy},
		usage:  map[string]int64{mock.id: 0},
	}
	return s
}

// --- Status handler tests ---

func TestHandleStatus(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)
	s.health["s3test"] = true
	s.usage["s3test"] = 1024 * 1024 * 100 // 100MB

	req := httptest.NewRequest("GET", "/v1/status?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.StorageID != "s3test" {
		t.Errorf("expected storage_id 's3test', got %q", resp.StorageID)
	}
	if !resp.Online {
		t.Error("expected online=true")
	}
	if resp.Used != 1024*1024*100 {
		t.Errorf("expected used 104857600, got %d", resp.Used)
	}
	if resp.Available != 100*1073741824 {
		t.Errorf("expected available %d, got %d", 100*1073741824, resp.Available)
	}
	if resp.Total != resp.Used+resp.Available {
		t.Error("expected total = used + available")
	}
}

func TestHandleStatus_MissingParam(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleStatus_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/status?storage=unknown", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleStatus_Offline(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)
	s.health["s3test"] = false

	req := httptest.NewRequest("GET", "/v1/status?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	var resp StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.Online {
		t.Error("expected online=false")
	}
}

// --- List handler tests ---

func TestHandleList(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/debian.iso"] = mockObject{size: 600 * 1024 * 1024, etag: "\"abc\""}
	mock.objects["template/iso/ubuntu.iso"] = mockObject{size: 1200 * 1024 * 1024, etag: "\"def\""}
	mock.objects["snippets/cloud-init.yaml"] = mockObject{size: 1024, etag: "\"ghi\""}
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=iso", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}

	// Check that volumes have correct content type and format
	for _, v := range volumes {
		if v.Content != "iso" {
			t.Errorf("expected content 'iso', got %q", v.Content)
		}
		if v.Format != "iso" {
			t.Errorf("expected format 'iso', got %q", v.Format)
		}
		if !strings.HasPrefix(v.Volume, "s3test:iso/") {
			t.Errorf("expected volume to start with 's3test:iso/', got %q", v.Volume)
		}
	}
}

func TestHandleList_SkipsDirectoryMarkers(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/"] = mockObject{size: 0}
	mock.objects["template/iso/real.iso"] = mockObject{size: 100}
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=iso", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume (directory marker skipped), got %d", len(volumes))
	}
}

func TestHandleList_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=unknown&content=iso", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleList_S3Error(t *testing.T) {
	mock := newMockClient("s3test")
	mock.listErr = fmt.Errorf("connection refused")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=iso", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	// Should return 200 with empty list, not 500
	if w.Code != 200 {
		t.Fatalf("expected 200 (empty list on S3 error), got %d", w.Code)
	}

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)
	if len(volumes) != 0 {
		t.Errorf("expected empty list on S3 error, got %d", len(volumes))
	}
}

func TestHandleList_EmptyBucket(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=iso", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHandleList_Backups(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["dump/vzdump-qemu-100-2024_01_01.vma.zst"] = mockObject{size: 5 * 1024 * 1024 * 1024}
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=backup", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(volumes))
	}
	if volumes[0].Content != "backup" {
		t.Errorf("expected content 'backup', got %q", volumes[0].Content)
	}
}

func TestHandleList_IncludesLocalPendingFiles(t *testing.T) {
	mock := newMockClient("s3test")
	// One file already in S3
	mock.objects["snippets/existing.yaml"] = mockObject{size: 100, etag: "\"abc\""}
	s := newTestServer(t, mock)

	// Create a local file that is NOT in S3 (simulates pending watcher upload)
	localDir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(localDir, 0755)
	os.WriteFile(filepath.Join(localDir, "pending.yaml"), []byte("pending data"), 0644)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=snippets", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes (1 S3 + 1 local pending), got %d", len(volumes))
	}

	// Verify both files are present
	found := map[string]bool{}
	for _, v := range volumes {
		found[v.Volume] = true
	}
	if !found["s3test:snippets/existing.yaml"] {
		t.Error("expected S3 file 's3test:snippets/existing.yaml' in list")
	}
	if !found["s3test:snippets/pending.yaml"] {
		t.Error("expected local pending file 's3test:snippets/pending.yaml' in list")
	}
}

func TestHandleList_LocalFileNotDuplicated(t *testing.T) {
	mock := newMockClient("s3test")
	// File exists in both S3 and local cache
	mock.objects["snippets/both.yaml"] = mockObject{size: 100, etag: "\"abc\""}
	s := newTestServer(t, mock)

	localDir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(localDir, 0755)
	os.WriteFile(filepath.Join(localDir, "both.yaml"), []byte("data"), 0644)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=snippets", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume (no duplicates), got %d", len(volumes))
	}
}

func TestHandleList_SkipsTmpFiles(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	localDir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(localDir, 0755)
	os.WriteFile(filepath.Join(localDir, "upload.tmp"), []byte("partial"), 0644)
	os.WriteFile(filepath.Join(localDir, "real.yaml"), []byte("data"), 0644)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=snippets", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume (.tmp skipped), got %d", len(volumes))
	}
	if volumes[0].Volume != "s3test:snippets/real.yaml" {
		t.Errorf("expected 's3test:snippets/real.yaml', got %q", volumes[0].Volume)
	}
}

func TestHandleList_S3Error_ShowsLocalFiles(t *testing.T) {
	mock := newMockClient("s3test")
	mock.listErr = fmt.Errorf("connection refused")
	s := newTestServer(t, mock)

	// Local file exists but S3 is unreachable
	localDir := filepath.Join(s.cfg.CacheDir, "s3test", "snippets")
	os.MkdirAll(localDir, 0755)
	os.WriteFile(filepath.Join(localDir, "local.yaml"), []byte("data"), 0644)

	req := httptest.NewRequest("GET", "/v1/list?storage=s3test&content=snippets", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var volumes []VolumeInfo
	json.NewDecoder(w.Body).Decode(&volumes)

	if len(volumes) != 1 {
		t.Fatalf("expected 1 local volume when S3 unreachable, got %d", len(volumes))
	}
}

// --- Download handler tests ---

func TestHandleDownload_Fresh(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/debian.iso"] = mockObject{
		data: "fake iso data",
		size: 13,
		etag: "\"v1\"",
	}
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/debian.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)

	path := resp["path"]
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Verify file was cached
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading cached file: %v", err)
	}
	if string(content) != "fake iso data" {
		t.Errorf("expected 'fake iso data', got %q", string(content))
	}
}

func TestHandleDownload_CachedFresh(t *testing.T) {
	mock := newMockClient("s3test")
	now := time.Now()
	mock.objects["template/iso/debian.iso"] = mockObject{
		data:         "iso data",
		size:         8,
		etag:         "\"v1\"",
		lastModified: now,
	}
	s := newTestServer(t, mock)

	// Pre-populate cache
	meta := cache.FileMeta{ETag: "\"v1\"", LastModified: now, Size: 8}
	s.cache.Store("s3test", "template/iso/debian.iso", strings.NewReader("iso data"), meta)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/debian.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] == "" {
		t.Fatal("expected cached path")
	}
}

func TestHandleDownload_CachedStale(t *testing.T) {
	mock := newMockClient("s3test")
	now := time.Now()
	mock.objects["template/iso/debian.iso"] = mockObject{
		data:         "new version",
		size:         11,
		etag:         "\"v2\"",
		lastModified: now,
	}
	s := newTestServer(t, mock)

	// Pre-populate cache with old version
	meta := cache.FileMeta{ETag: "\"v1\"", LastModified: now.Add(-time.Hour), Size: 8}
	cachedPath, _ := s.cache.Store("s3test", "template/iso/debian.iso", strings.NewReader("old data"), meta)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/debian.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)

	// Should have re-downloaded the new version
	content, _ := os.ReadFile(resp["path"])
	if string(content) != "new version" {
		t.Errorf("expected 'new version', got %q", string(content))
	}
	_ = cachedPath
}

func TestHandleDownload_S3Unreachable_ServeStaleCache(t *testing.T) {
	mock := newMockClient("s3test")
	mock.headErr = fmt.Errorf("connection refused")
	s := newTestServer(t, mock)

	// Pre-populate cache
	meta := cache.FileMeta{ETag: "\"v1\"", Size: 5}
	s.cache.Store("s3test", "template/iso/debian.iso", strings.NewReader("hello"), meta)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/debian.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	// Should serve stale cache, not error
	if w.Code != 200 {
		t.Fatalf("expected 200 (stale cache), got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] == "" {
		t.Fatal("expected cached path when S3 unreachable")
	}
}

func TestHandleDownload_S3Unreachable_NoCache(t *testing.T) {
	mock := newMockClient("s3test")
	mock.getErr = fmt.Errorf("connection refused")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/missing.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500 when S3 unreachable and no cache, got %d", w.Code)
	}
}

func TestHandleDownload_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/download?storage=unknown&key=k", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDownload_ObjectNotFound(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key=template/iso/missing.iso", nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500 for missing object, got %d", w.Code)
	}
}

// --- Upload handler tests ---

func TestHandleUpload(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	// Create a local file to upload
	localFile := filepath.Join(s.cfg.CacheDir, "upload-test.iso")
	os.WriteFile(localFile, []byte("uploaded content"), 0644)

	req := httptest.NewRequest("GET", "/v1/upload?storage=s3test&key=template/iso/test.iso&path="+localFile, nil)
	w := httptest.NewRecorder()
	s.handleUpload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}

	// Verify object was stored in mock
	obj, ok := mock.objects["template/iso/test.iso"]
	if !ok {
		t.Fatal("expected object to be stored in mock S3")
	}
	if obj.data != "uploaded content" {
		t.Errorf("expected 'uploaded content', got %q", obj.data)
	}
}

func TestHandleUpload_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/upload?storage=unknown&key=k&path=/tmp/x", nil)
	w := httptest.NewRecorder()
	s.handleUpload(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleUpload_MissingFile(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/upload?storage=s3test&key=k&path=/nonexistent/file", nil)
	w := httptest.NewRecorder()
	s.handleUpload(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpload_S3Error(t *testing.T) {
	mock := newMockClient("s3test")
	mock.putErr = fmt.Errorf("access denied")
	s := newTestServer(t, mock)

	localFile := filepath.Join(s.cfg.CacheDir, "upload-test.iso")
	os.WriteFile(localFile, []byte("data"), 0644)

	req := httptest.NewRequest("GET", "/v1/upload?storage=s3test&key=k&path="+localFile, nil)
	w := httptest.NewRecorder()
	s.handleUpload(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// --- Delete handler tests ---

func TestHandleDelete(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/old.iso"] = mockObject{size: 100}
	s := newTestServer(t, mock)

	// Also cache the file
	s.cache.Store("s3test", "template/iso/old.iso", strings.NewReader("data"), cache.FileMeta{Size: 4})

	req := httptest.NewRequest("GET", "/v1/delete?storage=s3test&key=template/iso/old.iso", nil)
	w := httptest.NewRecorder()
	s.handleDelete(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}

	// Verify object was deleted from mock
	if _, ok := mock.objects["template/iso/old.iso"]; ok {
		t.Error("expected object to be deleted from mock S3")
	}

	// Verify cache was cleaned
	if s.cache.Has("s3test", "template/iso/old.iso") {
		t.Error("expected object to be removed from cache")
	}
}

func TestHandleDelete_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/delete?storage=unknown&key=k", nil)
	w := httptest.NewRecorder()
	s.handleDelete(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDelete_S3Error(t *testing.T) {
	mock := newMockClient("s3test")
	mock.delErr = fmt.Errorf("access denied")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/delete?storage=s3test&key=k", nil)
	w := httptest.NewRecorder()
	s.handleDelete(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// --- Path handler tests ---

func TestHandlePath_DelegatesToDownload(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/debian.iso"] = mockObject{
		data: "iso data",
		size: 8,
		etag: "\"v1\"",
	}
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/path?storage=s3test&key=template/iso/debian.iso", nil)
	w := httptest.NewRecorder()
	s.handlePath(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] == "" {
		t.Fatal("expected path in response")
	}
}

// --- Config handler tests ---

func TestHandleConfig(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/config", nil)
	w := httptest.NewRecorder()
	s.handleConfig(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["cache_dir"] != s.cfg.CacheDir {
		t.Errorf("expected cache_dir %q, got %q", s.cfg.CacheDir, resp["cache_dir"])
	}
}

// --- Health check tests ---

func TestCheckAllHealth_Healthy(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/a.iso"] = mockObject{size: 100}
	mock.objects["template/iso/b.iso"] = mockObject{size: 200}
	s := newTestServer(t, mock)

	s.checkAllHealth()

	s.healthMu.RLock()
	online := s.health["s3test"]
	used := s.usage["s3test"]
	s.healthMu.RUnlock()

	if !online {
		t.Error("expected online=true")
	}
	if used != 300 {
		t.Errorf("expected used=300, got %d", used)
	}
}

func TestCheckAllHealth_Unhealthy(t *testing.T) {
	mock := newMockClient("s3test")
	mock.healthy = false
	s := newTestServer(t, mock)

	s.checkAllHealth()

	s.healthMu.RLock()
	online := s.health["s3test"]
	s.healthMu.RUnlock()

	if online {
		t.Error("expected online=false")
	}
}
