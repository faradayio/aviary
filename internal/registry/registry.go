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

// Version is the registry file layout this build writes. A newer version is
// refused rather than rewritten, so an older aviary cannot drop fields it
// does not know. Version 1, which aviary 0.2.0 wrote, is still read; the next
// write saves Version and a 0.2.0 aviary then refuses the file.
const Version = 2

// DefaultIdleTTL is how long a warm stack stays running after the last
// transient command (`test`, `init`, `run`) finishes when the stack entry
// carries no explicit idle_ttl.
const DefaultIdleTTL = 20 * time.Minute

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
	// Held is true when the user brought the stack up explicitly (`aviary
	// up` or `aviary compose up`) and has not stopped it (`down`, `stop`
	// or `rm`). Held stacks are never auto-stopped for idleness.
	Held bool `json:"held,omitempty"`
	// LastUsedAt is when a transient command (`test`, `init`, `run`) last
	// used the stack's containers since the stack was created or stopped.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// IdleTTL is the Go duration from aviary.yml's idle_ttl when set.
	IdleTTL string `json:"idle_ttl,omitempty"`
}

// IsWarm reports whether the stack was started by a transient command rather
// than `up` and has not yet been stopped.
func (e Entry) IsWarm() bool {
	return !e.Held && e.LastUsedAt != nil
}

// IdleDuration returns e.IdleTTL when set and valid, or DefaultIdleTTL.
func (e Entry) IdleDuration() time.Duration {
	if e.IdleTTL != "" {
		if d, err := time.ParseDuration(e.IdleTTL); err == nil && d > 0 {
			return d
		}
	}
	return DefaultIdleTTL
}

// IdleDeadline returns when a warm stack becomes eligible to be stopped, or nil
// when the stack is held or cold.
func (e Entry) IdleDeadline() *time.Time {
	if !e.IsWarm() {
		return nil
	}
	deadline := e.LastUsedAt.Add(e.IdleDuration())
	return &deadline
}

// IsIdleAt reports whether e is a warm stack whose idle window has elapsed at now.
func (e Entry) IsIdleAt(now time.Time) bool {
	deadline := e.IdleDeadline()
	return deadline != nil && !now.Before(*deadline)
}

// IdleFor returns how long ago LastUsedAt was at now, or 0 when unset.
func (e Entry) IdleFor(now time.Time) time.Duration {
	if e.LastUsedAt == nil || now.Before(*e.LastUsedAt) {
		return 0
	}
	return now.Sub(*e.LastUsedAt)
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
	// Now returns the current time for timestamps; nil defaults to time.Now.
	Now func() time.Time
}

func (r *Registry) nowUTC() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
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
			entry = Entry{Project: project, Ports: map[string]int{}, CreatedAt: r.nowUTC()}
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
			now := r.nowUTC()
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
	if s.Version != 1 && s.Version != Version {
		return nil, fmt.Errorf("%s: registry version %d is not supported; this aviary reads versions 1 and %d", r.path, s.Version, Version)
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
	lock, err := r.newLock(project + ".lock")
	if err != nil {
		return nil, err
	}
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	return lock.Unlock, nil
}

func (r *Registry) newLock(filename string) (*flock.Flock, error) {
	dir := filepath.Join(filepath.Dir(r.path), "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return flock.New(filepath.Join(dir, filename)), nil
}

// UseLease holds a shared kernel lock on <project>.use.lock while a command is
// starting or using a stack's containers, preventing an idle reaper from
// stopping the stack underneath it.
type UseLease struct {
	Entry        Entry
	registry     *Registry
	project      string
	idleTTL      string
	lock         *flock.Flock
	touchOnClose bool
}

// ProjectLock is an exclusive kernel file lock held on one project's lockfile.
type ProjectLock struct {
	lock *flock.Flock
}

func (l *ProjectLock) Unlock() error {
	if l == nil || l.lock == nil {
		return nil
	}
	err := l.lock.Unlock()
	l.lock = nil
	return err
}

// BeginHold acquires a shared use lock on project so no idle reaper can stop it
// while `up` is starting. Call MarkHeld only after `up` succeeds.
func (r *Registry) BeginHold(project string) (*UseLease, error) {
	lock, err := r.newLock(project + ".use.lock")
	if err != nil {
		return nil, err
	}
	if err := lock.RLock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	entry, ok, err := r.Get(project)
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	if !ok {
		_ = lock.Unlock()
		return nil, fmt.Errorf("%s is not registered", project)
	}
	return &UseLease{Entry: entry, registry: r, project: project, lock: lock}, nil
}

// MarkHeld records that project was started explicitly (`up`) and must not be
// auto-stopped for idleness.
func (r *Registry) MarkHeld(project string) (Entry, error) {
	var entry Entry
	err := r.update(func(s *state) error {
		var ok bool
		entry, ok = s.Entries[project]
		if !ok {
			return fmt.Errorf("%s is not registered", project)
		}
		entry.Held = true
		s.Entries[project] = entry
		return nil
	})
	return entry, err
}

// MarkUsed records that a command used the stack without holding it. Held is
// left as it is. When the stack is not held, this starts its idle timer.
// Marking an unregistered project returns nil, nil.
func (r *Registry) MarkUsed(project, idleTTL string) (*Entry, error) {
	var updated *Entry
	err := r.update(func(s *state) error {
		existing, ok := s.Entries[project]
		if !ok {
			return nil
		}
		now := r.nowUTC()
		existing.LastUsedAt = &now
		existing.IdleTTL = idleTTL
		s.Entries[project] = existing
		updated = &existing
		return nil
	})
	return updated, err
}

// BeginUse acquires a shared use lock for a transient command (`test`, `init`,
// `run`). Finish stamps LastUsedAt (preserving Held if already set) before
// releasing the lock.
func (r *Registry) BeginUse(project, idleTTL string) (*UseLease, error) {
	lock, err := r.newLock(project + ".use.lock")
	if err != nil {
		return nil, err
	}
	if err := lock.RLock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	entry, ok, err := r.Get(project)
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	if !ok {
		_ = lock.Unlock()
		return nil, fmt.Errorf("%s is not registered", project)
	}
	return &UseLease{
		Entry:        entry,
		registry:     r,
		project:      project,
		idleTTL:      idleTTL,
		lock:         lock,
		touchOnClose: true,
	}, nil
}

// Finish updates LastUsedAt (for transient leases) and releases the shared use
// lock. If the stack was removed while the command ran, Finish releases the
// lock without recreating the entry and returns nil.
func (l *UseLease) Finish() (*Entry, error) {
	if l == nil || l.lock == nil {
		return nil, nil
	}
	var updated *Entry
	var updateErr error
	if l.touchOnClose {
		updateErr = l.registry.update(func(s *state) error {
			existing, ok := s.Entries[l.project]
			if !ok {
				return nil
			}
			now := l.registry.nowUTC()
			existing.LastUsedAt = &now
			existing.IdleTTL = l.idleTTL
			s.Entries[l.project] = existing
			updated = &existing
			return nil
		})
	}
	unlockErr := l.lock.Unlock()
	l.lock = nil
	if updateErr != nil {
		return nil, updateErr
	}
	return updated, unlockErr
}

// MarkStopped clears Held and LastUsedAt for project while keeping its ports
// and InitializedAt. Marking an unregistered project stopped returns nil, nil.
func (r *Registry) MarkStopped(project string) (*Entry, error) {
	s, err := r.load()
	if err != nil {
		return nil, err
	}
	if _, ok := s.Entries[project]; !ok {
		return nil, nil
	}
	var updated *Entry
	err = r.update(func(s *state) error {
		entry, found := s.Entries[project]
		if !found {
			return nil
		}
		entry.Held = false
		entry.LastUsedAt = nil
		s.Entries[project] = entry
		updated = &entry
		return nil
	})
	return updated, err
}

// TryLockIdle attempts a non-blocking exclusive lock on <project>.use.lock,
// returning nil, nil when an active UseLease holds the lock.
func (r *Registry) TryLockIdle(project string) (*ProjectLock, error) {
	lock, err := r.newLock(project + ".use.lock")
	if err != nil {
		return nil, err
	}
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	if !ok {
		return nil, nil
	}
	return &ProjectLock{lock: lock}, nil
}

// WaitUseIdle blocks until all active UseLeases on project have finished.
func (r *Registry) WaitUseIdle(project string) error {
	lock, err := r.newLock(project + ".use.lock")
	if err != nil {
		return err
	}
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	return lock.Unlock()
}

// TryClaimReaper attempts a non-blocking exclusive lock on
// <project>.reaper.lock, returning nil, nil when another reaper already holds
// it.
func (r *Registry) TryClaimReaper(project string) (*ProjectLock, error) {
	lock, err := r.newLock(project + ".reaper.lock")
	if err != nil {
		return nil, err
	}
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	if !ok {
		return nil, nil
	}
	return &ProjectLock{lock: lock}, nil
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
	s.Version = Version
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
