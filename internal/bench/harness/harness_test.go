package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReportRefusesWithoutCard is the honesty gate: a report without its
// environment is not a fact, so producing one must fail.
func TestReportRefusesWithoutCard(t *testing.T) {
	dir := t.TempDir()
	// summary.json present, card.json deliberately absent.
	writeJSON(t, filepath.Join(dir, "summary.json"), map[string]any{"completed": 1})
	err := Report(dir, "")
	if err == nil {
		t.Fatal("Report must refuse to run without an environment card")
	}
	if !containsSub(err.Error(), "environment card") {
		t.Errorf("refusal should name the missing card: %v", err)
	}
}

func TestReportRendersWithCard(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "card.json"), map[string]any{
		"generated_at": "2026-01-01T00:00:00Z",
		"os":           "linux",
		"go_version":   "go1.25.5",
		"num_cpu":      8,
	})
	writeJSON(t, filepath.Join(dir, "summary.json"), map[string]any{
		"completed": 1000,
		"p99_ns":    123456,
	})
	if err := Report(dir, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"Environment card", "go1.25.5", "completed", "Reproduction"} {
		if !containsSub(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

func TestCompareFlagsRegression(t *testing.T) {
	dir := t.TempDir()
	summaryPath := filepath.Join(dir, "summary.json")
	baselinePath := filepath.Join(dir, "baseline.json")

	writeJSON(t, summaryPath, map[string]any{"p99_ns": float64(2000)})
	writeJSON(t, baselinePath, Baseline{
		Metrics: map[string]BaselineMetric{
			"p99_ns": {Value: 1000, Tolerance: 0.10, Unit: "ns"},
		},
	})

	cmp, err := Compare(summaryPath, baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Regressions()) != 1 {
		t.Fatalf("expected 1 regression, got %d (%+v)", len(cmp.Regressions()), cmp.Metrics)
	}
	if ExitCodeForComparison(cmp) != 1 {
		t.Error("a regression must yield a failing exit code")
	}
}

func TestCompareWithinBand(t *testing.T) {
	dir := t.TempDir()
	summaryPath := filepath.Join(dir, "summary.json")
	baselinePath := filepath.Join(dir, "baseline.json")

	// 5% over a ±10% band is within tolerance.
	writeJSON(t, summaryPath, map[string]any{"p99_ns": float64(1050)})
	writeJSON(t, baselinePath, Baseline{
		Metrics: map[string]BaselineMetric{
			"p99_ns": {Value: 1000, Tolerance: 0.10, Unit: "ns"},
		},
	})
	cmp, err := Compare(summaryPath, baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Regressions()) != 0 {
		t.Errorf("within-band metric flagged as regression: %+v", cmp.Metrics)
	}
	if ExitCodeForComparison(cmp) != 0 {
		t.Error("within-band comparison must exit 0")
	}
}

func TestCompareHigherIsBetterDirection(t *testing.T) {
	dir := t.TempDir()
	summaryPath := filepath.Join(dir, "summary.json")
	baselinePath := filepath.Join(dir, "baseline.json")

	// Throughput dropped 50%: that is a regression, not an improvement.
	writeJSON(t, summaryPath, map[string]any{"rps": float64(500)})
	writeJSON(t, baselinePath, Baseline{
		Metrics: map[string]BaselineMetric{
			"rps": {Value: 1000, Tolerance: 0.10, Unit: "req/s", HigherIsBetter: true},
		},
	})
	cmp, err := Compare(summaryPath, baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Regressions()) != 1 {
		t.Errorf("throughput drop must be a regression: %+v", cmp.Metrics)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}