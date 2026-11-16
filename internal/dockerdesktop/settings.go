// Package dockerdesktop inspects Docker Desktop's local settings on Apple
// silicon Macs and reports configurations that make linux/amd64 containers slow
// or prone to out-of-memory kills.
package dockerdesktop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const MinRecommendedMemoryMiB = 8192

// Settings models the subset of Docker Desktop's settings-store.json that
// governs amd64 emulation and VM memory on Apple silicon.
type Settings struct {
	UseVirtualizationFramework        bool `json:"UseVirtualizationFramework"`
	UseVirtualizationFrameworkRosetta bool `json:"UseVirtualizationFrameworkRosetta"`
	MemoryMiB                         int  `json:"MemoryMiB"`
}

// LoadDefault reads Docker Desktop's settings-store.json on darwin/arm64. It
// returns nil when not on Apple silicon, when AVIARY_NO_DOCKER_WARNING is set,
// or when the settings file is absent or unreadable.
func LoadDefault() *Settings {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil
	}
	if os.Getenv("AVIARY_NO_DOCKER_WARNING") != "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, "Library", "Group Containers", "group.com.docker", "settings-store.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s, err := Parse(raw)
	if err != nil {
		return nil
	}
	return s
}

// Parse decodes a Docker Desktop settings-store.json payload.
func Parse(raw []byte) (*Settings, error) {
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Warnings returns human-readable configuration warnings when the stack uses
// linux/amd64 images on Apple silicon without Rosetta or sufficient memory.
func (s Settings) Warnings(hasLinuxAmd64 bool) []string {
	if !hasLinuxAmd64 {
		return nil
	}
	var out []string
	if !s.UseVirtualizationFramework {
		out = append(out, "Docker Desktop is not using Apple Virtualization framework, so linux/amd64 containers run under slow QEMU emulation instead of Rosetta")
	} else if !s.UseVirtualizationFrameworkRosetta {
		out = append(out, "Docker Desktop has Rosetta emulation turned off, so linux/amd64 containers run under slow QEMU emulation")
	}
	if s.MemoryMiB > 0 && s.MemoryMiB < MinRecommendedMemoryMiB {
		out = append(out, fmt.Sprintf("Docker Desktop has %d MiB memory allocated (< %d MiB); linux/amd64 stacks may OOM (exit 137)", s.MemoryMiB, MinRecommendedMemoryMiB))
	}
	return out
}

// ComposeFilesSpecifyLinuxAmd64 reports whether any compose file in files
// (resolved against definitionDir when relative) mentions linux/amd64.
func ComposeFilesSpecifyLinuxAmd64(definitionDir string, files []string) bool {
	needle := []byte("linux/amd64")
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(definitionDir, f)
		}
		raw, err := os.ReadFile(f)
		if err == nil && bytes.Contains(raw, needle) {
			return true
		}
	}
	return false
}
