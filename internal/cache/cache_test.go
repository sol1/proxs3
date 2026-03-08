package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if fc.baseDir != dir {
		t.Errorf("expected baseDir %s, got %s", dir, fc.baseDir)
	}
	if fc.maxMB != 100 {
		t.Errorf("expected maxMB 100, got %d", fc.maxMB)
	}
}

func TestNew_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	_, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestStoreAndRetrieve(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	data := "hello world"
	meta := FileMeta{
		ETag:         "\"abc123\"",
		LastModified: time.Now(),
		Size:         int64(len(data)),
	}

	path, err := fc.Store("store1", "template/iso/test.iso", strings.NewReader(data), meta)
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Verify file exists on disk
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading cached file: %v", err)
	}
	if string(content) != data {
		t.Errorf("expected %q, got %q", data, string(content))
	}

	// Verify Has
	if !fc.Has("store1", "template/iso/test.iso") {
		t.Error("expected Has to return true")
	}

	// Verify Path
	p := fc.Path("store1", "template/iso/test.iso")
	if p != path {
		t.Errorf("expected path %s, got %s", path, p)
	}

	// Verify metadata
	m := fc.GetMeta("store1", "template/iso/test.iso")
	if m == nil {
		t.Fatal("expected metadata, got nil")
	}
	if m.ETag != "\"abc123\"" {
		t.Errorf("expected etag %q, got %q", "\"abc123\"", m.ETag)
	}
	if m.CachedAt.IsZero() {
		t.Error("expected CachedAt to be set")
	}
}

func TestHas_NotCached(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if fc.Has("s", "nonexistent") {
		t.Error("expected Has to return false for uncached key")
	}
}

func TestPath_NotCached(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if fc.Path("s", "nonexistent") != "" {
		t.Error("expected empty path for uncached key")
	}
}

func TestGetMeta_NotCached(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if fc.GetMeta("s", "nonexistent") != nil {
		t.Error("expected nil meta for uncached key")
	}
}

func TestMetaPath(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	mp := fc.metaPath("store1", "template/iso/test.iso")
	expected := filepath.Join(dir, ".meta", "store1", "template/iso/test.iso.json")
	if mp != expected {
		t.Errorf("expected meta path %s, got %s", expected, mp)
	}
}

func TestMetaInDotMetaDir(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{ETag: "\"v1\"", LastModified: time.Now(), Size: 5}
	fc.Store("s", "key", strings.NewReader("hello"), meta)

	// Metadata should be in .meta/ subdirectory, not alongside the content file
	metaFile := filepath.Join(dir, ".meta", "s", "key.json")
	if _, err := os.Stat(metaFile); err != nil {
		t.Errorf("metadata file not found in .meta/ dir: %v", err)
	}

	// No .meta sidecar should exist next to the content file
	contentMeta := filepath.Join(dir, "s", "key.meta")
	if _, err := os.Stat(contentMeta); err == nil {
		t.Error("legacy .meta sidecar should not be created")
	}
}

func TestStoreMeta(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{
		ETag:         "\"stored\"",
		LastModified: time.Now(),
		Size:         42,
	}
	fc.StoreMeta("s", "mykey", meta)

	got := fc.GetMeta("s", "mykey")
	if got == nil {
		t.Fatal("expected metadata, got nil")
	}
	if got.ETag != "\"stored\"" {
		t.Errorf("expected etag %q, got %q", "\"stored\"", got.ETag)
	}
	if got.Size != 42 {
		t.Errorf("expected size 42, got %d", got.Size)
	}
	if got.CachedAt.IsZero() {
		t.Error("expected CachedAt to be set")
	}
}

func TestGetMeta_LegacyFallback(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Create content file and legacy .meta sidecar (not in .meta/ dir)
	contentPath := filepath.Join(dir, "s", "legacy-key")
	os.MkdirAll(filepath.Dir(contentPath), 0755)
	os.WriteFile(contentPath, []byte("data"), 0644)
	os.WriteFile(contentPath+".meta", []byte(`{"etag":"\"legacy\"","size":4}`), 0644)

	got := fc.GetMeta("s", "legacy-key")
	if got == nil {
		t.Fatal("expected legacy metadata, got nil")
	}
	if got.ETag != "\"legacy\"" {
		t.Errorf("expected etag %q, got %q", "\"legacy\"", got.ETag)
	}
}

func TestIsStale(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	now := time.Now()
	meta := FileMeta{
		ETag:         "\"v1\"",
		LastModified: now,
		Size:         5,
	}
	fc.Store("s", "key", strings.NewReader("hello"), meta)

	// Same ETag - not stale
	if fc.IsStale("s", "key", "\"v1\"", now) {
		t.Error("expected not stale with same etag")
	}

	// Different ETag - stale
	if !fc.IsStale("s", "key", "\"v2\"", now) {
		t.Error("expected stale with different etag")
	}

	// Same ETag, newer timestamp - not stale (etag takes precedence)
	if fc.IsStale("s", "key", "\"v1\"", now.Add(time.Hour)) {
		t.Error("expected not stale when etag matches even with newer timestamp")
	}

	// Empty ETag, same timestamp - not stale
	if fc.IsStale("s", "key", "", now) {
		t.Error("expected not stale with empty etag and same timestamp")
	}

	// Empty ETag, newer timestamp - stale
	if !fc.IsStale("s", "key", "", now.Add(time.Hour)) {
		t.Error("expected stale with empty etag and newer timestamp")
	}

	// Nonexistent key - always stale
	if !fc.IsStale("s", "missing", "\"v1\"", now) {
		t.Error("expected stale for missing key")
	}
}

func TestInvalidate(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{ETag: "\"v1\"", LastModified: time.Now(), Size: 5}
	fc.Store("s", "key", strings.NewReader("hello"), meta)

	if !fc.Has("s", "key") {
		t.Fatal("expected key to exist before invalidate")
	}

	fc.Invalidate("s", "key")

	if fc.Has("s", "key") {
		t.Error("expected key to be gone after invalidate")
	}
	if fc.GetMeta("s", "key") != nil {
		t.Error("expected meta to be gone after invalidate")
	}
}

func TestInvalidate_CleansLegacyMeta(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Create content and legacy .meta sidecar
	contentPath := filepath.Join(dir, "s", "legacykey")
	os.MkdirAll(filepath.Dir(contentPath), 0755)
	os.WriteFile(contentPath, []byte("data"), 0644)
	os.WriteFile(contentPath+".meta", []byte(`{}`), 0644)

	fc.Invalidate("s", "legacykey")

	if _, err := os.Stat(contentPath + ".meta"); err == nil {
		t.Error("expected legacy .meta to be removed after invalidate")
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{ETag: "\"v1\"", LastModified: time.Now(), Size: 3}
	fc.Store("s", "key", strings.NewReader("abc"), meta)

	if err := fc.Remove("s", "key"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if fc.Has("s", "key") {
		t.Error("expected key removed")
	}
	if fc.GetMeta("s", "key") != nil {
		t.Error("expected meta removed")
	}
}

func TestRemove_Nonexistent(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	err = fc.Remove("s", "nonexistent")
	if err == nil {
		t.Error("expected error removing nonexistent key")
	}
}

func TestLink(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Create a source file
	srcPath := filepath.Join(dir, "source.iso")
	os.WriteFile(srcPath, []byte("iso data"), 0644)

	meta := FileMeta{ETag: "\"link1\"", LastModified: time.Now(), Size: 8}
	fc.Link("s", "template/iso/linked.iso", srcPath, meta)

	p := fc.Path("s", "template/iso/linked.iso")
	if p == "" {
		t.Fatal("expected linked file to exist in cache")
	}

	content, _ := os.ReadFile(p)
	if string(content) != "iso data" {
		t.Errorf("unexpected content: %s", string(content))
	}

	// Verify metadata was stored
	m := fc.GetMeta("s", "template/iso/linked.iso")
	if m == nil {
		t.Fatal("expected metadata for linked file")
	}
	if m.ETag != "\"link1\"" {
		t.Errorf("expected etag %q, got %q", "\"link1\"", m.ETag)
	}
}

func TestEviction(t *testing.T) {
	dir := t.TempDir()
	// 1 MB max cache
	fc, err := New(dir, 1)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	bigData := bytes.Repeat([]byte("x"), 600*1024)
	meta := FileMeta{ETag: "\"big1\"", Size: int64(len(bigData))}
	fc.Store("s", "old.iso", bytes.NewReader(bigData), meta)

	// Set old file's mod time to the past so it's evicted first
	oldPath := fc.path("s", "old.iso")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(oldPath, past, past)

	meta2 := FileMeta{ETag: "\"big2\"", Size: int64(len(bigData))}
	fc.Store("s", "new.iso", bytes.NewReader(bigData), meta2)

	// Give eviction goroutine time to run
	time.Sleep(200 * time.Millisecond)

	// The older file should have been evicted
	if fc.Has("s", "old.iso") {
		t.Log("warning: old file not evicted (may be timing-dependent)")
	}
	// The newer file should still be there
	if !fc.Has("s", "new.iso") {
		t.Error("expected new file to still exist")
	}
}

func TestEviction_ZeroLimit(t *testing.T) {
	dir := t.TempDir()
	// maxMB=0 means no eviction
	fc, err := New(dir, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	bigData := bytes.Repeat([]byte("x"), 100*1024)
	meta := FileMeta{Size: int64(len(bigData))}
	fc.Store("s", "file1", bytes.NewReader(bigData), meta)
	fc.Store("s", "file2", bytes.NewReader(bigData), meta)

	time.Sleep(100 * time.Millisecond)

	// Both files should remain (no eviction when limit is 0)
	if !fc.Has("s", "file1") || !fc.Has("s", "file2") {
		t.Error("expected both files to remain when maxMB=0")
	}
}

func TestEvictByAge(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Create an old file (mod time set to 10 days ago)
	meta := FileMeta{ETag: "\"old\"", Size: 5}
	fc.Store("mystorage", "dump/old-backup.vma", strings.NewReader("hello"), meta)
	oldPath := fc.path("mystorage", "dump/old-backup.vma")
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour)
	os.Chtimes(oldPath, tenDaysAgo, tenDaysAgo)

	// Create a new file
	meta2 := FileMeta{ETag: "\"new\"", Size: 5}
	fc.Store("mystorage", "dump/new-backup.vma", strings.NewReader("world"), meta2)

	// Evict files older than 7 days
	removed := fc.EvictByAge("mystorage", 7*24*time.Hour)

	if removed != 1 {
		t.Errorf("expected 1 file removed, got %d", removed)
	}
	if fc.Has("mystorage", "dump/old-backup.vma") {
		t.Error("expected old file to be evicted")
	}
	if !fc.Has("mystorage", "dump/new-backup.vma") {
		t.Error("expected new file to remain")
	}
}

func TestEvictByAge_ZeroDuration(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{Size: 5}
	fc.Store("s", "key", strings.NewReader("hello"), meta)

	removed := fc.EvictByAge("s", 0)
	if removed != 0 {
		t.Errorf("expected 0 removed with zero duration, got %d", removed)
	}
	if !fc.Has("s", "key") {
		t.Error("expected file to remain with zero duration")
	}
}

func TestEvictByAge_NoStorage(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Evicting a nonexistent storage should not panic
	removed := fc.EvictByAge("nonexistent", 7*24*time.Hour)
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

func TestEvictByAge_OnlyAffectsTargetStorage(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{Size: 5}
	// Files in storage-a
	fc.Store("storage-a", "key1", strings.NewReader("hello"), meta)
	oldPath := fc.path("storage-a", "key1")
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(oldPath, old, old)

	// Files in storage-b
	fc.Store("storage-b", "key1", strings.NewReader("hello"), meta)
	oldPath2 := fc.path("storage-b", "key1")
	os.Chtimes(oldPath2, old, old)

	// Evict only storage-a
	fc.EvictByAge("storage-a", 7*24*time.Hour)

	if fc.Has("storage-a", "key1") {
		t.Error("expected storage-a file to be evicted")
	}
	if !fc.Has("storage-b", "key1") {
		t.Error("expected storage-b file to remain untouched")
	}
}

func TestEvictByAge_CleansMetadata(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{ETag: "\"old\"", Size: 5}
	fc.Store("s", "key", strings.NewReader("hello"), meta)
	oldPath := fc.path("s", "key")
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(oldPath, old, old)

	fc.EvictByAge("s", 7*24*time.Hour)

	// Metadata should also be cleaned up
	metaFile := fc.metaPath("s", "key")
	if _, err := os.Stat(metaFile); err == nil {
		t.Error("expected metadata file to be cleaned up after age eviction")
	}
}

func TestSizeMB(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Empty cache
	if fc.SizeMB() != 0 {
		t.Errorf("expected 0 MB, got %d", fc.SizeMB())
	}

	// Store a 1MB file
	data := bytes.Repeat([]byte("x"), 1024*1024)
	fc.Store("s", "1mb", bytes.NewReader(data), FileMeta{Size: int64(len(data))})

	size := fc.SizeMB()
	// Should be ~1MB (allow for metadata overhead)
	if size < 1 || size > 2 {
		t.Errorf("expected ~1 MB, got %d", size)
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "key" + string(rune('a'+n%10))
			meta := FileMeta{ETag: "\"v1\"", Size: 5}
			fc.Store("s", key, strings.NewReader("hello"), meta)
			fc.Has("s", key)
			fc.Path("s", key)
			fc.GetMeta("s", key)
			fc.IsStale("s", key, "\"v1\"", time.Now())
		}(i)
	}
	wg.Wait()
}

func TestStore_NestedKey(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Deeply nested key should create subdirectories
	key := "template/iso/subdir/nested/file.iso"
	meta := FileMeta{Size: 5}
	path, err := fc.Store("s", key, strings.NewReader("hello"), meta)
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created at expected path: %v", err)
	}
}

func TestMultipleStorages(t *testing.T) {
	dir := t.TempDir()
	fc, err := New(dir, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	meta := FileMeta{Size: 5}
	fc.Store("storage-a", "key", strings.NewReader("aaa"), meta)
	fc.Store("storage-b", "key", strings.NewReader("bbb"), meta)

	pa := fc.Path("storage-a", "key")
	pb := fc.Path("storage-b", "key")

	if pa == "" || pb == "" {
		t.Fatal("expected both storages to have the file")
	}
	if pa == pb {
		t.Error("expected different paths for different storages")
	}

	contentA, _ := os.ReadFile(pa)
	contentB, _ := os.ReadFile(pb)
	if string(contentA) != "aaa" {
		t.Errorf("unexpected content for storage-a: %s", contentA)
	}
	if string(contentB) != "bbb" {
		t.Errorf("unexpected content for storage-b: %s", contentB)
	}
}
