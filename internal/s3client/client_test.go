package s3client

import (
	"testing"

	"github.com/sol1/proxs3/internal/config"
)

func TestNew_BasicConfig(t *testing.T) {
	cfg := config.StorageConfig{
		StorageID: "test-store",
		Endpoint:  "s3.amazonaws.com",
		Bucket:    "my-bucket",
		Region:    "us-east-1",
		UseSSL:    true,
		PathStyle: false,
		AccessKey: "AKID",
		SecretKey: "SECRET",
	}

	client, err := New(cfg, config.ProxyConfig{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if client.StorageID() != "test-store" {
		t.Errorf("expected storage ID 'test-store', got %q", client.StorageID())
	}
	if client.bucket != "my-bucket" {
		t.Errorf("expected bucket 'my-bucket', got %q", client.bucket)
	}
}

func TestNew_PathStyle(t *testing.T) {
	cfg := config.StorageConfig{
		StorageID: "minio",
		Endpoint:  "minio.local:9000",
		Bucket:    "test",
		Region:    "us-east-1",
		UseSSL:    false,
		PathStyle: true,
	}

	client, err := New(cfg, config.ProxyConfig{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if client.StorageID() != "minio" {
		t.Errorf("expected storage ID 'minio', got %q", client.StorageID())
	}
}

func TestNew_WithProxy(t *testing.T) {
	cfg := config.StorageConfig{
		StorageID: "proxied",
		Endpoint:  "s3.amazonaws.com",
		Bucket:    "test",
		Region:    "us-east-1",
		UseSSL:    true,
	}
	proxy := config.ProxyConfig{
		HTTPSProxy: "http://proxy:3128",
		HTTPProxy:  "http://proxy:3128",
	}

	client, err := New(cfg, proxy)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNew_NoSSL(t *testing.T) {
	cfg := config.StorageConfig{
		StorageID: "nossl",
		Endpoint:  "minio.local:9000",
		Bucket:    "test",
		Region:    "us-east-1",
		UseSSL:    false,
	}

	client, err := New(cfg, config.ProxyConfig{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNew_EmptyCredentials(t *testing.T) {
	// Public bucket - empty credentials should work
	cfg := config.StorageConfig{
		StorageID: "public",
		Endpoint:  "s3.amazonaws.com",
		Bucket:    "public-bucket",
		Region:    "us-east-1",
		UseSSL:    true,
		AccessKey: "",
		SecretKey: "",
	}

	client, err := New(cfg, config.ProxyConfig{})
	if err != nil {
		t.Fatalf("New failed with empty credentials: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestStorageID(t *testing.T) {
	cfg := config.StorageConfig{
		StorageID: "my-storage-id",
		Endpoint:  "e",
		Bucket:    "b",
		Region:    "r",
	}

	client, err := New(cfg, config.ProxyConfig{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if client.StorageID() != "my-storage-id" {
		t.Errorf("expected 'my-storage-id', got %q", client.StorageID())
	}
}
