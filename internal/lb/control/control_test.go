package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lbConfig = `
lb:
  listeners:
    - id: web
      proto: http
      bind: 127.0.0.1:8080
      pool: p1
      max_conns: 100
  pools:
    - id: p1
      picker: round_robin
      backends:
        - {id: b1, addr: "127.0.0.1:9001", weight: 1}
        - {id: b2, addr: "127.0.0.1:9002", weight: 2}
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rift.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBuildsSnapshot(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap := ctl.Snapshot()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if len(snap.Listeners) != 1 {
		t.Errorf("listeners = %d, want 1", len(snap.Listeners))
	}
	pool := snap.Pools["p1"]
	if pool == nil {
		t.Fatal("pool p1 missing")
	}
	if len(pool.Backends) != 2 {
		t.Errorf("backends = %d, want 2", len(pool.Backends))
	}
	if pool.Backends[1].Weight != 2 {
		t.Errorf("weight = %d, want 2", pool.Backends[1].Weight)
	}
	if pool.Picker != "round_robin" {
		t.Errorf("picker = %q, want round_robin", pool.Picker)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	ctl := New(writeConfig(t, `
lb:
  pools:
    - id: p1
      backends: [{id: b1, addr: "no-port"}]
`))
	if err := ctl.Load(); err == nil {
		t.Fatal("invalid config must fail to load")
	}
	if ctl.Snapshot() != nil {
		t.Error("no snapshot may be published from an invalid config")
	}
}

func TestReloadIncrementsVersionAndSwaps(t *testing.T) {
	path := writeConfig(t, lbConfig)
	ctl := New(path)
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	v1 := ctl.Snapshot().Version

	// Change the backend set, then reload.
	updated := strings.Replace(lbConfig, `{id: b2, addr: "127.0.0.1:9002", weight: 2}`,
		`{id: b3, addr: "127.0.0.1:9003", weight: 1}`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	v2 := ctl.Snapshot().Version
	if v2 != v1+1 {
		t.Errorf("version %d → %d, want increment", v1, v2)
	}
	if len(ctl.Snapshot().Pools["p1"].Backends) != 2 {
		t.Error("snapshot not swapped to the new backend set")
	}
}

// TestReloadRejectsInvalidConfigRetainsPrevious: a typo must not take down
// the data plane.
func TestReloadRejectsInvalidConfigRetainsPrevious(t *testing.T) {
	path := writeConfig(t, lbConfig)
	ctl := New(path)
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	before := ctl.Snapshot()

	if err := os.WriteFile(path, []byte("lb:\n  pools:\n    - id: p1\n      backends: [{id: b, addr: nope}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Reload(context.Background()); err == nil {
		t.Fatal("invalid reload must be rejected")
	}
	if ctl.Snapshot() != before {
		t.Error("a rejected reload must retain the previous snapshot")
	}
}

func TestReloadMarksRemovedBackendsDraining(t *testing.T) {
	path := writeConfig(t, lbConfig)
	ctl := New(path)
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	// Replace b2 with b3 so a removal actually occurs.
	updated := strings.Replace(lbConfig,
		`{id: b2, addr: "127.0.0.1:9002", weight: 2}`,
		`{id: b3, addr: "127.0.0.1:9003", weight: 1}`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	draining := ctl.Draining()
	found := false
	for _, d := range draining {
		if strings.Contains(d, "127.0.0.1:9002") {
			found = true
		}
	}
	if !found {
		t.Errorf("removed backend not marked draining: %v", draining)
	}
}

// TestAdminReloadRequiresAuthFromNonLoopback is the admin-plane red line: an
// unauthenticated remote reload is a configuration-takeover primitive.
func TestAdminReloadRequiresAuthFromNonLoopback(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	h := ctl.Handler()

	// No token configured, remote peer: refused.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reload", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("remote reload without token: %d, want 401", rec.Code)
	}

	// Loopback peer without a token: allowed (the admin plane defaults to
	// loopback, where no remote caller exists to authenticate).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/admin/reload", nil)
	req2.RemoteAddr = "127.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("loopback reload should be permitted without a token")
	}
}

func TestAdminReloadWithToken(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	ctl.SetAdminToken("s3cret-token")
	h := ctl.Handler()

	// Wrong token: refused.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reload", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", rec.Code)
	}

	// Correct token: accepted.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/admin/reload", nil)
	req2.RemoteAddr = "203.0.113.9:1234"
	req2.Header.Set("Authorization", "Bearer s3cret-token")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("correct token: %d, want 200", rec2.Code)
	}
}

func TestAdminReloadRejectsGET(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/reload", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	ctl.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reload: %d, want 405", rec.Code)
	}
}

func TestSnapshotEndpointExposesTopology(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/pools", nil)
	rec := httptest.NewRecorder()
	ctl.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pools: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "127.0.0.1:9001") {
		t.Errorf("pools response missing backend: %s", body)
	}
}

func TestBuildSnapshotFromSchema(t *testing.T) {
	ctl := New(writeConfig(t, lbConfig))
	if err := ctl.Load(); err != nil {
		t.Fatal(err)
	}
	snap := ctl.Snapshot()
	if snap.Pools["p1"].ID != "p1" {
		t.Errorf("pool id = %q", snap.Pools["p1"].ID)
	}
}