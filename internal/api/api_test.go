package api

import (
	"testing"
)

func TestContentToPrefix(t *testing.T) {
	tests := []struct {
		content  string
		expected string
	}{
		{"iso", "template/iso/"},
		{"vztmpl", "template/cache/"},
		{"snippets", "snippets/"},
		{"backup", "dump/"},
		{"import", "images/"},
		{"unknown", "unknown/"},
		{"", "/"},
	}
	for _, tt := range tests {
		got := contentToPrefix(tt.content)
		if got != tt.expected {
			t.Errorf("contentToPrefix(%q) = %q, want %q", tt.content, got, tt.expected)
		}
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		key      string
		expected string
	}{
		// ISO images
		{"template/iso/debian-12.iso", "iso"},
		{"template/iso/UBUNTU.ISO", "iso"},
		{"template/iso/file.ISO", "iso"},

		// Compressed archives (container templates)
		{"template/cache/ubuntu-22.04-standard_22.04-1_amd64.tar.gz", "tgz"},
		{"template/cache/alpine.tar.xz", "tgz"},
		{"template/cache/debian.tar.zst", "tgz"},
		{"template/cache/TEMPLATE.TAR.GZ", "tgz"},

		// Raw/other formats
		{"snippets/cloud-init.yaml", "raw"},
		{"dump/vzdump-qemu-100-2024_01_01.vma", "raw"},
		{"dump/vzdump-qemu-100.vma.zst", "raw"},
		{"images/disk-0.raw", "raw"},
		{"images/disk-0.qcow2", "raw"},
		{"some/random/file.txt", "raw"},
		{"noextension", "raw"},
	}
	for _, tt := range tests {
		got := detectFormat(tt.key)
		if got != tt.expected {
			t.Errorf("detectFormat(%q) = %q, want %q", tt.key, got, tt.expected)
		}
	}
}

func TestPrefixDirs(t *testing.T) {
	// Verify the watcher's prefixDirs map covers all expected directories
	expected := map[string]string{
		"template/iso":   "template/iso/",
		"template/cache": "template/cache/",
		"snippets":       "snippets/",
		"dump":           "dump/",
		"images":         "images/",
	}
	for key, val := range expected {
		got, ok := prefixDirs[key]
		if !ok {
			t.Errorf("prefixDirs missing key %q", key)
			continue
		}
		if got != val {
			t.Errorf("prefixDirs[%q] = %q, want %q", key, got, val)
		}
	}
	if len(prefixDirs) != len(expected) {
		t.Errorf("prefixDirs has %d entries, expected %d", len(prefixDirs), len(expected))
	}
}

func TestContentToPrefixRoundTrip(t *testing.T) {
	// Verify that each content type maps to a prefix that differs
	// from the naive default of "content/"
	// Note: "snippets" maps to "snippets/" which happens to match the default
	// pattern, so we only check the types where the mapping is non-obvious.
	nonObvious := map[string]string{
		"iso":    "template/iso/",
		"vztmpl": "template/cache/",
		"backup": "dump/",
		"import": "images/",
	}
	for ct, expected := range nonObvious {
		prefix := contentToPrefix(ct)
		if prefix != expected {
			t.Errorf("contentToPrefix(%q) = %q, want %q", ct, prefix, expected)
		}
	}
}
