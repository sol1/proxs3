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
	if cfg.CredentialDir != DefaultCredentialDir {
		t.Errorf("expected credential dir %s, got %s", DefaultCredentialDir, cfg.CredentialDir)
	}
	if cfg.StorageCfg != DefaultStorageCfg {
		t.Errorf("expected storage cfg %s, got %s", DefaultStorageCfg, cfg.StorageCfg)
	}
	if cfg.HeadroomGB != DefaultHeadroomGB {
		t.Errorf("expected headroom %d, got %d", DefaultHeadroomGB, cfg.HeadroomGB)
	}
}

func TestLoadDaemonConfig(t *testing.T) {
	dir := t.TempDir()

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

	credDir := filepath.Join(dir, "creds")
	os.MkdirAll(credDir, 0755)
	os.WriteFile(filepath.Join(credDir, "my-s3.json"), []byte(`{"access_key":"AK1","secret_key":"SK1"}`), 0600)
	os.WriteFile(filepath.Join(credDir, "another-s3.json"), []byte(`{"access_key":"AK2","secret_key":"SK2"}`), 0600)

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

func TestLoadDaemonConfig_Defaults(t *testing.T) {
	dir := t.TempDir()

	storageCfg := filepath.Join(dir, "storage.cfg")
	os.WriteFile(storageCfg, []byte(""), 0644)

	daemonCfg := filepath.Join(dir, "proxs3d.json")
	os.WriteFile(daemonCfg, []byte(`{
		"storage_cfg": "`+storageCfg+`",
		"credential_dir": "`+dir+`"
	}`), 0644)

	cfg, err := LoadDaemonConfig(daemonCfg)
	if err != nil {
		t.Fatalf("LoadDaemonConfig failed: %v", err)
	}

	// Unset fields should get defaults
	if cfg.SocketPath != DefaultSocketPath {
		t.Errorf("expected default socket path, got %s", cfg.SocketPath)
	}
	if cfg.CacheDir != DefaultCacheDir {
		t.Errorf("expected default cache dir, got %s", cfg.CacheDir)
	}
	if cfg.CacheMaxMB != DefaultCacheMaxMB {
		t.Errorf("expected default cache max, got %d", cfg.CacheMaxMB)
	}
	if cfg.HeadroomGB != DefaultHeadroomGB {
		t.Errorf("expected default headroom %d, got %d", DefaultHeadroomGB, cfg.HeadroomGB)
	}
}

func TestLoadDaemonConfig_CustomHeadroom(t *testing.T) {
	dir := t.TempDir()

	storageCfg := filepath.Join(dir, "storage.cfg")
	os.WriteFile(storageCfg, []byte(""), 0644)

	daemonCfg := filepath.Join(dir, "proxs3d.json")
	os.WriteFile(daemonCfg, []byte(`{
		"storage_cfg": "`+storageCfg+`",
		"credential_dir": "`+dir+`",
		"headroom_gb": 500
	}`), 0644)

	cfg, err := LoadDaemonConfig(daemonCfg)
	if err != nil {
		t.Fatalf("LoadDaemonConfig failed: %v", err)
	}

	if cfg.HeadroomGB != 500 {
		t.Errorf("expected headroom 500, got %d", cfg.HeadroomGB)
	}
}

func TestLoadDaemonConfig_ProxySettings(t *testing.T) {
	dir := t.TempDir()

	storageCfg := filepath.Join(dir, "storage.cfg")
	os.WriteFile(storageCfg, []byte(""), 0644)

	daemonCfg := filepath.Join(dir, "proxs3d.json")
	os.WriteFile(daemonCfg, []byte(`{
		"storage_cfg": "`+storageCfg+`",
		"credential_dir": "`+dir+`",
		"proxy": {
			"http_proxy": "http://proxy:3128",
			"https_proxy": "http://proxy:3128",
			"no_proxy": "localhost,127.0.0.1"
		}
	}`), 0644)

	cfg, err := LoadDaemonConfig(daemonCfg)
	if err != nil {
		t.Fatalf("LoadDaemonConfig failed: %v", err)
	}

	if cfg.Proxy.HTTPProxy != "http://proxy:3128" {
		t.Errorf("unexpected http proxy: %s", cfg.Proxy.HTTPProxy)
	}
	if cfg.Proxy.HTTPSProxy != "http://proxy:3128" {
		t.Errorf("unexpected https proxy: %s", cfg.Proxy.HTTPSProxy)
	}
	if cfg.Proxy.NoProxy != "localhost,127.0.0.1" {
		t.Errorf("unexpected no proxy: %s", cfg.Proxy.NoProxy)
	}
}

func TestLoadDaemonConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	daemonCfg := filepath.Join(dir, "proxs3d.json")
	os.WriteFile(daemonCfg, []byte(`{invalid json`), 0644)

	_, err := LoadDaemonConfig(daemonCfg)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadDaemonConfig_MissingFile(t *testing.T) {
	_, err := LoadDaemonConfig("/nonexistent/path/proxs3d.json")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

// --- ParseStorageCfg tests ---

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
	if s.PathStyle {
		t.Error("expected default path_style=false")
	}
	if s.CacheMaxAge != 0 {
		t.Errorf("expected default cache-max-age 0, got %d", s.CacheMaxAge)
	}
}

func TestParseStorageCfg_EndpointSanitisation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"plain hostname", "s3.amazonaws.com", "s3.amazonaws.com"},
		{"with https prefix", "https://s3.amazonaws.com", "s3.amazonaws.com"},
		{"with http prefix", "http://minio.local:9000", "minio.local:9000"},
		{"with trailing slash", "s3.amazonaws.com/", "s3.amazonaws.com"},
		{"with https and trailing slash", "https://s3.amazonaws.com/", "s3.amazonaws.com"},
		{"hostname with port", "minio.local:9000", "minio.local:9000"},
		{"multiple trailing slashes", "s3.amazonaws.com///", "s3.amazonaws.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "storage.cfg")
			os.WriteFile(f, []byte("s3: test\n\tendpoint "+tt.input+"\n\tbucket b\n"), 0644)

			storages, err := ParseStorageCfg(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(storages) != 1 {
				t.Fatalf("expected 1 storage, got %d", len(storages))
			}
			if storages[0].Endpoint != tt.expected {
				t.Errorf("expected endpoint %q, got %q", tt.expected, storages[0].Endpoint)
			}
		})
	}
}

func TestParseStorageCfg_CacheMaxAge(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected int
	}{
		{"zero", "0", 0},
		{"seven days", "7", 7},
		{"thirty days", "30", 30},
		{"negative ignored", "-1", 0},
		{"non-numeric ignored", "abc", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "storage.cfg")
			os.WriteFile(f, []byte("s3: test\n\tendpoint s3.example.com\n\tbucket b\n\tcache-max-age "+tt.value+"\n"), 0644)

			storages, err := ParseStorageCfg(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if storages[0].CacheMaxAge != tt.expected {
				t.Errorf("expected cache-max-age %d, got %d", tt.expected, storages[0].CacheMaxAge)
			}
		})
	}
}

func TestParseStorageCfg_UseSSLValues(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"1", true},
		{"yes", true},
		{"true", true},
		{"0", false},
		{"no", false},
		{"false", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "storage.cfg")
			os.WriteFile(f, []byte("s3: test\n\tendpoint e\n\tbucket b\n\tuse-ssl "+tt.value+"\n"), 0644)

			storages, err := ParseStorageCfg(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if storages[0].UseSSL != tt.expected {
				t.Errorf("use-ssl %q: expected %v, got %v", tt.value, tt.expected, storages[0].UseSSL)
			}
		})
	}
}

func TestParseStorageCfg_PathStyleValues(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"1", true},
		{"yes", true},
		{"true", true},
		{"0", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "storage.cfg")
			os.WriteFile(f, []byte("s3: test\n\tendpoint e\n\tbucket b\n\tpath-style "+tt.value+"\n"), 0644)

			storages, err := ParseStorageCfg(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if storages[0].PathStyle != tt.expected {
				t.Errorf("path-style %q: expected %v, got %v", tt.value, tt.expected, storages[0].PathStyle)
			}
		})
	}
}

func TestParseStorageCfg_Comments(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(`# This is a comment
s3: test
	endpoint s3.example.com
	# This is also a comment
	bucket mybucket
	region eu-west-1
`), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage, got %d", len(storages))
	}
	if storages[0].Region != "eu-west-1" {
		t.Errorf("expected region eu-west-1, got %s", storages[0].Region)
	}
}

func TestParseStorageCfg_MultipleStorageTypes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(`dir: local
	path /var/lib/vz
	content iso,vztmpl,backup

nfs: shared
	export /mnt/data
	path /mnt/pve/shared
	server 192.168.1.1

s3: first-s3
	endpoint s3.amazonaws.com
	bucket bucket1

lvmthin: local-lvm
	thinpool data
	vgname pve

s3: second-s3
	endpoint minio.local:9000
	bucket bucket2
	path-style 1

zfspool: local-zfs
	pool rpool/data
`), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 2 {
		t.Fatalf("expected 2 s3 storages, got %d", len(storages))
	}
	if storages[0].StorageID != "first-s3" {
		t.Errorf("expected first-s3, got %s", storages[0].StorageID)
	}
	if storages[1].StorageID != "second-s3" {
		t.Errorf("expected second-s3, got %s", storages[1].StorageID)
	}
	if !storages[1].PathStyle {
		t.Error("expected path-style for second-s3")
	}
}

func TestParseStorageCfg_LastSectionIncluded(t *testing.T) {
	// Ensure the last section in the file is captured (no trailing newline)
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte("s3: last\n\tendpoint e\n\tbucket b"), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage, got %d", len(storages))
	}
	if storages[0].StorageID != "last" {
		t.Errorf("expected 'last', got %s", storages[0].StorageID)
	}
}

func TestParseStorageCfg_MissingFile(t *testing.T) {
	_, err := ParseStorageCfg("/nonexistent/storage.cfg")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseStorageCfg_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(""), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storages) != 0 {
		t.Errorf("expected 0 storages, got %d", len(storages))
	}
}

func TestParseStorageCfg_AllFields(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "storage.cfg")
	os.WriteFile(f, []byte(`s3: full
	endpoint s3.ap-southeast-2.amazonaws.com
	bucket my-bucket
	region ap-southeast-2
	use-ssl 1
	path-style 0
	cache-max-age 14
`), 0644)

	storages, err := ParseStorageCfg(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := storages[0]
	if s.StorageID != "full" {
		t.Errorf("unexpected storage ID: %s", s.StorageID)
	}
	if s.Endpoint != "s3.ap-southeast-2.amazonaws.com" {
		t.Errorf("unexpected endpoint: %s", s.Endpoint)
	}
	if s.Bucket != "my-bucket" {
		t.Errorf("unexpected bucket: %s", s.Bucket)
	}
	if s.Region != "ap-southeast-2" {
		t.Errorf("unexpected region: %s", s.Region)
	}
	if !s.UseSSL {
		t.Error("expected use-ssl true")
	}
	if s.PathStyle {
		t.Error("expected path-style false")
	}
	if s.CacheMaxAge != 14 {
		t.Errorf("expected cache-max-age 14, got %d", s.CacheMaxAge)
	}
}

// --- Credential tests ---

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

func TestLoadCredential_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`not json`), 0600)
	_, err := LoadCredential(dir, "bad")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- DiscoverStorages tests ---

func TestDiscoverStorages_MissingCredentials(t *testing.T) {
	dir := t.TempDir()
	storageCfg := filepath.Join(dir, "storage.cfg")
	os.WriteFile(storageCfg, []byte("s3: nocreds\n\tendpoint e\n\tbucket b\n"), 0644)

	cfg := DefaultDaemonConfig()
	cfg.StorageCfg = storageCfg
	cfg.CredentialDir = filepath.Join(dir, "no-such-dir")

	err := cfg.DiscoverStorages()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Storage should still be discovered, just without credentials
	if len(cfg.Storages) != 1 {
		t.Fatalf("expected 1 storage, got %d", len(cfg.Storages))
	}
	if cfg.Storages[0].AccessKey != "" {
		t.Error("expected empty access key for missing credentials")
	}
}
