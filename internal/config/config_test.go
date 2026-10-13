package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fullExample is a v1 schema example covering every key.
const fullExample = `compose:
  files: [compose.yaml, compose.dev.yaml]   # relative to this file
  profiles: []                              # optional, passed as --profile
ports: [postgres, redis, web_https]         # names -> AVIARY_PORT_<NAME>
require_env: [VAULT_TOKEN]                  # fail early with a clear message
init:                                       # seed from scratch, once per key
  - {service: api, command: [rake, db:reset]}
  - {service: api, command: [psql, -f, db/reference_data.sql]}
  - {service: worker, command: [bin/worker, seed]}
after_restore:                              # bring a restored volume current
  - {service: api, command: [rake, db:migrate]}
suites:
  web:        {service: web, needs: [postgres, redis], command: [pnpm, run, test:ci]}
  frontend:   {service: frontend, command: [pnpm, run, test:ci], init: false}
  acceptance: {service: acceptance, profiles: [acceptance],
               needs: [proxy, frontend, worker, web, api, admin, scheduler, search],
               command: [pnpm, test]}
  ml-train:   {service: ml_trainer, profiles: [ml-train], needs: [postgres],
               command: [./run_tests.sh]}
  api-empty:  {service: api, needs: [redis], command: [rake, test], init: false}
snapshot:
  volumes: [postgres]
  key_files: [src/worker/seeds/, src/api/db/reference_data.sql]   # seed inputs
  stamp_files: [src/api/db/schema.sql]                             # schema
`

func TestLoadReadsFilesAndPorts(t *testing.T) {
	cfg, err := Load(write(t, "compose:\n  files: [a.yaml, b.yaml]\nports: [postgres, api_v2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Compose.Files) != 2 || cfg.Compose.Files[1] != "b.yaml" {
		t.Errorf("files = %v", cfg.Compose.Files)
	}
	if len(cfg.Ports) != 2 || cfg.Ports[1] != "api_v2" {
		t.Errorf("ports = %v", cfg.Ports)
	}
}

func TestLoadReadsTheFullSchema(t *testing.T) {
	cfg, err := Load(write(t, fullExample))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.RequireEnv, []string{"VAULT_TOKEN"}) {
		t.Errorf("require_env = %v", cfg.RequireEnv)
	}
	if len(cfg.Init) != 3 || cfg.Init[2].Service != "worker" {
		t.Errorf("init = %+v", cfg.Init)
	}
	want := []Step{{Service: "api", Command: []string{"rake", "db:migrate"}}}
	if !reflect.DeepEqual(cfg.AfterRestore, want) {
		t.Errorf("after_restore = %+v", cfg.AfterRestore)
	}
	if got := cfg.Suites["acceptance"].Profiles; !reflect.DeepEqual(got, []string{"acceptance"}) {
		t.Errorf("acceptance profiles = %v", got)
	}
	if got := cfg.Suites["ml-train"].Needs; !reflect.DeepEqual(got, []string{"postgres"}) {
		t.Errorf("ml-train needs = %v", got)
	}
	if cfg.Snapshot == nil {
		t.Fatal("snapshot not read")
	}
	if !reflect.DeepEqual(cfg.Snapshot.Volumes, []string{"postgres"}) ||
		len(cfg.Snapshot.KeyFiles) != 2 ||
		!reflect.DeepEqual(cfg.Snapshot.StampFiles, []string{"src/api/db/schema.sql"}) {
		t.Errorf("snapshot = %+v", cfg.Snapshot)
	}
}

func TestFullSchemaRoundTrips(t *testing.T) {
	cfg, err := Load(write(t, fullExample))
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(write(t, string(out)))
	if err != nil {
		t.Fatalf("reloading marshalled config: %v\n%s", err, out)
	}
	// Marshalled form is compared because an omitted list reloads as an
	// empty one, which DeepEqual would report as a difference.
	outAgain, err := yaml.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(outAgain) {
		t.Errorf("round trip changed the config:\nbefore\n%s\nafter\n%s", out, outAgain)
	}
}

func TestStackProfilesAreRead(t *testing.T) {
	cfg, err := Load(write(t, "compose:\n  files: [a.yaml]\n  profiles: [dev, tools]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Compose.Profiles, []string{"dev", "tools"}) {
		t.Errorf("profiles = %v", cfg.Compose.Profiles)
	}
}

func TestSuitesRunInitUnlessTheyOptOut(t *testing.T) {
	cfg, err := Load(write(t, `compose:
  files: [a.yaml]
init:
  - service: db
    command: [seed]
suites:
  seeded:
    service: api
    command: [test]
    needs: [db]
  explicit:
    service: api
    command: [test]
    init: true
  empty:
    service: api
    command: [test]
    init: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Suites["seeded"].WantsInit() {
		t.Error("a suite that says nothing about init should want it")
	}
	if !cfg.Suites["explicit"].WantsInit() {
		t.Error("init: true should want init")
	}
	if cfg.Suites["empty"].WantsInit() {
		t.Error("init: false should opt out")
	}
	if cfg.Init[0].Service != "db" || cfg.Suites["seeded"].Needs[0] != "db" {
		t.Errorf("init = %+v, suites = %+v", cfg.Init, cfg.Suites)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), FileName) {
		t.Errorf("error should name the file, got %v", err)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	const base = "compose:\n  files: [a.yaml]\n"
	// Each case names the input and a fragment of the error it must produce,
	// so an incidental YAML error cannot stand in for a missing check.
	cases := map[string]struct{ body, want string }{
		"no compose section":                 {"ports: [postgres]\n", "compose.files"},
		"empty compose files":                {"compose:\n  files: []\n", "compose.files"},
		"empty stack profile":                {"compose:\n  files: [a.yaml]\n  profiles: [\"\"]\n", "compose.profiles[0] is empty"},
		"unknown top-level":                  {base + "prots: [db]\n", "field prots not found"},
		"unknown compose key":                {"compose:\n  files: [a.yaml]\n  file: [b.yaml]\n", "field file not found"},
		"unknown suite key":                  {base + "suites:\n  api: {service: api, command: [t], need: [db]}\n", "field need not found"},
		"unknown snapshot key":               {base + "snapshot:\n  volumes: [pg]\n  stamp: [x]\n", "field stamp not found"},
		"uppercase port name":                {base + "ports: [Postgres]\n", `port name "Postgres"`},
		"port name starting with a digit":    {base + "ports: [2db]\n", `port name "2db"`},
		"port name with a dash":              {base + "ports: [web-https]\n", `port name "web-https"`},
		"duplicate port":                     {base + "ports: [db, db]\n", "listed twice"},
		"init step without service":          {base + "init:\n  - command: [\"true\"]\n", "init[0]: needs a service"},
		"init step without command":          {base + "init:\n  - service: db\n", "init[0]: needs a command"},
		"after_restore step without service": {base + "after_restore:\n  - command: [migrate]\n", "after_restore[0]: needs a service"},
		"after_restore step without command": {base + "after_restore:\n  - service: db\n", "after_restore[0]: needs a command"},
		"suite without service":              {base + "suites:\n  api:\n    command: [test]\n", `suite "api": needs a service`},
		"suite without command":              {base + "suites:\n  api:\n    service: api\n", `suite "api": needs a command`},
		"suite with an empty need":           {base + "suites:\n  api: {service: api, command: [t], needs: [db, \"\"]}\n", `suite "api": needs[1] is empty`},
		"suite with an empty profile":        {base + "suites:\n  api: {service: api, command: [t], profiles: [\"\"]}\n", `suite "api": profiles[0] is empty`},
		"snapshot without volumes":           {base + "snapshot:\n  key_files: [seeds/]\n", "snapshot.volumes"},
		"snapshot with empty volumes":        {base + "snapshot:\n  volumes: []\n", "snapshot.volumes"},
		"require_env with a dash":            {base + "require_env: [VAULT-TOKEN]\n", `require_env name "VAULT-TOKEN"`},
		"require_env starting with a digit":  {base + "require_env: [1TOKEN]\n", `require_env name "1TOKEN"`},
		"require_env empty name":             {base + "require_env: [\"\"]\n", `require_env name ""`},
		"require_env with an equals sign":    {base + "require_env: [A=B]\n", `require_env name "A=B"`},
	}
	for name, c := range cases {
		_, err := Load(write(t, c.body))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if !strings.Contains(err.Error(), FileName) {
			t.Errorf("%s: error should name the file, got %v", name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error should contain %q, got %v", name, c.want, err)
		}
	}
}

func TestLoadAcceptsValidEnvNames(t *testing.T) {
	cfg, err := Load(write(t, "compose:\n  files: [a.yaml]\nrequire_env: [VAULT_TOKEN, _private, lower_ok2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RequireEnv) != 3 {
		t.Errorf("require_env = %v", cfg.RequireEnv)
	}
}

// The example the README points at must stay loadable.
func TestMinimalExampleLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "minimal"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Ports, []string{"postgres", "redis"}) {
		t.Errorf("ports = %v", cfg.Ports)
	}
	if !reflect.DeepEqual(cfg.Compose.Files, []string{"compose.yaml"}) {
		t.Errorf("files = %v", cfg.Compose.Files)
	}
	if len(cfg.Init) != 1 {
		t.Errorf("init = %v", cfg.Init)
	}
	if !cfg.Suites["db"].WantsInit() || cfg.Suites["smoke"].WantsInit() {
		t.Errorf("db should want init and smoke should not: %+v", cfg.Suites)
	}
}
