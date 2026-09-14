// Package model holds the media server data contracts (TECHNICAL_SPEC
// §7.1): Asset, Index, RangeSpec and the RFC 7233 semantics. Invalid
// ranges produce 416 — not a silent 200 fallback; that distinction is a
// spec-level decision (MEDIA_SERVER_SPEC §2).
package medmodel

import "time"

// Asset is one indexed media file: logical name, strong ETag, size, mtime.
// Built from filesystem metadata only (AD-9) — no container parsing.
type Asset struct {
	Path    string
	ETag    string // strong: quoted hex(size)-hex(mtime.UnixNano())
	Size    int64
	ModTime time.Time
}

// Index maps logical names to Assets behind an atomic.Pointer swap on
// rebuild (same pattern as the LB snapshot, AD-6): requests never see a
// partial index.
type Index struct {
	// Phase 4: map[string]*Asset + RWMutex (cold paths only).
}

// Lookup resolves a logical name to an Asset.
func (ix *Index) Lookup(name string) (*Asset, bool) {
	// Phase 4 (ROADMAP.md).
	_ = ix
	_ = name
	return nil, false
}

// RangeParseOutcome classifies a Range header parse.
type RangeParseOutcome uint8

const (
	// RangeNone: header absent → full 200.
	RangeNone RangeParseOutcome = iota
	// RangeOK: single range parsed.
	RangeOK
	// RangeMulti: multipart — v1 ignores, serves 200 full, counted.
	RangeMulti
	// RangeInvalid: unsatisfiable/invalid → 416.
	RangeInvalid
)

// RangeSpec is the normalized single window: Start ∈ [0,size), Length ≥ 1,
// Start+Length ≤ size.
type RangeSpec struct {
	Start, Length int64
}

// ParseRange parses a Range header against a content size.
func ParseRange(header string, size int64) (RangeSpec, bool, RangeParseOutcome) {
	// Phase 4 (ROADMAP.md).
	_, _ = header, size
	return RangeSpec{}, false, RangeNone
}
