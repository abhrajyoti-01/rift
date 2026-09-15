package medmodel

import (
	"testing"
	"time"
)

func TestParseRangeMatrix(t *testing.T) {
	const size = 1000
	cases := []struct {
		header  string
		wantOK  bool
		want    RangeSpec
		outcome RangeParseOutcome
	}{
		{"", false, RangeSpec{}, RangeNone},
		{"bytes=0-0", true, RangeSpec{0, 1}, RangeOK},
		{"bytes=500-999", true, RangeSpec{500, 500}, RangeOK},
		{"bytes=500-", true, RangeSpec{500, 500}, RangeOK},
		{"bytes=-500", true, RangeSpec{500, 500}, RangeOK},
		{"bytes=-1", true, RangeSpec{999, 1}, RangeOK},
		{"bytes=0-", true, RangeSpec{0, 1000}, RangeOK},
		{"bytes=999-999", true, RangeSpec{999, 1}, RangeOK},
		// Beyond EOF clamps the end rather than failing.
		{"bytes=900-5000", true, RangeSpec{900, 100}, RangeOK},
		// Suffix larger than the representation selects all of it.
		{"bytes=-5000", true, RangeSpec{0, 1000}, RangeOK},
		// Unsatisfiable / malformed → 416.
		{"bytes=1000-", false, RangeSpec{}, RangeInvalid},
		{"bytes=1000-2000", false, RangeSpec{}, RangeInvalid},
		{"bytes=5-2", false, RangeSpec{}, RangeInvalid},
		{"bytes=-0", false, RangeSpec{}, RangeInvalid},
		{"bytes=abc", false, RangeSpec{}, RangeInvalid},
		{"bytes=", false, RangeSpec{}, RangeInvalid},
		{"items=0-10", false, RangeSpec{}, RangeInvalid},
		{"bytes=--5", false, RangeSpec{}, RangeInvalid},
		{"bytes=1-2-3", false, RangeSpec{}, RangeInvalid},
		{"bytes=+1-2", false, RangeSpec{}, RangeInvalid},
		{"bytes= 1 - 2 ", true, RangeSpec{1, 2}, RangeOK},
		// Multi-range is distinct from invalid.
		{"bytes=0-0,2-10", false, RangeSpec{}, RangeMulti},
		{"bytes=0-1, 5-6", false, RangeSpec{}, RangeMulti},
	}
	for _, c := range cases {
		got, ok, outcome := ParseRange(c.header, size)
		if outcome != c.outcome {
			t.Errorf("ParseRange(%q) outcome = %v, want %v", c.header, outcome, c.outcome)
			continue
		}
		if ok != c.wantOK {
			t.Errorf("ParseRange(%q) ok = %v, want %v", c.header, ok, c.wantOK)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseRange(%q) = %+v, want %+v", c.header, got, c.want)
		}
	}
}

func TestParseRangeEmptyRepresentation(t *testing.T) {
	// Any range over a 0-byte representation is unsatisfiable.
	for _, h := range []string{"bytes=0-", "bytes=0-0", "bytes=-1", "bytes=1-"} {
		if _, ok, outcome := ParseRange(h, 0); ok || outcome != RangeInvalid {
			t.Errorf("ParseRange(%q, 0) = ok=%v outcome=%v, want invalid", h, ok, outcome)
		}
	}
}

func TestParseRangeOverflow(t *testing.T) {
	// Numbers that overflow int64 must be invalid, not wrap.
	huge := "99999999999999999999999999"
	if _, ok, outcome := ParseRange("bytes="+huge+"-", 1000); ok || outcome != RangeInvalid {
		t.Errorf("overflowing start: ok=%v outcome=%v, want invalid", ok, outcome)
	}
}

func TestRangeSpecEnd(t *testing.T) {
	if got := (RangeSpec{Start: 100, Length: 50}).End(); got != 149 {
		t.Errorf("End() = %d, want 149", got)
	}
}

func TestStrongETagStabilityAndChange(t *testing.T) {
	mt := time.Unix(1700000000, 0)
	a := StrongETag(1000, mt)
	b := StrongETag(1000, mt)
	if a != b {
		t.Error("ETag must be stable for identical inputs")
	}
	if StrongETag(1001, mt) == a {
		t.Error("ETag must change with size")
	}
	if StrongETag(1000, mt.Add(time.Nanosecond)) == a {
		t.Error("ETag must change with mtime")
	}
	if a[0] != '"' || a[len(a)-1] != '"' {
		t.Errorf("ETag must be quoted: %q", a)
	}
}

func TestIndexLookup(t *testing.T) {
	ix := NewIndex(map[string]*Asset{
		"a.mp4": {Path: "/x/a.mp4", Size: 10},
	})
	if _, ok := ix.Lookup("a.mp4"); !ok {
		t.Error("expected hit for a.mp4")
	}
	if _, ok := ix.Lookup("../a.mp4"); ok {
		t.Error("index must not resolve traversal names")
	}
	if ix.Len() != 1 {
		t.Errorf("Len() = %d, want 1", ix.Len())
	}
}

func FuzzParseRange(f *testing.F) {
	for _, seed := range []string{
		"bytes=0-", "bytes=-500", "bytes=1-2", "bytes=abc", "bytes=0-0,1-1",
		"bytes=  ", "bytes=" + "9999999999999999999999" + "-", "",
	} {
		f.Add(seed, int64(1000))
	}
	f.Fuzz(func(t *testing.T, header string, size int64) {
		if size < 0 {
			size = -size
		}
		spec, ok, outcome := ParseRange(header, size)
		if !ok {
			return
		}
		// Invariants for any accepted range.
		if outcome != RangeOK {
			t.Fatalf("ok=true with outcome %v", outcome)
		}
		if spec.Length < 1 {
			t.Fatalf("accepted range with length %d", spec.Length)
		}
		if spec.Start < 0 {
			t.Fatalf("accepted range with negative start %d", spec.Start)
		}
		if spec.Start+spec.Length > size {
			t.Fatalf("accepted range %d+%d exceeds size %d", spec.Start, spec.Length, size)
		}
	})
}
