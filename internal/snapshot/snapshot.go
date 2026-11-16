// Package snapshot caches seeded Docker volumes on disk keyed by their seed
// inputs and schema stamp, so new worktrees restore a seeded database in
// seconds instead of running init from scratch.
package snapshot

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
)

const (
	formatVersion = "aviary-snapshot-v1"
	defaultStamp  = "default"
	manifestFile  = "manifest.json"
)

// Volume describes one Compose-managed named volume covered by a snapshot.
type Volume struct {
	// Key is the logical volume name under the compose file's top-level
	// `volumes:` section (such as "postgres").
	Key string
	// DockerName is the project-prefixed Docker volume name (such as
	// "aviary-wt-abc123_postgres").
	DockerName string
	// Images lists every service image mounting Key, sorted.
	Images []string
	// ArchiveImage and ArchivePlatform identify the container image used to
	// stream tar archives out of and into DockerName.
	ArchiveImage    string
	ArchivePlatform string
}

// Identity uniquely identifies what a stack's seeded volumes hold (`Key`) and
// the schema version they are at (`Stamp`).
type Identity struct {
	Project string
	Key     string
	Stamp   string
	Volumes []Volume
}

// Hit describes a reusable snapshot found in the cache.
type Hit struct {
	Stamp string
	// Exact is true when Hit.Stamp equals Identity.Stamp, so no
	// after_restore steps are needed.
	Exact bool
}

// Archiver streams volume contents between Docker volumes and compressed tar
// archives on the host filesystem.
type Archiver interface {
	SaveVolume(vol Volume, archivePath string) error
	RestoreVolume(project string, vol Volume, archivePath string) error
}

// Store manages cached volume snapshots on disk.
type Store struct {
	Dir      string
	Archiver Archiver
	Now      func() time.Time
}

type manifest struct {
	Key       string    `json:"key"`
	Stamp     string    `json:"stamp"`
	Volumes   []string  `json:"volumes"`
	CreatedAt time.Time `json:"created_at"`
}

// DefaultDir returns $XDG_CACHE_HOME/aviary/snapshots, falling back to
// ~/.cache/aviary/snapshots when XDG_CACHE_HOME is unset or relative.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "aviary", "snapshots"), nil
}

// OpenDefault constructs a Store at DefaultDir backed by DockerArchiver.
func OpenDefault() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Archiver: DockerArchiver{}, Now: time.Now}, nil
}

// BuildIdentity computes a stack's snapshot Key, Stamp, and volume bindings.
func BuildIdentity(
	project, root string,
	snap *config.Snapshot,
	initSteps []config.Step,
	resolved *compose.Resolved,
) (*Identity, error) {
	names := slices.Clone(snap.Volumes)
	slices.Sort(names)
	names = slices.Compact(names)

	volumes := make([]Volume, 0, len(names))
	for _, key := range names {
		rv, ok := resolved.Volumes[key]
		if !ok || rv.Name == "" {
			return nil, fmt.Errorf("snapshot volume %q is not defined in compose volumes", key)
		}
		images := resolved.ImagesMounting(key)
		archiveImage, archivePlatform := resolved.ArchiveContainer(key)
		if archiveImage == "" {
			return nil, fmt.Errorf("snapshot volume %q is not mounted by any service with an image", key)
		}
		volumes = append(volumes, Volume{
			Key:             key,
			DockerName:      rv.Name,
			Images:          images,
			ArchiveImage:    archiveImage,
			ArchivePlatform: archivePlatform,
		})
	}

	keyHash := sha256.New()
	writeToken(keyHash, formatVersion)
	writeToken(keyHash, "volumes")
	for _, v := range volumes {
		writeToken(keyHash, v.Key)
		for _, img := range v.Images {
			writeToken(keyHash, img)
		}
		writeToken(keyHash, "")
	}
	writeToken(keyHash, "init")
	for _, step := range initSteps {
		writeToken(keyHash, step.Service)
		for _, arg := range step.Command {
			writeToken(keyHash, arg)
		}
		writeToken(keyHash, "")
	}
	writeToken(keyHash, "key_files")
	if err := hashPaths(keyHash, root, snap.KeyFiles); err != nil {
		return nil, fmt.Errorf("snapshot key_files: %w", err)
	}

	stamp := defaultStamp
	if len(snap.StampFiles) > 0 {
		stampHash := sha256.New()
		writeToken(stampHash, formatVersion)
		if err := hashPaths(stampHash, root, snap.StampFiles); err != nil {
			return nil, fmt.Errorf("snapshot stamp_files: %w", err)
		}
		stamp = hex.EncodeToString(stampHash.Sum(nil))[:16]
	}

	return &Identity{
		Project: project,
		Key:     hex.EncodeToString(keyHash.Sum(nil))[:16],
		Stamp:   stamp,
		Volumes: volumes,
	}, nil
}

func writeToken(h hash.Hash, s string) {
	_, _ = fmt.Fprintf(h, "%d:%s\n", len(s), s)
}

func hashPaths(h hash.Hash, root string, relPaths []string) error {
	sorted := slices.Clone(relPaths)
	slices.Sort(sorted)
	for _, rel := range sorted {
		cleanRel := filepath.ToSlash(filepath.Clean(rel))
		full := filepath.Join(root, filepath.FromSlash(cleanRel))
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("%s: %w", cleanRel, err)
		}
		if !info.IsDir() {
			if err := hashEntry(h, cleanRel, full, info); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(full, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			entryRel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			return hashEntry(h, filepath.ToSlash(entryRel), p, fi)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func hashEntry(h hash.Hash, slashRel, full string, info fs.FileInfo) error {
	writeToken(h, slashRel)
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return err
		}
		writeToken(h, "symlink:"+target)
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	writeToken(h, fmt.Sprintf("file:%d", info.Size()))
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	_, _ = h.Write([]byte("\n"))
	return nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) lockKey(key string) (func() error, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(s.Dir, key+".lock"))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	return lock.Unlock, nil
}

func (s *Store) stampDir(key, stamp string) string {
	return filepath.Join(s.Dir, key, stamp)
}

func (s *Store) archivePath(key, stamp, volumeKey string) string {
	return filepath.Join(s.stampDir(key, stamp), volumeKey+".tar.gz")
}

// Lookup finds a cached snapshot for id. An exact (Key, Stamp) match wins; when
// none exists and allowDifferentStamp is true, the newest valid snapshot under
// id.Key is returned with Exact=false so the caller can run after_restore.
func (s *Store) Lookup(id *Identity, allowDifferentStamp bool) (*Hit, error) {
	unlock, err := s.lockKey(id.Key)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if _, ok := s.readValidManifest(id, id.Stamp); ok {
		return &Hit{Stamp: id.Stamp, Exact: true}, nil
	}
	if !allowDifferentStamp {
		return nil, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, id.Key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var best *manifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		m, ok := s.readValidManifest(id, entry.Name())
		if !ok {
			continue
		}
		if best == nil || m.CreatedAt.After(best.CreatedAt) {
			mCopy := m
			best = &mCopy
		}
	}
	if best == nil {
		return nil, nil
	}
	return &Hit{Stamp: best.Stamp, Exact: false}, nil
}

func (s *Store) readValidManifest(id *Identity, stamp string) (manifest, bool) {
	dir := s.stampDir(id.Key, stamp)
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return manifest{}, false
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, false
	}
	if m.Key != id.Key || m.Stamp != stamp {
		return manifest{}, false
	}
	for _, v := range id.Volumes {
		if !slices.Contains(m.Volumes, v.Key) {
			return manifest{}, false
		}
		if fi, err := os.Stat(s.archivePath(id.Key, stamp, v.Key)); err != nil || fi.IsDir() {
			return manifest{}, false
		}
	}
	return m, true
}

// Restore populates each of id's Docker volumes from the snapshot at stamp.
func (s *Store) Restore(id *Identity, stamp string) error {
	unlock, err := s.lockKey(id.Key)
	if err != nil {
		return err
	}
	defer unlock()

	for _, vol := range id.Volumes {
		archive := s.archivePath(id.Key, stamp, vol.Key)
		if err := s.Archiver.RestoreVolume(id.Project, vol, archive); err != nil {
			return fmt.Errorf("restoring volume %s: %w", vol.Key, err)
		}
	}
	return nil
}

// Save archives each of id's Docker volumes under (id.Key, id.Stamp) and writes
// manifest.json atomically once every archive is on disk.
func (s *Store) Save(id *Identity) error {
	unlock, err := s.lockKey(id.Key)
	if err != nil {
		return err
	}
	defer unlock()

	dir := s.stampDir(id.Key, id.Stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, manifestFile)
	_ = os.Remove(manifestPath)

	volKeys := make([]string, 0, len(id.Volumes))
	for _, vol := range id.Volumes {
		archive := s.archivePath(id.Key, id.Stamp, vol.Key)
		if err := s.Archiver.SaveVolume(vol, archive); err != nil {
			return fmt.Errorf("saving volume %s: %w", vol.Key, err)
		}
		volKeys = append(volKeys, vol.Key)
	}

	raw, err := json.MarshalIndent(manifest{
		Key:       id.Key,
		Stamp:     id.Stamp,
		Volumes:   volKeys,
		CreatedAt: s.now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(manifestPath, raw)
}

func writeAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// DockerArchiver streams tar archives to and from Docker volumes using a
// one-off container of the service image that mounts the volume, compressing
// with gzip on the host.
type DockerArchiver struct{}

func (DockerArchiver) SaveVolume(vol Volume, archivePath string) error {
	tmp := archivePath + ".tmp"
	outFile, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() {
		outFile.Close()
		_ = os.Remove(tmp)
	}

	gz, err := gzip.NewWriterLevel(outFile, gzip.BestSpeed)
	if err != nil {
		cleanup()
		return err
	}

	args := []string{"run", "--rm"}
	if vol.ArchivePlatform != "" {
		args = append(args, "--platform", vol.ArchivePlatform)
	}
	args = append(args,
		"--user", "0:0",
		"--entrypoint", "tar",
		"-v", vol.DockerName+":/volume:ro",
		vol.ArchiveImage,
		"-cf", "-", "-C", "/volume", ".",
	)
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = gz
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return fmt.Errorf("docker run tar: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return err
	}
	if err := outFile.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := outFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, archivePath)
}

func (DockerArchiver) RestoreVolume(project string, vol Volume, archivePath string) error {
	if err := ensureComposeVolume(project, vol); err != nil {
		return err
	}
	inFile, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer inFile.Close()

	gz, err := gzip.NewReader(inFile)
	if err != nil {
		return err
	}
	defer gz.Close()

	args := []string{"run", "--rm", "-i"}
	if vol.ArchivePlatform != "" {
		args = append(args, "--platform", vol.ArchivePlatform)
	}
	args = append(args,
		"--user", "0:0",
		"--entrypoint", "sh",
		"-v", vol.DockerName+":/volume",
		vol.ArchiveImage,
		"-c", "find /volume -mindepth 1 -delete && exec tar -xpf - -C /volume",
	)
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdin = gz
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker run untar: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ensureComposeVolume creates vol.DockerName with Docker Compose's ownership
// labels when it does not exist yet, so `docker compose up` adopts it without
// warning and `aviary rm` / `aviary gc` removes it by project label.
func ensureComposeVolume(project string, vol Volume) error {
	if err := exec.Command("docker", "volume", "inspect", vol.DockerName).Run(); err == nil {
		return nil
	}
	var stderr bytes.Buffer
	cmd := exec.Command("docker", "volume", "create",
		"--label", "com.docker.compose.project="+project,
		"--label", "com.docker.compose.volume="+vol.Key,
		vol.DockerName,
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker volume create %s: %w (%s)", vol.DockerName, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
