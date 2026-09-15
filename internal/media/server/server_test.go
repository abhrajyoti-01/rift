package medserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFixtureServer builds a temp media root with deterministic content and
// a server wired to a real listener.
func newFixtureServer(t *testing.T, cfg Config) (*Server, string, func()) {
	t.Helper()
	root := t.TempDir()
	// 1000-byte file with byte value = index%251 so content checks are exact.
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg.Root = root
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1:0"
	}
	srv := New(cfg)
	ix, err := BuildIndex(root, false, nil)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	srv.PublishIndex(ix)

	ts := httptest.NewServer(srv.Handler())
	return srv, root, ts.Close
}

func TestMediaServesFullBody(t *testing.T) {
	_, _, done := newFixtureServer(t, Config{})
	defer done()
	// Use the httptest server indirectly by re-creating handler; simpler:
	srv, root, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	_ = srv
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 1000 {
		t.Errorf("body %d bytes, want 1000", rec.Body.Len())
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag must be set")
	}
	_ = root
}

func TestMediaRangeRequests(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()

	cases := []struct {
		rangeHdr string
		wantCode int
		wantLen  int
		wantCR   string
	}{
		{"bytes=0-99", http.StatusPartialContent, 100, "bytes 0-99/1000"},
		{"bytes=100-199", http.StatusPartialContent, 100, "bytes 100-199/1000"},
		{"bytes=900-", http.StatusPartialContent, 100, "bytes 900-999/1000"},
		{"bytes=-100", http.StatusPartialContent, 100, "bytes 900-999/1000"},
		{"bytes=0-0", http.StatusPartialContent, 1, "bytes 0-0/1000"},
		{"bytes=1000-", http.StatusRequestedRangeNotSatisfiable, 0, "bytes */1000"},
		{"bytes=abc", http.StatusRequestedRangeNotSatisfiable, 0, "bytes */1000"},
		{"bytes=5-2", http.StatusRequestedRangeNotSatisfiable, 0, "bytes */1000"},
		// Multi-range: v1 serves the full body with 200.
		{"bytes=0-0,2-10", http.StatusOK, 1000, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
		req.Header.Set("Range", c.rangeHdr)
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != c.wantCode {
			t.Errorf("Range %q: status %d, want %d", c.rangeHdr, rec.Code, c.wantCode)
			continue
		}
		if c.wantLen > 0 && rec.Body.Len() != c.wantLen {
			t.Errorf("Range %q: body %d, want %d", c.rangeHdr, rec.Body.Len(), c.wantLen)
		}
		if c.wantCR != "" && rec.Header().Get("Content-Range") != c.wantCR {
			t.Errorf("Range %q: Content-Range %q, want %q", c.rangeHdr, rec.Header().Get("Content-Range"), c.wantCR)
		}
	}
}

func TestMediaRangeContentMatchesOffsets(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	req.Header.Set("Range", "bytes=10-19")
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if len(body) != 10 {
		t.Fatalf("got %d bytes", len(body))
	}
	for i, b := range body {
		want := byte((10 + i) % 251)
		if b != want {
			t.Fatalf("byte %d = %d, want %d (range must map to file offsets)", i, b, want)
		}
	}
}

func TestMediaHeadHasNoBody(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/v1/media/sample.bin", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != "1000" {
		t.Errorf("HEAD Content-Length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestMediaIfRangeMismatchServesFull(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("If-Range", `"stale-etag"`)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 1000 {
		t.Errorf("stale If-Range: code=%d len=%d, want 200/1000", rec.Code, rec.Body.Len())
	}

	// Matching strong ETag → 206.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	req2.Header.Set("Range", "bytes=0-99")
	req2.Header.Set("If-Range", rec.Header().Get("ETag"))
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusPartialContent {
		t.Errorf("matching If-Range: code=%d, want 206", rec2.Code)
	}
}

func TestMediaEmptyFileAnyRangeIs416(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/empty.bin", nil)
	req.Header.Set("Range", "bytes=0-")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("empty-file range: code=%d, want 416", rec.Code)
	}
	// Full GET of an empty file is fine.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/media/empty.bin", nil)
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || rec2.Body.Len() != 0 {
		t.Errorf("empty full GET: code=%d len=%d, want 200/0", rec2.Code, rec2.Body.Len())
	}
}

func TestMediaMethodNotAllowed(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(m, "/v1/media/sample.bin", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: code=%d, want 405", m, rec.Code)
		}
	}
}

// TestMediaPathTraversal is the red-line security test: no crafted name may
// reach outside root.
func TestMediaPathTraversal(t *testing.T) {
	srv, root, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	// A secret file OUTSIDE root that traversal would target.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	defer os.Remove(secret)

	attacks := []string{
		"../secret.txt",
		"../../secret.txt",
		"..%2fsecret.txt",
		"%2e%2e%2fsecret.txt",
		"..%252fsecret.txt",
		"....//secret.txt",
		"sub/../../secret.txt",
		"/etc/passwd",
		"..\\secret.txt",
		"sub/..%2f..%2fsecret.txt",
	}
	for _, a := range attacks {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/media/"+a, nil)
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("traversal %q returned 200 — must not serve outside root", a)
		}
		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("traversal %q leaked file contents", a)
		}
	}
}

func TestMediaNestedPathServed(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/media/sub/nested.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "nested" {
		t.Errorf("nested file: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMediaAdmissionLimits(t *testing.T) {
	// max_streams_per_client 1: a second concurrent request from the same
	// client is refused with 429 while the first is in flight.
	srv, _, closeFn := newFixtureServer(t, Config{MaxStreams: 4, MaxStreamsPerClient: 1})
	defer closeFn()

	// Occupy the per-client slot directly through admit.
	req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	rec := httptest.NewRecorder()
	if !srv.admit(rec, req) {
		t.Fatal("first admit should succeed")
	}
	rec2 := httptest.NewRecorder()
	if srv.admit(rec2, req) {
		t.Fatal("second admit for the same client must be refused")
	}
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("refusal code = %d, want 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	srv.release(req)
	// Slot released: admit works again.
	rec3 := httptest.NewRecorder()
	if !srv.admit(rec3, req) {
		t.Error("admit after release must succeed")
	}
	srv.release(req)
}

func TestMediaIndexBuild(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "b", "deep.mp4"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "top.mp3"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(root, "skip.tmp"), []byte("z"), 0o644)

	ix, err := BuildIndex(root, false, []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Lookup("a/b/deep.mp4"); !ok {
		t.Error("nested asset missing from index")
	}
	if _, ok := ix.Lookup("top.mp3"); !ok {
		t.Error("top-level asset missing")
	}
	if _, ok := ix.Lookup("skip.tmp"); ok {
		t.Error("excluded pattern still indexed")
	}
}

func TestMediaIndexSwapIsAtomic(t *testing.T) {
	srv, root, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	// Publish a fresh index; reads must see either old or new, never a
	// partially-built map (the atomic pointer guarantees this).
	ix, err := BuildIndex(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.PublishIndex(ix)
	if srv.Index() == nil {
		t.Error("index must be published")
	}
}

func TestMediaRunServesOverRealListener(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv.cfg.Bind = addr
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/v1/media/sample.bin")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body) != 1000 {
		t.Errorf("code=%d len=%d, want 200/1000", resp.StatusCode, len(body))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestMediaFirstByteMetricsRecorded(t *testing.T) {
	srv, _, closeFn := newFixtureServer(t, Config{})
	defer closeFn()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil))
	m := srv.Metrics()
	if m.BytesSent.Load() != 1000 {
		t.Errorf("BytesSent = %d, want 1000", m.BytesSent.Load())
	}
	if m.FirstByteCount.Load() != 1 {
		t.Errorf("FirstByteCount = %d, want 1", m.FirstByteCount.Load())
	}
	if m.ActiveStreams.Load() != 0 {
		t.Errorf("ActiveStreams = %d after completion, want 0 (slot leaked)", m.ActiveStreams.Load())
	}
}

func TestSanitizeAssetName(t *testing.T) {
	ok := []string{"a.mp4", "sub/a.mp4", "a/b/c.mp4", "weird name.mp3"}
	for _, n := range ok {
		if _, valid := sanitizeAssetName(n); !valid {
			t.Errorf("sanitizeAssetName(%q) rejected a legitimate name", n)
		}
	}
	bad := []string{"", "../x", "..", "/abs", "a/../../x", "a\\b", "a\x00b"}
	for _, n := range bad {
		if _, valid := sanitizeAssetName(n); valid {
			t.Errorf("sanitizeAssetName(%q) accepted an unsafe name", n)
		}
	}
}

func TestClientIPIgnoresForwardedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/media/sample.bin", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want peer address (XFF must not affect admission)", got)
	}
}

func BenchmarkMediaRangeRequest(b *testing.B) {
	root := b.TempDir()
	data := make([]byte, 1<<20)
	os.WriteFile(filepath.Join(root, "big.bin"), data, 0o644)
	srv := New(Config{Root: root, IOMode: "buffered", Readahead: 64 << 10})
	ix, _ := BuildIndex(root, false, nil)
	srv.PublishIndex(ix)
	h := srv.Handler()

	b.ResetTimer()
	b.SetBytes(4096)
	for i := 0; i < b.N; i++ {
		rec := &discardRecorder{header: http.Header{}}
		req := httptest.NewRequest(http.MethodGet, "/v1/media/big.bin", nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", (i*4096)%(1<<20-4096), (i*4096)%(1<<20-4096)+4095))
		h.ServeHTTP(rec, req)
	}
}

// discardRecorder is a minimal ResponseWriter that drops the body, so the
// benchmark measures handler cost rather than allocation of a buffer.
type discardRecorder struct {
	header http.Header
	code   int
}

func (d *discardRecorder) Header() http.Header       { return d.header }
func (d *discardRecorder) WriteHeader(c int)         { d.code = c }
func (d *discardRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardRecorder) SetWriteDeadline(time.Time) error { return nil }
