package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultSocketPath    = "/run/proxs3d.sock"
	DefaultCacheDir      = "/var/cache/proxs3"
	DefaultCacheMaxMB    = 4096
	DefaultCredentialDir = "/etc/pve/priv/proxs3"
	DefaultStorageCfg    = "/etc/pve/storage.cfg"
	DefaultHeadroomGB    = 100
)

// StorageConfig represents a discovered S3 storage from storage.cfg.
type StorageConfig struct {
	StorageID   string
	Bucket      string
	Endpoint    string
	Region      string
	UseSSL      bool
	PathStyle   bool
	AccessKey   string
	SecretKey   string
	CacheMaxAge int   // days; 0 = keep forever
	PartSizeMB  int64 // multipart upload part size in MB; 0 = default (64MB)
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
// Per-storage config is read from storage.cfg, not from here.
type DaemonConfig struct {
	SocketPath    string      `json:"socket_path"`
	CacheDir      string      `json:"cache_dir"`
	CacheMaxMB    int64       `json:"cache_max_mb"`
	CredentialDir string      `json:"credential_dir"`
	StorageCfg    string      `json:"storage_cfg"`
	HeadroomGB    int64       `json:"headroom_gb"`
	Proxy         ProxyConfig `json:"proxy"`

	// Populated at load time from storage.cfg + credential files
	Storages []StorageConfig `json:"-"`
}

func DefaultDaemonConfig() *DaemonConfig {
	return &DaemonConfig{
		SocketPath:    DefaultSocketPath,
		CacheDir:      DefaultCacheDir,
		CacheMaxMB:    DefaultCacheMaxMB,
		CredentialDir: DefaultCredentialDir,
		StorageCfg:    DefaultStorageCfg,
		HeadroomGB:    DefaultHeadroomGB,
	}
}

// LoadDaemonConfig reads the daemon config, then discovers S3 storages
// from storage.cfg and loads their credentials.
func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	cfg := DefaultDaemonConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := cfg.DiscoverStorages(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// DiscoverStorages reads storage.cfg to find all s3-type storages,
// then loads credentials for each from the credential dir.
func (cfg *DaemonConfig) DiscoverStorages() error {
	storages, err := ParseStorageCfg(cfg.StorageCfg)
	if err != nil {
		return fmt.Errorf("parsing storage.cfg: %w", err)
	}

	for i := range storages {
		cred, err := LoadCredential(cfg.CredentialDir, storages[i].StorageID)
		if err != nil {
			// Credentials are optional (public buckets) or may not exist yet
			log.Printf("Note: no credentials for %s: %v", storages[i].StorageID, err)
			continue
		}
		storages[i].AccessKey = cred.AccessKey
		storages[i].SecretKey = cred.SecretKey
	}

	cfg.Storages = storages
	return nil
}

// ParseStorageCfg reads a PVE storage.cfg file and extracts all
// sections with type "s3".
func ParseStorageCfg(path string) ([]StorageConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var storages []StorageConfig
	var current *StorageConfig

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Property lines start with whitespace; section headers don't
		isProperty := strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, " ")

		// New section: "type: name"
		if !isProperty && strings.Contains(line, ":") {
			// Save previous section if it was s3
			if current != nil {
				storages = append(storages, *current)
			}
			current = nil

			parts := strings.SplitN(line, ":", 2)
			stype := strings.TrimSpace(parts[0])
			sname := strings.TrimSpace(parts[1])

			if stype == "s3" {
				current = &StorageConfig{
					StorageID: sname,
					UseSSL:    true, // default
					Region:    "us-east-1",
				}
			}
			continue
		}

		// Property line within a section
		if isProperty && current != nil {
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])

			switch key {
			case "endpoint":
				// Strip protocol prefix and trailing slash if user included them
				val = strings.TrimPrefix(val, "https://")
				val = strings.TrimPrefix(val, "http://")
				val = strings.TrimRight(val, "/")
				current.Endpoint = val
			case "bucket":
				current.Bucket = val
			case "region":
				current.Region = val
			case "use-ssl":
				current.UseSSL = (val == "1" || val == "yes" || val == "true")
			case "path-style":
				current.PathStyle = (val == "1" || val == "yes" || val == "true")
			case "cache-max-age":
				if days, err := strconv.Atoi(val); err == nil && days >= 0 {
					current.CacheMaxAge = days
				}
			case "part-size-mb":
				if mb, err := strconv.ParseInt(val, 10, 64); err == nil && mb > 0 {
					current.PartSizeMB = mb
				}
			}
		}
	}

	// Don't forget the last section
	if current != nil {
		storages = append(storages, *current)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return storages, nil
}

// LoadCredential reads credentials from a per-storage JSON file.
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
	return &cred, nil
}
