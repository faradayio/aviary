// Package registry records every stack aviary has set up on this machine
// and the host ports each one owns, so worktrees never collide and stale
// stacks can be found after their worktree is gone.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

// Version is the registry file layout this build reads and writes. A file
// with any other version is refused rather than rewritten, so a newer aviary's
// state is never clobbered by an older one.
const Version = 1

// FirstPort is the bottom of the range aviary allocates from. It sits above
// the ports development stacks conventionally hardcode, so an aviary stack
// and a hand-started one can coexist on the same machine.
const FirstPort = 20000

// The range is cut into portSlots blocks of slotWidth ports; each project
// name hashes to the block its allocation starts at. It ends below 32768,
// where Linux's ephemeral range starts (macOS's starts at 49152), so an
// outbound connection never takes a port a stopped stack owns.
const (
	portSlots = 638
	slotWidth = 20
	lastPort  = FirstPort + portSlots*slotWidth - 1
)

// BasePort is where allocation for project starts: the first port of the
// block its name hashes to. FNV-1a is fixed by its spec, so the same project
// name gets the same base on every machine and with every Go release.
func BasePort(project string) int {
	h := fnv.New32a()
	h.Write([]byte(project))
	return FirstPort + int(h.Sum32()%portSlots)*slotWidth
}

type Entry struct {
	Project string `json:"project"`
	// Root is the worktree directory the stack runs against.
	Root string `json:"root"`
	// Config is the external definition directory the stack was loaded
	// from, or empty when aviary.yml lives in the worktree.
	Config    string         `json:"config"`
	Ports     map[string]int `json:"ports"`
	CreatedAt time.Time      `json:"created_at"`
	// InitializedAt is set once the stack's init steps have run since the
	// stack was created or since aviary last removed its volumes (`down
	// --volumes`, `compose down --volumes` or `rm`), and cleared by that
	// removal. Volumes removed any other way leave it set.
	InitializedAt *time.Time `json:"initialized_at,omitempty"`
}

type state struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Prober reports whether a host port can be handed to a new stack.
type Prober interface {
	Free(port int) bool
}

// ListenProber treats a port as free when aviary can bind it on every
// interface and on IPv4 and IPv6 loopback. The loopback binds matter on
// macOS, where a wildcard bind succeeds even while a local dev server holds
// the port on 127.0.0.1.
type ListenProber struct{}

func (ListenProber) Free(port int) bool {
	if !bindable(fmt.Sprintf(":%d", port)) {
		return false
	}
	// A host without IPv6 cannot bind [::1] at all; only an address
	// already in use counts against a loopback port.
	for _, addr := range []string{"127.0.0.1", "::1"} {
		if inUse(net.JoinHostPort(addr, fmt.Sprint(port))) {
			return false
		}
	}
	return true
}

func bindable(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func inUse(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Is(err, syscall.EADDRINUSE)
	}
	l.Close()
	return false
}

type Registry struct {
	path string
	// Prober checks ports nobody in the registry holds before they are
	// allocated. Open and OpenAt set it to ListenProber.
	Prober Prober
}

// Open locates the registry under the XDG state directory. It is created on
// first write. A relative XDG_STATE_HOME is ignored, as the XDG spec requires,
// so every worktree shares one registry instead of each getting its own.
func Open() (*Registry, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return OpenAt(filepath.Join(dir, "aviary", "registry.json")), nil
}

func OpenAt(path string) *Registry {
	return &Registry{path: path, Prober: ListenProber{}}
}

func (r *Registry) Path() string { return r.path }

// List returns every registered stack ordered by project name. It reads
// without taking the lock; writes replace the file by rename, so a reader
// never sees a partial one.
func (r *Registry) List() ([]Entry, error) {
	s, err := r.load()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(s.Entries))
	// load guarantees each key is its entry's project.
	for _, project := range slices.Sorted(maps.Keys(s.Entries)) {
		out = append(out, s.Entries[project])
	}
	return out, nil
}

// Get returns project's entry and whether it is registered. Like List, it
// reads without taking the lock, and it never writes.
func (r *Registry) Get(project string) (Entry, bool, error) {
	s, err := r.load()
	if err != nil {
		return Entry{}, false, err
	}
	entry, ok := s.Entries[project]
	return entry, ok, nil
}

// Allocate returns the entry for project, creating it or extending it so
// that every name in ports has a host port. Names that already have a port
// keep it; ports no longer named are released. A new port is the first free
// one from BasePort(project), so a project only lands somewhere else when
// another stack or process already holds those ports. config is the external
// definition directory, or empty for an in-tree aviary.yml.
func (r *Registry) Allocate(project, root, config string, ports []string) (Entry, error) {
	var entry Entry
	err := r.update(func(s *state) error {
		entry = s.Entries[project]
		if entry.Project == "" {
			entry = Entry{Project: project, Ports: map[string]int{}, CreatedAt: time.Now().UTC()}
		}
		if entry.Ports == nil {
			entry.Ports = map[string]int{}
		}
		entry.Root = root
		entry.Config = config
		wanted := map[string]bool{}
		for _, name := range ports {
			wanted[name] = true
		}
		for name := range entry.Ports {
			if !wanted[name] {
				delete(entry.Ports, name)
			}
		}
		// Store the entry before allocating so each new port counts as
		// taken when the next name is allocated.
		s.Entries[project] = entry
		for _, name := range ports {
			if _, ok := entry.Ports[name]; ok {
				continue
			}
			port, err := r.nextFreePort(s, BasePort(project))
			if err != nil {
				return err
			}
			entry.Ports[name] = port
		}
		return nil
	})
	return entry, err
}

// SetInitialized records whether project's init steps have run and returns
// the updated entry.
func (r *Registry) SetInitialized(project string, done bool) (Entry, error) {
	var entry Entry
	err := r.update(func(s *state) error {
		var ok bool
		entry, ok = s.Entries[project]
		if !ok {
			return fmt.Errorf("%s is not registered", project)
		}
		if done {
			now := time.Now().UTC()
			entry.InitializedAt = &now
		} else {
			entry.InitializedAt = nil
		}
		s.Entries[project] = entry
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Release forgets project and frees its ports. Releasing a project that is
// not registered is a no-op.
func (r *Registry) Release(project string) error {
	return r.update(func(s *state) error {
		delete(s.Entries, project)
		return nil
	})
}

// nextFreePort returns the first port from base upward, wrapping from
// lastPort to FirstPort, that no entry holds and the prober reports free.
func (r *Registry) nextFreePort(s *state, base int) (int, error) {
	taken := map[int]bool{}
	for _, e := range s.Entries {
		for _, p := range e.Ports {
			taken[p] = true
		}
	}
	span := lastPort - FirstPort + 1
	for i := range span {
		port := FirstPort + (base-FirstPort+i)%span
		if taken[port] || !r.Prober.Free(port) {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free port between %d and %d", FirstPort, lastPort)
}

func (r *Registry) load() (*state, error) {
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Version: Version, Entries: map[string]Entry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &state{}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path, err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("%s: registry version %d is not supported; this aviary reads version %d", r.path, s.Version, Version)
	}
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	// An entry keyed by one project but naming another (or none) would hand
	// callers an empty or wrong project name to take down or release.
	for _, key := range slices.Sorted(maps.Keys(s.Entries)) {
		if project := s.Entries[key].Project; project != key {
			return nil, fmt.Errorf("%s: entry %q has project %q; it must equal the key (fix or delete the entry)", r.path, key, project)
		}
	}
	return s, nil
}

// LockProject takes an exclusive lock for one project, held across work that
// reads and then updates its entry, such as running init steps, so two
// aviary processes in one worktree do not both do it. It blocks until the
// lock is free and returns the function that releases it.
func (r *Registry) LockProject(project string) (unlock func() error, err error) {
	dir := filepath.Join(filepath.Dir(r.path), "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, project+".lock"))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	return lock.Unlock, nil
}

// update applies fn to the current state under an exclusive lock on
// <path>.lock and replaces the file by writing <path>.tmp and renaming it.
// Nothing is written when fn fails.
func (r *Registry) update(fn func(*state) error) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	lock := flock.New(r.path + ".lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	defer lock.Unlock()

	s, err := r.load()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := writeSynced(tmp, raw); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(r.path))
}

// writeSynced writes path and flushes it to disk, so a crash after the
// rename cannot leave the registry empty or truncated.
func writeSynced(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir flushes dir so the rename itself survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
