package registry

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// busyPorts is a Prober that reports the listed ports as taken and every
// other port as free, so tests never bind real sockets.
type busyPorts map[int]bool

func (b busyPorts) Free(port int) bool { return !b[port] }

func open(t *testing.T, busy ...int) *Registry {
	t.Helper()
	reg := OpenAt(filepath.Join(t.TempDir(), "registry.json"))
	held := busyPorts{}
	for _, p := range busy {
		held[p] = true
	}
	reg.Prober = held
	return reg
}

func TestAllocateGivesDistinctPortsAcrossProjects(t *testing.T) {
	reg := open(t)
	a, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres", "redis"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Allocate("aviary-b", "/wt/b", "", []string{"postgres", "redis"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	for _, e := range []Entry{a, b} {
		for name, port := range e.Ports {
			if owner, dup := seen[port]; dup {
				t.Errorf("port %d given to both %s and %s/%s", port, owner, e.Project, name)
			}
			seen[port] = e.Project + "/" + name
			if port < FirstPort {
				t.Errorf("port %d is below FirstPort", port)
			}
		}
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 ports, got %v", seen)
	}
}

func TestAllocateIsStableAndFollowsTheNameList(t *testing.T) {
	reg := open(t)
	first, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres", "redis"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres", "redis"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Ports, first.Ports) {
		t.Errorf("ports changed between allocations: %v then %v", first.Ports, again.Ports)
	}
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed between allocations: %v then %v", first.CreatedAt, again.CreatedAt)
	}

	changed, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres", "api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, still := changed.Ports["redis"]; still {
		t.Error("redis should have been released once it left the name list")
	}
	if changed.Ports["postgres"] != first.Ports["postgres"] {
		t.Error("postgres should keep its port when the list changes")
	}
	// The port redis gave up is the lowest free one, so api takes it.
	if changed.Ports["api"] != first.Ports["redis"] {
		t.Errorf("api = %d, want the released redis port %d", changed.Ports["api"], first.Ports["redis"])
	}
}

func TestAllocateSkipsPortsTheProberReportsBusy(t *testing.T) {
	reg := open(t, FirstPort)
	entry, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.Ports["postgres"]; got != FirstPort+1 {
		t.Errorf("postgres = %d, want %d", got, FirstPort+1)
	}
}

func TestAllocateSkipsPortsHeldByOtherEntriesWithoutProbingThem(t *testing.T) {
	reg := open(t)
	if _, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	probed := map[int]bool{}
	reg.Prober = recordingProber(probed)
	b, err := reg.Allocate("aviary-b", "/wt/b", "", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Ports["postgres"] != FirstPort+1 {
		t.Errorf("postgres = %d, want %d", b.Ports["postgres"], FirstPort+1)
	}
	if probed[FirstPort] {
		t.Errorf("port %d belongs to aviary-a and should not have been probed", FirstPort)
	}
}

type recordingProber map[int]bool

func (r recordingProber) Free(port int) bool {
	r[port] = true
	return true
}

func TestAllocateErrorsWhenNoPortIsFree(t *testing.T) {
	reg := open(t)
	reg.Prober = allBusy{}
	_, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
	if err == nil || !strings.Contains(err.Error(), "no free port") {
		t.Fatalf("expected a no-free-port error, got %v", err)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed allocation should write nothing, got %v", entries)
	}
}

type allBusy struct{}

func (allBusy) Free(int) bool { return false }

func TestAllocatePersistsRootAndConfig(t *testing.T) {
	reg := open(t)
	if _, err := reg.Allocate("aviary-a", "/wt/a", "/defs/myrepo", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Allocate("aviary-b", "/wt/b", "", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := OpenAt(reg.Path()).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %v", entries)
	}
	a, b := entries[0], entries[1]
	if a.Project != "aviary-a" || a.Root != "/wt/a" || a.Config != "/defs/myrepo" {
		t.Errorf("aviary-a = %+v", a)
	}
	if b.Project != "aviary-b" || b.Root != "/wt/b" || b.Config != "" {
		t.Errorf("aviary-b = %+v", b)
	}
	if a.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}

	// Moving the definition in-tree clears config on the next allocation.
	moved, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Config != "" {
		t.Errorf("config = %q, want empty", moved.Config)
	}
}

func TestFileCarriesVersionAndEntryKeys(t *testing.T) {
	reg := open(t)
	if _, err := reg.Allocate("aviary-a", "/wt/a", "/defs/myrepo", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version int                                   `json:"version"`
		Entries map[string]map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Version != Version {
		t.Errorf("version = %d, want %d", file.Version, Version)
	}
	entry := file.Entries["aviary-a"]
	for _, key := range []string{"project", "root", "config", "ports", "created_at"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("entry is missing %q: %s", key, raw)
		}
	}
	if _, err := os.Stat(reg.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temp file should have been renamed away, stat err = %v", err)
	}
}

func TestUnknownVersionIsAnErrorNamingThePath(t *testing.T) {
	for name, body := range map[string]string{
		"future":  `{"version": 2, "entries": {}}`,
		"missing": `{"entries": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			reg := open(t)
			if err := os.WriteFile(reg.Path(), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := reg.List()
			if err == nil || !strings.Contains(err.Error(), reg.Path()) || !strings.Contains(err.Error(), "version") {
				t.Errorf("List: expected a version error naming %s, got %v", reg.Path(), err)
			}
			_, err = reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
			if err == nil || !strings.Contains(err.Error(), reg.Path()) {
				t.Errorf("Allocate: expected an error naming %s, got %v", reg.Path(), err)
			}
			after, err := os.ReadFile(reg.Path())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != body {
				t.Errorf("a file with an unknown version must not be rewritten, now %s", after)
			}
		})
	}
}

func TestMissingFileIsAnEmptyRegistry(t *testing.T) {
	reg := open(t)
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %v", entries)
	}
	if _, err := os.Stat(reg.Path()); !os.IsNotExist(err) {
		t.Errorf("reading should not create the file, stat err = %v", err)
	}
}

func TestListIsSortedByProject(t *testing.T) {
	reg := open(t)
	for _, p := range []string{"aviary-c", "aviary-a", "aviary-b"} {
		if _, err := reg.Allocate(p, "/wt/"+p, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Project)
	}
	if want := []string{"aviary-a", "aviary-b", "aviary-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestReleaseForgetsTheProject(t *testing.T) {
	reg := open(t)
	if _, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Release("aviary-a"); err != nil {
		t.Fatal(err)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected an empty registry, got %v", entries)
	}
	if err := reg.Release("aviary-a"); err != nil {
		t.Errorf("releasing an unregistered project should be a no-op, got %v", err)
	}
}

func TestSetInitializedRoundTrips(t *testing.T) {
	reg := open(t)
	if _, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	set, err := reg.SetInitialized("aviary-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if set.InitializedAt == nil || set.Ports["postgres"] != FirstPort || set.Root != "/wt/a" {
		t.Fatalf("SetInitialized returned %+v, want the whole entry with initialized_at set", set)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entries[0], set) {
		t.Fatalf("stored entry %+v, want the one SetInitialized returned %+v", entries[0], set)
	}

	// Re-allocating keeps the marker; only SetInitialized(false) clears it.
	again, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if again.InitializedAt == nil {
		t.Error("Allocate should not clear initialized_at")
	}

	cleared, err := reg.SetInitialized("aviary-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.InitializedAt != nil {
		t.Errorf("SetInitialized(false) returned %+v, want initialized_at cleared", cleared)
	}
	entries, err = reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].InitializedAt != nil {
		t.Errorf("initialized_at should be cleared, got %v", entries[0].InitializedAt)
	}

	if _, err := reg.SetInitialized("aviary-missing", true); err == nil || !strings.Contains(err.Error(), "aviary-missing") {
		t.Errorf("expected an error naming the unregistered project, got %v", err)
	}
}

func TestGetReadsOneEntryWithoutWriting(t *testing.T) {
	reg := open(t)
	if _, ok, err := reg.Get("aviary-a"); err != nil || ok {
		t.Fatalf("Get on a missing registry file = ok %v, err %v; want not found", ok, err)
	}
	if _, err := os.Stat(reg.Path()); !os.IsNotExist(err) {
		t.Errorf("Get created %s (stat err %v)", reg.Path(), err)
	}
	want, err := reg.Allocate("aviary-a", "/wt/a", "/defs/a", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := reg.Get("aviary-a")
	if err != nil || !ok {
		t.Fatalf("Get(aviary-a) = ok %v, err %v; want found", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get(aviary-a) = %+v, want %+v", got, want)
	}
	if _, ok, err := reg.Get("aviary-b"); err != nil || ok {
		t.Errorf("Get(aviary-b) = ok %v, err %v; want not found", ok, err)
	}
	after, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("Get changed the registry file:\n%s\nwas\n%s", after, before)
	}
}

func TestListenProberSeesHeldPorts(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", ":0"} {
		t.Run(addr, func(t *testing.T) {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			port := l.Addr().(*net.TCPAddr).Port
			if (ListenProber{}).Free(port) {
				t.Errorf("port %d is held by a listener on %s but was reported free", port, addr)
			}
		})
	}
}

func TestAllocateWithTheRealProberSkipsAHeldPort(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "registry.json"))
	held := 0
	for port := FirstPort; port <= lastPort; port++ {
		if (ListenProber{}).Free(port) {
			held = port
			break
		}
	}
	if held == 0 {
		t.Fatal("no free port to hold")
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", held))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	entry, err := reg.Allocate("aviary-a", "/wt/a", "", []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.Ports["postgres"]; got <= held {
		t.Errorf("postgres = %d, want a port above the held %d", got, held)
	}
}

func TestEntryWithoutAMatchingProjectIsAnErrorNamingTheFileAndKey(t *testing.T) {
	for name, entry := range map[string]string{
		"missing":  `{"root": "/wt/foo", "ports": {}}`,
		"empty":    `{"project": "", "root": "/wt/foo", "ports": {}}`,
		"mismatch": `{"project": "aviary-bar-def456", "root": "/wt/foo", "ports": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			reg := open(t)
			body := `{"version": 1, "entries": {"aviary-foo-abc123": ` + entry + `}}`
			if err := os.WriteFile(reg.Path(), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			named := func(err error) bool {
				return err != nil && strings.Contains(err.Error(), reg.Path()) &&
					strings.Contains(err.Error(), `"aviary-foo-abc123"`)
			}
			if _, err := reg.List(); !named(err) {
				t.Errorf("List: want an error naming %s and the key, got %v", reg.Path(), err)
			}
			if _, _, err := reg.Get("aviary-foo-abc123"); !named(err) {
				t.Errorf("Get: want an error naming %s and the key, got %v", reg.Path(), err)
			}
			if _, err := reg.Allocate("aviary-foo-abc123", "/wt/foo", "", nil); !named(err) {
				t.Errorf("Allocate: want an error naming %s and the key, got %v", reg.Path(), err)
			}
			if err := reg.Release(""); !named(err) {
				t.Errorf("Release: want an error naming %s and the key, got %v", reg.Path(), err)
			}
			after, err := os.ReadFile(reg.Path())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != body {
				t.Errorf("a registry with a bad entry must not be rewritten, now %s", after)
			}
		})
	}
}

func TestOpenIgnoresARelativeXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".local", "state", "aviary", "registry.json")
	for _, dir := range []string{"", "relative/dir", "."} {
		t.Setenv("XDG_STATE_HOME", dir)
		reg, err := Open()
		if err != nil {
			t.Fatal(err)
		}
		if reg.Path() != want {
			t.Errorf("XDG_STATE_HOME=%q: Path = %s, want %s", dir, reg.Path(), want)
		}
	}
	abs := t.TempDir()
	t.Setenv("XDG_STATE_HOME", abs)
	reg, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(abs, "aviary", "registry.json"); reg.Path() != want {
		t.Errorf("absolute XDG_STATE_HOME: Path = %s, want %s", reg.Path(), want)
	}
}
