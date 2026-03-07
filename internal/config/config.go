package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultSocketPath    = "/run/proxs3d.sock"
	DefaultCacheDir      = "/var/cache/proxs3"
	DefaultCacheMaxMB    = 4096
	DefaultCredentialDir = "/etc/pve/priv/proxs3"
)

// StorageConfig represents the configuration for a single S3-backed storage.
type StorageConfig struct {
	StorageID string `json:"storage_id"`
	Bucket    string `json:"bucket"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	UseSSL    bool   `json:"use_ssl"`
	PathStyle bool   `json:"path_style"`

	// Credentials are loaded separately from the credential dir,
	// not stored in this config (which may live in shared storage).
	AccessKey string `json:"-"`
	SecretKey string `json:"-"`
}

// Credential holds S3 access credentials, loaded from per-storage files.
type Credential struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// ProxyConfig holds HTTP proxy settings for outbound S3 connections.
type ProxyConfig struct {
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
}

// DaemonConfig is the top-level configuration for proxs3d.
type DaemonConfig struct {
	SocketPath    string          `json:"socket_path"`
	CacheDir      string          `json:"cache_dir"`
	CacheMaxMB    int64           `json:"cache_max_mb"`
	CredentialDir string          `json:"credential_dir"`
	Proxy         ProxyConfig     `json:"proxy"`
	Storages      []StorageConfig `json:"storages"`
}

func DefaultDaemonConfig() *DaemonConfig {
	return &DaemonConfig{
		SocketPath:    DefaultSocketPath,
		CacheDir:      DefaultCacheDir,
		CacheMaxMB:    DefaultCacheMaxMB,
		CredentialDir: DefaultCredentialDir,
	}
}

func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	cfg := DefaultDaemonConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Load credentials for each storage from the credential dir
	for i := range cfg.Storages {
		cred, err := LoadCredential(cfg.CredentialDir, cfg.Storages[i].StorageID)
		if err != nil {
			return nil, fmt.Errorf("loading credentials for %s: %w", cfg.Storages[i].StorageID, err)
		}
		cfg.Storages[i].AccessKey = cred.AccessKey
		cfg.Storages[i].SecretKey = cred.SecretKey
	}

	return cfg, nil
}

// LoadCredential reads credentials from a per-storage JSON file.
// File path: <credentialDir>/<storageID>.json
func LoadCredential(credentialDir, storageID string) (*Credential, error) {
	path := filepath.Join(credentialDir, storageID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading credential file %s: %w", path, err)
	}
	var cred Credential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, fmt.Errorf("parsing credential file %s: %w", path, err)
	}
	if cred.AccessKey == "" || cred.SecretKey == "" {
		return nil, fmt.Errorf("credential file %s: access_key and secret_key are required", path)
	}
	return &cred, nil
}
