package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDaemonConfig(t *testing.T) {
	cfg := DefaultDaemonConfig()
	if cfg.SocketPath != DefaultSocketPath {
		t.Errorf("expected socket path %s, got %s", DefaultSocketPath, cfg.SocketPath)
	}
	if cfg.CacheDir != DefaultCacheDir {
		t.Errorf("expected cache dir %s, got %s", DefaultCacheDir, cfg.CacheDir)
	}
	if cfg.CacheMaxMB != DefaultCacheMaxMB {
		t.Errorf("expected cache max %d, got %d", DefaultCacheMaxMB, cfg.CacheMaxMB)
	}
}

func TestLoadDaemonConfig(t *testing.T) {
	dir := t.TempDir()

	// Write a storage.cfg with one s3 storage and one non-s3 storage
	storageCfg := filepath.Join(dir, "storage.cfg")
	os.WriteFile(storageCfg, []byte(`dir: local
	path /var/lib/vz
	content iso,vztmpl,backup

s3: my-s3
	endpoint minio.local:9000
	bucket test-bucket
	region us-east-1
	use-ssl 0
	path-style 1

nfs: shared
	export /mnt/data
	path /mnt/pve/shared
	server 192.168.1.1
	content iso,vztmpl

s3: another-s3
	endpoint s3.amazonaws.com
	bucket prod-bucket
	use-ssl 1
`), 0644)

	// Write credentials
	credDir := filepath.Join(dir, "creds")
	os.MkdirAll(credDir, 0755)
	os.WriteFile(filepath.Join(credDir, "my-s3.json"), []byte(`{"access_key":"AK1","secret_key":"SK1"}`), 0600)
	os.WriteFile(filepath.Join(credDir, "another-s3.json"), []byte(`{"access_key":"AK2","secret_key":"SK2"}`), 0600)

	// Write daemon config
	daemonCfg := filepath.Join(dir, "proxs3d.json")
	os.WriteFile(daemonCfg, []byte(`{
		"socket_path": "/tmp/test.sock",
		"cache_dir": "`+filepath.Join(dir, "cache")+`",
		"cache_max_mb": 512,
		"credential_dir": "`+credDir+`",
		"storage_cfg": "`+storageCfg+`"
	}`), 0644)

	cfg, err := LoadDaemonConfig(daemonCfg)
	if err != nil {
		t.Fatalf("LoadDaemonConfig failed: %v", err)
	}

	if cfg.SocketPath != "/tmp/test.sock" {
		t.Errorf("unexpected socket path: %s", cfg.SocketPath)
	}
	if cfg.CacheMaxMB != 512 {
		t.Errorf("unexpected cache max: %d", cfg.CacheMaxMB)
	}
	if len(cfg.Storages) != 2 {
		t.Fatalf("expected 2 storages, got %d", len(cfg.Storages))
	}

	s1 := cfg.Storages[0]
	if s1.StorageID != "my-s3" {
		t.Errorf("expected storage ID 'my-s3', got '%s'", s1.StorageID)
	}
	if s1.Endpoint != "minio.local:9000" {
		t.Errorf("unexpected endpoint: %s", s1.Endpoint)
	}
	if s1.Bucket != "test-bucket" {
		t.Errorf("unexpected bucket: %s", s1.Bucket)
	}
	if s1.UseSSL {
		t.Error("expected use_ssl=false")
	}
	if !s1.PathStyle {
		t.Error("expected path_style=true")
	}
	if s1.AccessKey != "AK1" || s1.SecretKey != "SK1" {
		t.Error("credentials not loaded correctly for my-s3")
	}

	s2 := cfg.Storages[1]
	if s2.StorageID != "another-s3" {
		t.Errorf("expected storage ID 'another-s3', got '%s'", s2.StorageID)
	}
	if !s2.UseSSL {
		t.Error("expected use_ssl=true for another-s3")
	}
	if s2.AccessKey != "AK2" {
		t.Error("credentials not loaded correctly for another-s3")
	}
}

func TestParseStorageCfg_Empty(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(`dir: local
	path /var/lib/vz
`), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 0 {
		t.Errorf("expected 0 s3 storages, got %d", len(storages))
	}
}

func TestParseStorageCfg_Defaults(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(`s3: minimal
	endpoint s3.example.com
	bucket mybucket
`), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage, got %d", len(storages))
	}
	s := storages[0]
	if !s.UseSSL {
		t.Error("expected default use_ssl=true")
	}
	if s.Region != "us-east-1" {
		t.Errorf("expected default region us-east-1, got %s", s.Region)
	}
}

func TestLoadCredential(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "test.json"), []byte(`{"access_key":"AK","secret_key":"SK"}`), 0600)

	cred, err := LoadCredential(dir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.AccessKey != "AK" || cred.SecretKey != "SK" {
		t.Errorf("unexpected credentials: %+v", cred)
	}
}

func TestLoadCredential_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadCredential(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for missing credential file")
	}
}

func TestLoadCredential_Empty(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "empty.json"), []byte(`{"access_key":"","secret_key":""}`), 0600)
	cred, err := LoadCredential(dir, "empty")
	if err != nil {
		t.Errorf("empty credentials should be allowed (public buckets): %v", err)
	}
	if cred.AccessKey != "" || cred.SecretKey != "" {
		t.Error("expected empty credentials")
	}
}
