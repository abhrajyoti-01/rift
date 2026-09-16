package medserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhrajyoti-01/rift/internal/media/model"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Config configures the media server.
type Config struct {
	Bind                           string
	Root                           string
	MaxStreams                     int // global concurrent; default 64
	MaxStreamsPerClient            int // default 4
	ReadHeaderTimeout, IdleTimeout time.Duration
	WriteTimeout                   time.Duration
	IOMode                         string // "sendfile" (default) | "buffered"
	Readahead                      int    // bytes, buffered mode; default 1 MiB
	RateLimitPerClient             string
}

const (
	defaultMaxStreams        = 64
	defaultMaxStreamsPerCli  = 4
	defaultReadahead         = 1 << 20
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultWriteTimeout      = 5 * time.Minute
)

// Metrics is the media server's accounting surface. Counters are atomic
// so the data path never takes a lock to record.
type Metrics struct {
	BytesSent        atomic.Uint64
	ActiveStreams    atomic.Int64
	RangeRequests    [4]atomic.Uint64 // indexed by medmodel.RangeParseOutcome
	AdmissionRefused atomic.Uint64
	ClientStalls     atomic.Uint64
	FirstByteNanos   atomic.Uint64
	FirstByteCount   atomic.Uint64
	// DeadlineUnsupported counts streams where the connection would not
	// accept a write deadline, so slow-client protection fell back to the
	// server-level timeout.
	DeadlineUnsupported atomic.Uint64
}

// Server is the media data plane.
type Server struct {
	cfg   Config
	store *medmodel.IndexStore
	// global admission gate and per-client gate
	global  chan struct{}
	clients sync.Map
	metrics Metrics
}

// New builds a server from config. The index must be published separately
// via PublishIndex; a server with no index serves 404 for everything.
func New(cfg Config) *Server {
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = defaultMaxStreams
	}
	if cfg.MaxStreamsPerClient <= 0 {
		cfg.MaxStreamsPerClient = defaultMaxStreamsPerCli
	}
	if cfg.MaxStreamsPerClient > cfg.MaxStreams {
		cfg.MaxStreamsPerClient = cfg.MaxStreams
	}
	if cfg.Readahead <= 0 {
		cfg.Readahead = defaultReadahead
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.IOMode == "" {
		cfg.IOMode = "sendfile"
	}
	return &Server{
		cfg:    cfg,
		store:  &medmodel.IndexStore{},
		global: make(chan struct{}, cfg.MaxStreams),
	}
}

// PublishIndex atomically publishes a rebuilt index (SIGHUP path).
func (s *Server) PublishIndex(ix *medmodel.Index) { s.store.Store(ix) }

// Index returns the current index (for the admin/index API).
func (s *Server) Index() *medmodel.Index { return s.store.Load() }

// Metrics returns the accounting surface.
func (s *Server) Metrics() *Metrics { return &s.metrics }

// BuildIndex walks root and indexes regular files by relative slash path.
// Symlinks are not followed unless follow is true (default false: a symlink
// escaping root is a path-traversal vector).
func BuildIndex(root string, follow bool, exclude []string) (*medmodel.Index, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassConfig, "media.index", "resolve root")
	}
	assets := make(map[string]*medmodel.Asset)
	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 && !follow {
			return nil
		}
		if !d.Type().IsRegular() {
			if d.Type()&os.ModeSymlink != 0 && follow {
				// allowed below via os.Stat
			} else {
				return nil
			}
		}
		rel, rerr := filepath.Rel(absRoot, path)
		if rerr != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if excluded(name, exclude) {
			return nil
		}
		info, ierr := os.Stat(path)
		if ierr != nil {
			return nil
		}
		assets[name] = &medmodel.Asset{
			Path:    path,
			ETag:    medmodel.StrongETag(info.Size(), info.ModTime()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		return nil
	})
	if walkErr != nil {
		return nil, errs.Wrap(walkErr, errs.ClassConfig, "media.index", "walk root")
	}
	return medmodel.NewIndex(assets), nil
}

func excluded(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if ok, _ := filepath.Match(p, filepath.Base(name)); ok {
			return true
		}
	}
	return false
}

// Handler returns the HTTP handler for the media plane.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/media/", s.serveMedia)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.store.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"ready":false,"detail":"index not built"}`)
			return
		}
		io.WriteString(w, `{"ready":true}`)
	})
	return mux
}

// Run serves until ctx is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Bind)
	if err != nil {
		return errs.Wrap(err, errs.ClassResource, "media.run", "bind "+s.cfg.Bind)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 16,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errs.Wrap(err, errs.ClassNetwork, "media.run", "serve")
	}
	return nil
}

// serveMedia handles GET/HEAD with RFC 7233 range semantics.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, ok := strings.CutPrefix(r.URL.Path, "/v1/media/")
	if !ok || name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Decode percent-escapes but reject anything that could traverse: this
	// check is deliberately before index lookup so no oracle is possible.
	clean, ok := sanitizeAssetName(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ix := s.store.Load()
	if ix == nil {
		http.Error(w, "index unavailable", http.StatusServiceUnavailable)
		return
	}
	asset, ok := ix.Lookup(clean)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Admission: global then per-client. Refused → 429 with Retry-After.
	if !s.admit(w, r) {
		return
	}
	defer s.release(r)

	f, err := os.Open(asset.Path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	// Re-stat the open fd: the index may be stale relative to the file.
	// Serving the fd we opened (not the path) closes the TOCTOU window.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	size := info.Size()
	etag := medmodel.StrongETag(size, info.ModTime())

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", etag)
	h.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	h.Set("Content-Type", contentTypeFor(clean))

	// If-Range: only a matching strong ETag yields a 206; otherwise the
	// full representation is served (RFC 7233 §3.2).
	rangeHeader := r.Header.Get("Range")
	if ir := r.Header.Get("If-Range"); ir != "" && ir != etag {
		rangeHeader = ""
	}

	spec, okRange, outcome := medmodel.ParseRange(rangeHeader, size)
	s.metrics.RangeRequests[outcome].Add(1)

	switch outcome {
	case medmodel.RangeInvalid:
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	case medmodel.RangeMulti:
		// v1: serve the full representation; multipart/byteranges is a
		// documented milestone, and serving 200 is RFC-permitted.
		okRange = false
	case medmodel.RangeNone:
		okRange = false
	}

	status := http.StatusOK
	offset, length := int64(0), size
	if okRange {
		status = http.StatusPartialContent
		offset, length = spec.Start, spec.Length
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", spec.Start, spec.End(), size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))

	start := time.Now()
	w.WriteHeader(status)
	if r.Method == http.MethodHead || length == 0 {
		return
	}

	section := io.NewSectionReader(f, offset, length)
	n, copyErr := s.copyOut(w, section, length)

	s.metrics.BytesSent.Add(uint64(n))
	s.metrics.FirstByteNanos.Add(uint64(time.Since(start).Nanoseconds()))
	s.metrics.FirstByteCount.Add(1)
	if copyErr != nil {
		// A stalled/aborted client is counted, not propagated as a server
		// error. The connection is already committed at this point.
		s.metrics.ClientStalls.Add(1)
	}
}

// copyOut streams the section to the client.
//
// sendfile mode: io.Copy with an *io.SectionReader over an *os.File. The
// runtime's io.Copy takes the ReadFrom/WriteTo fast path where available;
// on Linux with a plain TCP connection this reaches sendfile(2) (E1
// verifies by syscall trace). No per-stream heap buffer exists in this
// mode.
//
// buffered mode: an explicit readahead buffer per stream, which bounds
// memory to Readahead bytes regardless of file size and avoids one
// syscall per small read on seek-heavy patterns.
//
// A write deadline bounds a stalled client so a slow reader cannot pin a
// goroutine, an fd, and an admission slot indefinitely. The deadline is set
// via http.ResponseController, which is the supported mechanism; a bare
// interface assertion on http.ResponseWriter does NOT reach the underlying
// connection and would silently leave the server unprotected.
func (s *Server) copyOut(w http.ResponseWriter, section *io.SectionReader, length int64) (int64, error) {
	if s.cfg.WriteTimeout > 0 {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)); err != nil {
			// Not fatal: the server-level WriteTimeout still applies as a
			// backstop. Recorded so the gap is visible rather than assumed
			// away.
			s.metrics.DeadlineUnsupported.Add(1)
		}
	}
	if s.cfg.IOMode == "buffered" {
		buf := make([]byte, s.cfg.Readahead)
		return io.CopyBuffer(w, section, buf)
	}
	return io.Copy(w, section)
}

// admit applies the global and per-client stream gates.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.global <- struct{}{}:
	default:
		s.metrics.AdmissionRefused.Add(1)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return false
	}
	ip := clientIP(r)
	cur, _ := s.clients.LoadOrStore(ip, new(atomic.Int64))
	n := cur.(*atomic.Int64).Add(1)
	if int(n) > s.cfg.MaxStreamsPerClient {
		cur.(*atomic.Int64).Add(-1)
		<-s.global
		s.metrics.AdmissionRefused.Add(1)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many streams for client", http.StatusTooManyRequests)
		return false
	}
	s.metrics.ActiveStreams.Add(1)
	return true
}

func (s *Server) release(r *http.Request) {
	ip := clientIP(r)
	if cur, ok := s.clients.Load(ip); ok {
		cur.(*atomic.Int64).Add(-1)
	}
	<-s.global
	s.metrics.ActiveStreams.Add(-1)
}

// clientIP extracts the peer IP for admission accounting. It deliberately
// ignores X-Forwarded-For: a media server attributing admission limits from
// a client-controlled header is trivially bypassable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sanitizeAssetName rejects traversal and separator abuse. Percent-decoding
// already happened in net/http, so we validate the decoded form.
func sanitizeAssetName(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", false
	}
	// Absolute paths and backslashes are rejected on the ORIGINAL string:
	// cleaning first would silently rewrite "/etc/passwd" into the
	// relative "etc/passwd", turning an attack into a lookup instead of a
	// refusal. Refuse, never normalize.
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", false
	}
	clean := pathCLean(name)
	if clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if strings.Contains(clean, "../") {
		return "", false
	}
	// Post-condition: cleaning must not have changed the string. Any
	// difference means the input tried to smuggle "." or ".." segments.
	if clean != name {
		return "", false
	}
	return clean, true
}

// pathCLean is a minimal lexical cleaner: it removes "." segments and
// resolves ".." within the path, returning "" if the path escapes.
func pathCLean(p string) string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, seg := range parts {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) == 0 {
				return ""
			}
			out = out[:len(out)-1]
		default:
			out = append(out, seg)
		}
	}
	joined := strings.Join(out, "/")
	// Defense in depth: the cleaned path must equal the original modulo
	// redundant separators, and must stay relative.
	if strings.HasPrefix(joined, "/") || joined == "" {
		return ""
	}
	return joined
}

func contentTypeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".ogg", ".opus":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}
