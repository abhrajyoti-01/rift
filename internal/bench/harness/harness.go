package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	benchenv "github.com/abhrajyoti-01/rift/internal/bench/env"
	"github.com/abhrajyoti-01/rift/internal/bench/loadgen"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Baseline is the tolerance-band baseline for regression gating.
type Baseline struct {
	GeneratedAt string                    `json:"generated_at"`
	Metrics     map[string]BaselineMetric `json:"metrics"`
}

// BaselineMetric is one metric's expected value and permitted drift.
type BaselineMetric struct {
	Value     float64 `json:"value"`
	Tolerance float64 `json:"tolerance"`
	Unit      string  `json:"unit"`
	// HigherIsBetter flips the regression direction for throughput-like
	// metrics.
	HigherIsBetter bool `json:"higher_is_better,omitempty"`
}

// Run executes a named scenario, writes raw samples, and records the
// environment card alongside them.
func Run(ctx context.Context, scenarioDir, resultsDir, scenarioID string) (*loadgen.Result, error) {
	sc, err := loadgen.LoadScenario(scenarioDir, scenarioID)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassConfig, "bench.run", "load scenario")
	}
	if sc.Target == "" {
		return nil, errs.New(errs.ClassConfig, "bench.run", "scenario has no target: "+scenarioID)
	}

	card, err := benchenv.Probe()
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassResource, "bench.run", "environment probe")
	}

	outDir := filepath.Join(resultsDir, fmt.Sprintf("%s-%s", scenarioID, time.Now().UTC().Format("20060102T150405")))
	runner := loadgen.NewRunner(sc)
	runner.OutDir = outDir

	res, err := runner.Run(ctx)
	if err != nil {
		return res, errs.Wrap(err, errs.ClassResource, "bench.run", "run scenario")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return res, errs.Wrap(err, errs.ClassResource, "bench.run", "create results dir")
	}
	cardJSON, _ := card.JSON()
	if err := os.WriteFile(filepath.Join(outDir, "card.json"), cardJSON, 0o644); err != nil {
		return res, errs.Wrap(err, errs.ClassResource, "bench.run", "write card")
	}
	summary := map[string]any{
		"scenario":   res.Scenario.Name,
		"completed":  res.Completed,
		"errors":     res.Errors,
		"elapsed_ms": res.Elapsed.Milliseconds(),
		"p50_ns":     res.P50.Nanoseconds(),
		"p95_ns":     res.P95.Nanoseconds(),
		"p99_ns":     res.P99.Nanoseconds(),
		"max_ns":     res.Max.Nanoseconds(),
		"bytes":      res.Bytes,
	}
	summaryJSON, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "summary.json"), summaryJSON, 0o644); err != nil {
		return res, errs.Wrap(err, errs.ClassResource, "bench.run", "write summary")
	}
	return res, nil
}

// Compare evaluates a results summary against baseline tolerance bands.
// Out-of-band metrics produce an error so CI fails; improvements beyond band
// are reported for re-baselining rather than silently accepted.
func Compare(summaryPath, baselinePath string) (Comparison, error) {
	var summary map[string]any
	if err := readJSON(summaryPath, &summary); err != nil {
		return Comparison{}, errs.Wrap(err, errs.ClassConfig, "bench.compare", "read summary")
	}
	var baseline Baseline
	if err := readJSON(baselinePath, &baseline); err != nil {
		return Comparison{}, errs.Wrap(err, errs.ClassConfig, "bench.compare", "read baseline")
	}

	cmp := Comparison{}
	for name, bm := range baseline.Metrics {
		raw, ok := summary[name]
		if !ok {
			continue
		}
		actual, ok := toFloat(raw)
		if !ok {
			continue
		}
		delta := 0.0
		if bm.Value != 0 {
			delta = (actual - bm.Value) / bm.Value
		}
		band := bm.Tolerance
		verdict := "pass"
		switch {
		case delta > band:
			verdict = "regression"
			if bm.HigherIsBetter {
				verdict = "improvement"
			}
		case delta < -band:
			verdict = "improvement"
			if bm.HigherIsBetter {
				verdict = "regression"
			}
		}
		cmp.Metrics = append(cmp.Metrics, MetricComparison{
			Name: name, Baseline: bm.Value, Actual: actual, Delta: delta,
			Tolerance: band, Verdict: verdict, Unit: bm.Unit,
		})
	}
	sort.Slice(cmp.Metrics, func(i, j int) bool { return cmp.Metrics[i].Name < cmp.Metrics[j].Name })
	return cmp, nil
}

// Comparison is the compare output.
type Comparison struct {
	Metrics []MetricComparison `json:"metrics"`
}

// MetricComparison is one metric's measured drift.
type MetricComparison struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Actual    float64 `json:"actual"`
	Delta     float64 `json:"delta"`
	Tolerance float64 `json:"tolerance"`
	Verdict   string  `json:"verdict"`
	Unit      string  `json:"unit"`
}

// Regressions returns the metrics that breached their tolerance band.
func (c Comparison) Regressions() []MetricComparison {
	var out []MetricComparison
	for _, m := range c.Metrics {
		if m.Verdict == "regression" {
			out = append(out, m)
		}
	}
	return out
}

// Text renders the comparison.
func (c Comparison) Text() string {
	if len(c.Metrics) == 0 {
		return "no comparable metrics\n"
	}
	out := ""
	for _, m := range c.Metrics {
		out += fmt.Sprintf("%-20s baseline=%.4f actual=%.4f delta=%+.1f%% band=±%.0f%% %s\n",
			m.Name, m.Baseline, m.Actual, m.Delta*100, m.Tolerance*100, m.Verdict)
	}
	return out
}

// Report renders a benchmark report from a results directory. It REFUSES to
// run without an environment card: a number without its environment is not
// a fact.
func Report(samplesDir, outPath string) error {
	cardPath := filepath.Join(samplesDir, "card.json")
	if _, err := os.Stat(cardPath); err != nil {
		return errs.New(errs.ClassConfig, "bench.report",
			"refusing to produce a report without an environment card ("+cardPath+" missing)")
	}
	var card benchenv.Card
	if err := readJSON(cardPath, &card); err != nil {
		return errs.Wrap(err, errs.ClassConfig, "bench.report", "read card")
	}
	var summary map[string]any
	if err := readJSON(filepath.Join(samplesDir, "summary.json"), &summary); err != nil {
		return errs.Wrap(err, errs.ClassConfig, "bench.report", "read summary")
	}

	report := "# RIFT Benchmark Report\n\n"
	report += "## Environment card\n\n```\n" + card.Text() + "```\n\n"
	report += "## Results\n\n"
	keys := make([]string, 0, len(summary))
	for k := range summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	report += "| Metric | Value |\n|---|---|\n"
	for _, k := range keys {
		report += fmt.Sprintf("| %s | %v |\n", k, summary[k])
	}
	report += "\n## Reproduction\n\nRegenerate with:\n\n```\nrift bench report --samples " +
		samplesDir + "\n```\n"

	if outPath == "" {
		outPath = filepath.Join(samplesDir, "report.md")
	}
	if err := os.WriteFile(outPath, []byte(report), 0o644); err != nil {
		return errs.Wrap(err, errs.ClassResource, "bench.report", "write report")
	}
	return nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// ExitCodeForComparison maps a comparison to a process exit code: 0 when
// within bands, 1 (runtime fault) when regressions exist.
func ExitCodeForComparison(c Comparison) int {
	if len(c.Regressions()) > 0 {
		return 1
	}
	return 0
}

var errNoBaseline = errors.New("baseline missing")

// LoadBaseline reads a baseline file, returning errNoBaseline when absent so
// callers can distinguish "no baseline yet" from a malformed one.
func LoadBaseline(path string) (*Baseline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoBaseline
		}
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}
