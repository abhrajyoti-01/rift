package medmodel

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Asset is one indexed media file: logical name, strong ETag, size, mtime.
// Built from filesystem metadata only — no container parsing.
type Asset struct {
	Path    string
	ETag    string // strong: quoted hex(size)-hex(mtime.UnixNano())
	Size    int64
	ModTime time.Time
}

// Index maps logical names to Assets. The map is immutable once published
// behind Store; rebuilds swap a whole new Index atomically so requests never
// see a partial index.
type Index struct {
	m map[string]*Asset
}

// NewIndex builds an Index from a name→Asset map. The caller must not
// mutate the map afterwards.
func NewIndex(assets map[string]*Asset) *Index {
	return &Index{m: assets}
}

// Lookup resolves a logical name to an Asset.
func (ix *Index) Lookup(name string) (*Asset, bool) {
	if ix == nil || ix.m == nil {
		return nil, false
	}
	a, ok := ix.m[name]
	return a, ok
}

// Len reports how many assets are indexed.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.m)
}

// IndexStore holds the published Index behind an atomic pointer, giving
// lock-free reads across rebuilds.
type IndexStore struct {
	p atomic.Pointer[Index]
}

// Store publishes a new index atomically.
func (s *IndexStore) Store(ix *Index) { s.p.Store(ix) }

// Load returns the current index (may be nil before the first Store).
func (s *IndexStore) Load() *Index { return s.p.Load() }

// RangeParseOutcome classifies a Range header parse.
type RangeParseOutcome uint8

const (
	// RangeNone: header absent → full 200.
	RangeNone RangeParseOutcome = iota
	// RangeOK: single range parsed.
	RangeOK
	// RangeMulti: multipart — v1 serves the full body with 200 and counts it.
	RangeMulti
	// RangeInvalid: malformed or unsatisfiable → 416.
	RangeInvalid
)

// RangeSpec is the normalized single window: Start ∈ [0,size),
// Length ≥ 1, Start+Length ≤ size.
type RangeSpec struct {
	Start, Length int64
}

// End returns the inclusive last byte offset of the window.
func (r RangeSpec) End() int64 { return r.Start + r.Length - 1 }

// ParseRange parses a Range header against a content size.
//
// Semantics follow RFC 7233 and are deliberately strict where tools
// commonly get it wrong:
//   - no header            → RangeNone (serve 200 full)
//   - more than one range  → RangeMulti (v1 serves 200 full, counted)
//   - syntactically bad    → RangeInvalid (416; RFC 7233 §4.4 — NOT 200)
//   - beyond EOF / -0 / s>e→ RangeInvalid (416)
//
// A size of 0 makes every range unsatisfiable, which is correct: there is
// no satisfiable byte range over an empty representation.
func ParseRange(header string, size int64) (RangeSpec, bool, RangeParseOutcome) {
	header = strings.TrimSpace(header)
	if header == "" {
		return RangeSpec{}, false, RangeNone
	}

	// Only the bytes unit is supported; anything else is not a range
	// request we can satisfy (RFC 7233 §3.1 — "other-range-set").
	const unit = "bytes="
	if len(header) < len(unit) || !strings.EqualFold(header[:len(unit)], unit) {
		return RangeSpec{}, false, RangeInvalid
	}
	spec := strings.TrimSpace(header[len(unit):])
	if spec == "" {
		return RangeSpec{}, false, RangeInvalid
	}

	// Multiple ranges: report distinctly; the caller decides (v1: 200).
	if strings.Contains(spec, ",") {
		return RangeSpec{}, false, RangeMulti
	}

	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return RangeSpec{}, false, RangeInvalid
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	if size <= 0 {
		// No satisfiable range exists for an empty representation.
		return RangeSpec{}, false, RangeInvalid
	}

	// Suffix range: "-N" = final N bytes.
	if startStr == "" {
		n, err := parseNonNeg(endStr)
		if err != nil || n == 0 {
			// "-0" selects zero bytes: unsatisfiable (RFC 7233 §2.1).
			return RangeSpec{}, false, RangeInvalid
		}
		if n > size {
			n = size
		}
		return RangeSpec{Start: size - n, Length: n}, true, RangeOK
	}

	start, err := parseNonNeg(startStr)
	if err != nil {
		return RangeSpec{}, false, RangeInvalid
	}
	if start >= size {
		// start beyond EOF: unsatisfiable.
		return RangeSpec{}, false, RangeInvalid
	}

	// Open-ended "N-": to EOF.
	if endStr == "" {
		return RangeSpec{Start: start, Length: size - start}, true, RangeOK
	}

	end, err := parseNonNeg(endStr)
	if err != nil {
		return RangeSpec{}, false, RangeInvalid
	}
	if end < start {
		// "bytes=5-2": malformed per RFC 7233 §2.1.
		return RangeSpec{}, false, RangeInvalid
	}
	if end >= size {
		end = size - 1
	}
	return RangeSpec{Start: start, Length: end - start + 1}, true, RangeOK
}

// parseNonNeg parses a non-negative decimal with no sign, no plus, and no
// leading whitespace. Empty is an error. Values that overflow int64 are an
// error rather than a wraparound.
func parseNonNeg(s string) (int64, error) {
	if s == "" {
		return 0, errBadNumber
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errBadNumber
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

type rangeErr string

func (e rangeErr) Error() string { return string(e) }

const errBadNumber = rangeErr("invalid range number")

// StrongETag builds the strong validator used for If-Range and caching:
// quoted hex(size)-hex(mtime nanos). Cheap, unique per representation
// change, and strong (so If-Range may use it).
func StrongETag(size int64, mtime time.Time) string {
	return `"` + strconv.FormatInt(size, 16) + "-" + strconv.FormatInt(mtime.UnixNano(), 16) + `"`
}