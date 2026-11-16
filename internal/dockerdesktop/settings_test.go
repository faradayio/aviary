package dockerdesktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsWarnings(t *testing.T) {
	healthy := Settings{
		UseVirtualizationFramework:        true,
		UseVirtualizationFrameworkRosetta: true,
		MemoryMiB:                         8192,
	}
	if got := healthy.Warnings(true); len(got) != 0 {
		t.Errorf("healthy settings produced warnings: %v", got)
	}

	dockerVMMAndLowMem, err := Parse([]byte(`{
		"UseVirtualizationFramework": false,
		"UseVirtualizationFrameworkRosetta": true,
		"MemoryMiB": 6144
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := dockerVMMAndLowMem.Warnings(false); len(got) != 0 {
		t.Errorf("stack without linux/amd64 produced warnings: %v", got)
	}
	got := dockerVMMAndLowMem.Warnings(true)
	if len(got) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "Apple Virtualization framework") {
		t.Errorf("warning[0] = %q", got[0])
	}
	if !strings.Contains(got[1], "6144 MiB") {
		t.Errorf("warning[1] = %q", got[1])
	}

	rosettaOff := Settings{UseVirtualizationFramework: true, UseVirtualizationFrameworkRosetta: false, MemoryMiB: 12288}
	if w := rosettaOff.Warnings(true); len(w) != 1 || !strings.Contains(w[0], "Rosetta") {
		t.Errorf("rosetta off warnings = %v", w)
	}
}

func TestComposeFilesSpecifyLinuxAmd64(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ComposeFilesSpecifyLinuxAmd64(dir, []string{"a.yaml"}) {
		t.Error("reported linux/amd64 when absent")
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("x-fdy:\n  platform: linux/amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ComposeFilesSpecifyLinuxAmd64(dir, []string{"a.yaml", "b.yaml"}) {
		t.Error("missed linux/amd64 in b.yaml")
	}
}
