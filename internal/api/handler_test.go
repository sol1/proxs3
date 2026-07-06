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
	"sync"
	"testing"
	"time"

	"github.com/sol1/proxs3/internal/cache"
	"github.com/sol1/proxs3/internal/config"
	"github.com/sol1/proxs3/internal/s3client"
)

// mockS3Client implements s3client.S3Client for testing.
type mockS3Client struct {
	id      string
	mu      sync.Mutex            // guards objects for tests with concurrent uploads
	objects map[string]mockObject // key -> object
	healthy bool
	listErr error
	headErr error
	getErr  error
	putErr  error
	delErr  error
	// Optional hooks for concurrency tests: PutObject signals putStarted (if
	// set), then blocks until putRelease is closed or the context is canceled.
	putStarted chan string
	putRelease chan struct{}
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

// object returns a stored object under the lock, for assertions in tests
// that run concurrent uploads.
func (m *mockS3Client) object(key string) (mockObject, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	return obj, ok
}

func (m *mockS3Client) ListObjects(ctx context.Context, prefix string) ([]s3client.ObjectInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	obj, ok := m.object(key)
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
	obj, ok := m.object(key)
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

func (m *mockS3Client) DownloadToFile(ctx context.Context, key string, w io.WriterAt) (int64, error) {
	if m.getErr != nil {
		return 0, m.getErr
	}
	obj, ok := m.object(key)
	if !ok {
		return 0, fmt.Errorf("not found: %s", key)
	}
	n, err := w.WriteAt([]byte(obj.data), 0)
	return int64(n), err
}

func (m *mockS3Client) PutObject(ctx context.Context, key string, body io.Reader, size int64) error {
	if m.putErr != nil {
		return m.putErr
	}
	if m.putStarted != nil {
		m.putStarted <- key
	}
	if m.putRelease != nil {
		select {
		case <-m.putRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, _ := io.ReadAll(body)
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *mockS3Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
		cache:          fc,
		health:         map[string]bool{mock.id: mock.healthy},
		usage:          map[string]int64{mock.id: 0},
		uploads:        make(map[string]*uploadState),
		uploadRequests: make(chan string, 1024),
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

func TestHandleDownload_ObjectDeletedOnS3_RemovesCache(t *testing.T) {
	mock := newMockClient("s3test")
	// HeadObject reports the object is gone (404), distinct from a transport error.
	mock.headErr = s3client.ErrNotFound
	s := newTestServer(t, mock)

	// Pre-populate cache to simulate a previously downloaded, now-deleted object.
	key := "template/iso/user-data-118.iso"
	meta := cache.FileMeta{ETag: "\"v1\"", Size: 5}
	s.cache.Store("s3test", key, strings.NewReader("stale"), meta)
	if s.cache.Path("s3test", key) == "" {
		t.Fatal("setup: expected cached file to exist")
	}

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key="+key, nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	// Must fail closed, not serve the stale identity payload.
	if w.Code != 404 {
		t.Fatalf("expected 404 when object deleted on S3, got %d: %s", w.Code, w.Body.String())
	}
	// Stale cache must be purged so it cannot be served on a later request.
	if p := s.cache.Path("s3test", key); p != "" {
		t.Errorf("expected stale cache removed, but Path returned %q", p)
	}
}

func TestHandleDownload_LocalPendingUpload_ServedNotPurged(t *testing.T) {
	mock := newMockClient("s3test")
	// The object does not exist on S3 yet — the watcher hasn't uploaded it.
	mock.headErr = s3client.ErrNotFound
	s := newTestServer(t, mock)

	// PVE writes files directly into the cache dir; no meta sidecar exists yet.
	key := "template/iso/user-data-200.iso"
	localPath := filepath.Join(s.cfg.CacheDir, "s3test", "template", "iso", "user-data-200.iso")
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, []byte("cloud-init payload"), 0644)

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key="+key, nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for local file pending upload, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] != localPath {
		t.Errorf("expected local path %q, got %q", localPath, resp["path"])
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Errorf("expected local file to survive, stat failed: %v", err)
	}

	// The file must be queued for upload through the watcher's debounce loop.
	select {
	case p := <-s.uploadRequests:
		if p != localPath {
			t.Errorf("expected upload request for %q, got %q", localPath, p)
		}
	default:
		t.Error("expected the served pending file to be queued for upload")
	}
}

func TestHandleDownload_PendingRewrite_ServesLocalNotS3(t *testing.T) {
	mock := newMockClient("s3test")
	key := "template/iso/user-data-118.iso"
	// An older version of the object still exists on S3 under the same key.
	mock.objects[key] = mockObject{data: "old", size: 3, etag: "\"old\"", lastModified: time.Now().Add(-time.Hour)}
	s := newTestServer(t, mock)

	// PVE rewrote the file locally; the watcher marked it pending upload.
	// The stale S3 copy must not clobber it via the staleness check.
	localPath := filepath.Join(s.cfg.CacheDir, "s3test", "template", "iso", "user-data-118.iso")
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, []byte("new payload"), 0644)
	s.cache.StoreMeta("s3test", key, cache.FileMeta{Size: 11, LastModified: time.Now(), PendingUpload: true})

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key="+key, nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for pending rewrite, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] != localPath {
		t.Errorf("expected local path %q, got %q", localPath, resp["path"])
	}
	data, err := os.ReadFile(localPath)
	if err != nil || string(data) != "new payload" {
		t.Errorf("expected local rewrite to survive, got %q (err %v)", data, err)
	}
	select {
	case <-s.uploadRequests:
	default:
		t.Error("expected the pending rewrite to be queued for upload")
	}
}

func TestHandleDownload_ObjectDeletedOnS3_WatcherUploadedMeta_RemovesCache(t *testing.T) {
	mock := newMockClient("s3test")
	mock.headErr = s3client.ErrNotFound
	s := newTestServer(t, mock)

	// Simulate a locally created file the watcher already uploaded: content on
	// disk plus the meta the watcher writes after PutObject (size, no ETag).
	// With confirmed S3 provenance, a 404 means deleted on S3 → fail closed.
	key := "template/iso/user-data-118.iso"
	localPath := filepath.Join(s.cfg.CacheDir, "s3test", "template", "iso", "user-data-118.iso")
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, []byte("stale"), 0644)
	s.cache.StoreMeta("s3test", key, cache.FileMeta{Size: 5, LastModified: time.Now()})

	req := httptest.NewRequest("GET", "/v1/download?storage=s3test&key="+key, nil)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404 when uploaded object was deleted on S3, got %d: %s", w.Code, w.Body.String())
	}
	if p := s.cache.Path("s3test", key); p != "" {
		t.Errorf("expected cache purged, but Path returned %q", p)
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

// --- Resync handler tests ---

// writeCacheFile writes content into the canonical cache path for storage/key
// and sets its mtime, so resync's mtime comparison is deterministic.
func writeCacheFile(t *testing.T, s *Server, storageID, key, content string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(s.cfg.CacheDir, storageID, key)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return p
}

func TestHandleResync_UploadsMissing(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)
	writeCacheFile(t, s, "s3test", "images/9113/disk-0", "hello world", time.Now())

	req := httptest.NewRequest("GET", "/v1/resync?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if _, ok := mock.objects["images/9113/disk-0"]; !ok {
		t.Fatalf("expected object uploaded; bucket has %v", mock.objects)
	}
	if !strings.Contains(w.Body.String(), "uploaded=1") {
		t.Errorf("expected summary uploaded=1, got: %s", w.Body.String())
	}
}

func TestHandleResync_SkipsInSync(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["template/iso/a.iso"] = mockObject{
		data: "hello", size: 5, etag: "\"x\"", lastModified: time.Now(),
	}
	s := newTestServer(t, mock)
	writeCacheFile(t, s, "s3test", "template/iso/a.iso", "hello", time.Now())

	req := httptest.NewRequest("GET", "/v1/resync?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "uploaded=0") {
		t.Errorf("expected uploaded=0, got: %s", body)
	}
	// Metadata should be backfilled so the watcher won't re-upload
	if s.cache.GetMeta("s3test", "template/iso/a.iso") == nil {
		t.Error("expected meta to be backfilled for in-sync file")
	}
}

func TestHandleResync_SizeMismatchLocalNewer_Uploads(t *testing.T) {
	mock := newMockClient("s3test")
	// S3 has an older, smaller copy
	mock.objects["snippets/cloud.yaml"] = mockObject{
		data: "old", size: 3, etag: "\"old\"", lastModified: time.Now().Add(-1 * time.Hour),
	}
	s := newTestServer(t, mock)
	// Local has a newer, larger copy
	writeCacheFile(t, s, "s3test", "snippets/cloud.yaml", "much longer content", time.Now())

	req := httptest.NewRequest("GET", "/v1/resync?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "uploaded=1") {
		t.Errorf("expected uploaded=1, got: %s", body)
	}
	if !strings.Contains(body, "WARNING") {
		t.Errorf("expected warning emitted for date conflict, got: %s", body)
	}
	if mock.objects["snippets/cloud.yaml"].size != int64(len("much longer content")) {
		t.Errorf("expected S3 to have new content size, got: %+v", mock.objects["snippets/cloud.yaml"])
	}
}

func TestHandleResync_SizeMismatchS3Newer_SkipsByDefault(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["snippets/cloud.yaml"] = mockObject{
		data: "newer-on-s3", size: 11, etag: "\"new\"", lastModified: time.Now(),
	}
	s := newTestServer(t, mock)
	// Local file with mtime in the past
	writeCacheFile(t, s, "s3test", "snippets/cloud.yaml", "old", time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest("GET", "/v1/resync?storage=s3test", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "conflicts=1") {
		t.Errorf("expected conflicts=1, got: %s", body)
	}
	if mock.objects["snippets/cloud.yaml"].size != 11 {
		t.Errorf("S3 object should be untouched, got: %+v", mock.objects["snippets/cloud.yaml"])
	}
}

func TestHandleResync_SizeMismatchS3Newer_ForceOverwrites(t *testing.T) {
	mock := newMockClient("s3test")
	mock.objects["snippets/cloud.yaml"] = mockObject{
		data: "newer-on-s3", size: 11, etag: "\"new\"", lastModified: time.Now(),
	}
	s := newTestServer(t, mock)
	writeCacheFile(t, s, "s3test", "snippets/cloud.yaml", "old", time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest("GET", "/v1/resync?storage=s3test&force=1", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "uploaded=1") {
		t.Errorf("expected uploaded=1 with force, got: %s", body)
	}
	if mock.objects["snippets/cloud.yaml"].size != 3 {
		t.Errorf("expected S3 to be overwritten with local size 3, got: %+v", mock.objects["snippets/cloud.yaml"])
	}
}

func TestHandleResync_MissingStorageParam(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/resync", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleResync_UnknownStorage(t *testing.T) {
	mock := newMockClient("s3test")
	s := newTestServer(t, mock)

	req := httptest.NewRequest("GET", "/v1/resync?storage=other", nil)
	w := httptest.NewRecorder()
	s.handleResync(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
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
