package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
)

type memoryArchiver struct {
	volumes map[string]string
}

func newMemoryArchiver() *memoryArchiver {
	return &memoryArchiver{volumes: map[string]string{}}
}

func (m *memoryArchiver) SaveVolume(vol Volume, archivePath string) error {
	return os.WriteFile(archivePath, []byte(m.volumes[vol.DockerName]), 0o600)
}

func (m *memoryArchiver) RestoreVolume(project string, vol Volume, archivePath string) error {
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	m.volumes[vol.DockerName] = string(raw)
	return nil
}

func resolvedFixture(project, image string) *compose.Resolved {
	return &compose.Resolved{
		Services: map[string]compose.ResolvedService{
			"postgres": {
				Image:    image,
				Platform: "linux/amd64",
				Volumes:  []compose.ResolvedMount{{Type: "volume", Source: "postgres"}},
			},
		},
		Volumes: map[string]compose.ResolvedVolume{
			"postgres": {Name: project + "_postgres"},
		},
	}
}

func TestBuildIdentityChangesOnSeedImageInitOrStampChanges(t *testing.T) {
	root := t.TempDir()
	seedDir := filepath.Join(root, "seeds")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedFile := filepath.Join(seedDir, "users.sql")
	schemaFile := filepath.Join(root, "structure.sql")
	if err := os.WriteFile(seedFile, []byte("INSERT INTO users VALUES (1);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE users (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &config.Snapshot{
		Volumes:    []string{"postgres"},
		KeyFiles:   []string{"seeds"},
		StampFiles: []string{"structure.sql"},
	}
	steps := []config.Step{{Service: "db", Command: []string{"psql", "-f", "seeds/users.sql"}}}

	base, err := BuildIdentity("aviary-a", root, snap, steps, resolvedFixture("aviary-a", "postgres:17"))
	if err != nil {
		t.Fatal(err)
	}
	otherWorktree, err := BuildIdentity("aviary-b", root, snap, steps, resolvedFixture("aviary-b", "postgres:17"))
	if err != nil {
		t.Fatal(err)
	}
	if base.Key != otherWorktree.Key || base.Stamp != otherWorktree.Stamp {
		t.Fatalf("two projects with identical files produced different keys/stamps: %+v vs %+v", base, otherWorktree)
	}

	// Schema change alters Stamp, keeping Key intact so after_restore can run.
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE users (id int, name text);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	migrated, err := BuildIdentity("aviary-a", root, snap, steps, resolvedFixture("aviary-a", "postgres:17"))
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Key != base.Key || migrated.Stamp == base.Stamp {
		t.Fatalf("schema change: key %s (was %s), stamp %s (was %s)", migrated.Key, base.Key, migrated.Stamp, base.Stamp)
	}

	// Image upgrade alters Key.
	newImage, err := BuildIdentity("aviary-a", root, snap, steps, resolvedFixture("aviary-a", "postgres:18"))
	if err != nil {
		t.Fatal(err)
	}
	if newImage.Key == base.Key {
		t.Fatal("mounting image change did not change Key")
	}

	// Seed file edit alters Key.
	if err := os.WriteFile(seedFile, []byte("INSERT INTO users VALUES (1), (2);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newSeed, err := BuildIdentity("aviary-a", root, snap, steps, resolvedFixture("aviary-a", "postgres:17"))
	if err != nil {
		t.Fatal(err)
	}
	if newSeed.Key == base.Key {
		t.Fatal("seed file change did not change Key")
	}
}

func TestBuildIdentityRejectsUnknownOrUnmountedVolume(t *testing.T) {
	root := t.TempDir()
	_, err := BuildIdentity("aviary-a", root, &config.Snapshot{Volumes: []string{"missing"}}, nil, resolvedFixture("aviary-a", "postgres:17"))
	if err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("expected missing volume error, got %v", err)
	}
}

func TestStoreSaveLookupAndRestore(t *testing.T) {
	arch := newMemoryArchiver()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:      t.TempDir(),
		Archiver: arch,
		Now:      func() time.Time { return now },
	}

	idV1 := &Identity{
		Project: "aviary-one",
		Key:     "seedkey1",
		Stamp:   "stamp_v1",
		Volumes: []Volume{{Key: "postgres", DockerName: "aviary-one_postgres", ArchiveImage: "postgres:17"}},
	}
	arch.volumes["aviary-one_postgres"] = "seeded-rows-v1"
	if err := store.Save(idV1); err != nil {
		t.Fatal(err)
	}

	// Exact hit in a second worktree on the same schema.
	wt2V1 := &Identity{
		Project: "aviary-two",
		Key:     "seedkey1",
		Stamp:   "stamp_v1",
		Volumes: []Volume{{Key: "postgres", DockerName: "aviary-two_postgres", ArchiveImage: "postgres:17"}},
	}
	hit, err := store.Lookup(wt2V1, false)
	if err != nil || hit == nil || !hit.Exact || hit.Stamp != "stamp_v1" {
		t.Fatalf("Lookup exact = %+v, %v", hit, err)
	}
	if err := store.Restore(wt2V1, hit.Stamp); err != nil {
		t.Fatal(err)
	}
	if got := arch.volumes["aviary-two_postgres"]; got != "seeded-rows-v1" {
		t.Fatalf("restored volume = %q, want seeded-rows-v1", got)
	}

	// Newer schema misses without after_restore, hits newest stamp with after_restore.
	now = now.Add(time.Minute)
	idV2 := &Identity{
		Project: "aviary-one",
		Key:     "seedkey1",
		Stamp:   "stamp_v2",
		Volumes: []Volume{{Key: "postgres", DockerName: "aviary-one_postgres", ArchiveImage: "postgres:17"}},
	}
	arch.volumes["aviary-one_postgres"] = "seeded-rows-v2"
	if err := store.Save(idV2); err != nil {
		t.Fatal(err)
	}

	wt3V3 := &Identity{
		Project: "aviary-three",
		Key:     "seedkey1",
		Stamp:   "stamp_v3",
		Volumes: []Volume{{Key: "postgres", DockerName: "aviary-three_postgres", ArchiveImage: "postgres:17"}},
	}
	if miss, err := store.Lookup(wt3V3, false); err != nil || miss != nil {
		t.Fatalf("Lookup without after_restore = %+v, %v; want nil", miss, err)
	}
	hit, err = store.Lookup(wt3V3, true)
	if err != nil || hit == nil || hit.Exact || hit.Stamp != "stamp_v2" {
		t.Fatalf("Lookup with after_restore = %+v, %v; want non-exact stamp_v2", hit, err)
	}
}
