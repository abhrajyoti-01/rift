package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rift.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
observability:
  log_level: info
  admin_addr: 127.0.0.1:9000
lb:
  listeners:
    - id: web
      proto: http
      bind: 127.0.0.1:8080
      pool: p1
      max_conns: 100
  pools:
    - id: p1
      picker: least_connections
      backends:
        - {id: b1, addr: "127.0.0.1:9001", weight: 1}
      health:
        kind: tcp
        interval: 5s
        timeout: 2s
`

func TestLoadValidConfig(t *testing.T) {
	path := writeTemp(t, validConfig)
	s, src, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.Path != path {
		t.Errorf("src.Path = %q, want %q", src.Path, path)
	}
	if len(s.LB.Listeners) != 1 || s.LB.Listeners[0].ID != "web" {
		t.Errorf("listener not parsed: %+v", s.LB.Listeners)
	}
	if s.LB.Pools[0].Health.Interval.D().Seconds() != 5 {
		t.Errorf("interval = %v, want 5s", s.LB.Pools[0].Health.Interval.D())
	}
}

// TestUnknownFieldRejected is the typo-safety contract: an unknown field is
// a hard error, not silently ignored.
func TestUnknownFieldRejected(t *testing.T) {
	path := writeTemp(t, validConfig+"\n  typo_field: 1\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if errs.ClassOf(err) != errs.ClassConfig {
		t.Errorf("class = %v, want ClassConfig", errs.ClassOf(err))
	}
}

func TestValidationReportsEveryViolation(t *testing.T) {
	bad := `
lb:
  listeners:
    - id: ""
      proto: bogus
      bind: "not-an-addr"
      pool: missing_pool
      max_conns: -5
  pools:
    - id: p1
      picker: nonsense
      backends:
        - {id: b1, addr: "no-port", weight: 0}
`
	path := writeTemp(t, bad)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("invalid config must fail validation")
	}
	msg := err.Error()
	// Every problem must be addressable by field path, not just the first.
	for _, want := range []string{
		"lb.listeners[0].proto",
		"lb.listeners[0].bind",
		"lb.listeners[0].pool",
		"lb.pools[0].picker",
		"lb.pools[0].backends[0].addr",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("validation output missing %q:\n%s", want, msg)
		}
	}
}

func TestAdminPlaneRefusesRoutableBind(t *testing.T) {
	// An admin plane on a routable address without an explicit opt-in is
	// how an unauthenticated /reload endpoint ships by accident.
	path := writeTemp(t, `
observability:
  admin_addr: 0.0.0.0:9000
`)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("routable admin bind without allow_remote must be refused")
	}
	if !strings.Contains(err.Error(), "allow_remote") {
		t.Errorf("error should name the opt-in: %v", err)
	}

	ok := `
observability:
  admin_addr: 0.0.0.0:9000
  allow_remote: true
`
	if _, _, err := Load(writeTemp(t, ok)); err != nil {
		t.Errorf("allow_remote should permit a routable bind: %v", err)
	}
}

func TestResolverMustBeLiteralIP(t *testing.T) {
	// The monitor must not depend on DNS to bootstrap itself.
	bad := `
dns:
  role: node
  node_id: n1
  hub: {url: "http://127.0.0.1:9001"}
  resolvers:
    - {addr: "dns.google:53"}
  targets:
    - {name: "example.com.", type: A}
`
	_, _, err := Load(writeTemp(t, bad))
	if err == nil {
		t.Fatal("hostname resolver must be rejected")
	}
	if !strings.Contains(err.Error(), "literal IP") {
		t.Errorf("error should explain the literal-IP rule: %v", err)
	}
}

func TestMediaValidation(t *testing.T) {
	bad := `
media:
  root: /srv/media
  max_streams: 10
  max_streams_per_client: 50
  io_mode: nonsense
  readahead: "not-a-size"
  disk_high_water: 1.5
`
	_, _, err := Load(writeTemp(t, bad))
	if err == nil {
		t.Fatal("invalid media config must fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"media.max_streams_per_client",
		"media.io_mode",
		"media.readahead",
		"media.disk_high_water",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestByteSizeParsing(t *testing.T) {
	path := writeTemp(t, `
dns:
  role: node
  node_id: n1
  hub: {url: "http://127.0.0.1:9001"}
  spool_max: 64MiB
  resolvers: [{addr: "1.1.1.1:53"}]
  targets: [{name: "x.", type: A}]
`)
	s, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.DNS.SpoolMax.Bytes(); got != 64<<20 {
		t.Errorf("spool_max = %d, want %d", got, 64<<20)
	}
}

// TestRedactionHidesSecrets is the secret-leak red line: no echo path may
// render key material.
func TestRedactionHidesSecrets(t *testing.T) {
	path := writeTemp(t, `
observability:
  admin_addr: 127.0.0.1:9000
  admin_token_file: /etc/rift/token
lb:
  listeners:
    - id: web
      proto: http
      bind: 127.0.0.1:8080
      pool: p1
      tls: {cert_file: /etc/rift/cert.pem, key_file: /etc/rift/PRIVATE.key}
  pools:
    - id: p1
      backends: [{id: b1, addr: "127.0.0.1:9001"}]
`)
	s, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := Redacted(s)
	if r.Observability.AdminTokenFile != redactedMarker {
		t.Errorf("admin token file not redacted: %q", r.Observability.AdminTokenFile)
	}
	if r.LB.Listeners[0].TLS.KeyFile != redactedMarker {
		t.Errorf("key file not redacted: %q", r.LB.Listeners[0].TLS.KeyFile)
	}
	// The certificate path is not secret and must survive.
	if r.LB.Listeners[0].TLS.CertFile != "/etc/rift/cert.pem" {
		t.Errorf("cert path should not be redacted: %q", r.LB.Listeners[0].TLS.CertFile)
	}
	// The original must not be mutated by redaction.
	if s.LB.Listeners[0].TLS.KeyFile == redactedMarker {
		t.Error("Redacted mutated the source schema")
	}
}

func TestRedactedDiffNeverLeaksSecrets(t *testing.T) {
	a, _, err := Load(writeTemp(t, `
observability: {admin_addr: 127.0.0.1:9000, admin_token_file: /a/token}
lb:
  pools: [{id: p1, backends: [{id: b1, addr: "127.0.0.1:9001"}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Load(writeTemp(t, `
observability: {admin_addr: 127.0.0.1:9000, admin_token_file: /b/other-token}
lb:
  pools: [{id: p1, backends: [{id: b1, addr: "127.0.0.1:9002"}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	d := DiffSchemas(a, b)
	text := d.String()
	if strings.Contains(text, "/a/token") || strings.Contains(text, "/b/other-token") {
		t.Errorf("diff leaked a secret path:\n%s", text)
	}
	// The genuine change must still be visible.
	if !strings.Contains(text, "backends") {
		t.Errorf("diff missed the backend change:\n%s", text)
	}
}

func TestDiffDetectsChanges(t *testing.T) {
	a, _, _ := Load(writeTemp(t, validConfig))
	b, _, _ := Load(writeTemp(t, strings.Replace(validConfig, "max_conns: 100", "max_conns: 200", 1)))
	d := DiffSchemas(a, b)
	if d.Empty() {
		t.Fatal("diff must detect the max_conns change")
	}
	if !strings.Contains(d.String(), "max_conns") {
		t.Errorf("diff should name the field: %s", d.String())
	}
	// Identical schemas produce an empty diff.
	if !DiffSchemas(a, a).Empty() {
		t.Error("identical schemas must diff empty")
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	path := writeTemp(t, validConfig)
	t.Setenv("RIFT_OBSERVABILITY_ADMIN_ADDR", "127.0.0.1:9999")
	s, src, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Observability.AdminAddr != "127.0.0.1:9999" {
		t.Errorf("env override not applied: %q", s.Observability.AdminAddr)
	}
	if len(src.EnvApplied) == 0 {
		t.Error("applied overrides should be recorded")
	}
}

func TestSkipEnvOption(t *testing.T) {
	path := writeTemp(t, validConfig)
	t.Setenv("RIFT_OBSERVABILITY_ADMIN_ADDR", "127.0.0.1:9999")
	s, _, err := LoadWithOptions(path, Options{SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Observability.AdminAddr == "127.0.0.1:9999" {
		t.Error("SkipEnv must ignore environment overrides")
	}
}

func TestMissingFileIsConfigError(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("missing file must error")
	}
	if errs.ClassOf(err) != errs.ClassConfig {
		t.Errorf("class = %v, want ClassConfig", errs.ClassOf(err))
	}
}

func TestHubPlaintextIngestRefusedWhenRoutable(t *testing.T) {
	bad := `
dns:
  role: hub
  hub_server:
    ingest_addr: 0.0.0.0:9001
    data_dir: /tmp/hub
    allow_plaintext_ingest: true
`
	_, _, err := Load(writeTemp(t, bad))
	if err == nil {
		t.Fatal("plaintext ingest on a routable bind must be refused")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error should name the plaintext risk: %v", err)
	}
}

func TestHubRequiresMTLSByDefault(t *testing.T) {
	bad := `
dns:
  role: hub
  hub_server:
    ingest_addr: 127.0.0.1:9001
    data_dir: /tmp/hub
`
	_, _, err := Load(writeTemp(t, bad))
	if err == nil {
		t.Fatal("hub without mTLS material must be refused")
	}
	if !strings.Contains(err.Error(), "mTLS") {
		t.Errorf("error should mention mTLS: %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	path := writeTemp(t, `
observability:
  admin_addr: 127.0.0.1:9000
  shutdown_timeout: 1m30s
`)
	s, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Observability.ShutdownTimeout.D().Seconds(); got != 90 {
		t.Errorf("shutdown_timeout = %v, want 90s", got)
	}
}

func TestInvalidDurationRejected(t *testing.T) {
	path := writeTemp(t, `
observability:
  admin_addr: 127.0.0.1:9000
  shutdown_timeout: "not-a-duration"
`)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("invalid duration must be rejected")
	}
}