package dnsmodel

import (
	"testing"
	"time"

	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

func obs(view View, resolver string, answers []string, at time.Time) Observation {
	o := Observation{
		View:      view,
		Resolver:  resolver,
		QName:     "www.example.com.",
		QType:     wire.TypeA,
		Timestamp: at,
	}
	for _, a := range answers {
		o.Answers = append(o.Answers, Answer{Name: "www.example.com.", Type: wire.TypeA, Data: a, TTL: 300})
	}
	return o
}

func fp(answers []Answer) string {
	if len(answers) == 0 {
		return "-"
	}
	out := ""
	for i, a := range answers {
		if i > 0 {
			out += ","
		}
		out += a.Data
	}
	return out
}

func TestClassifyUnknownOnEmptyWindow(t *testing.T) {
	now := time.Now()
	res := Classify(nil, DefaultClassifierConfig(), fp, now)
	if res.State != PropStateUnknown {
		t.Errorf("empty window: state = %v, want unknown", res.State)
	}
}

func TestClassifyUnresolvableWhenNoAuthoritative(t *testing.T) {
	// We have recursive data but no authoritative ground truth: we cannot
	// say what is published, so the honest state is unresolvable.
	now := time.Now()
	obsList := []Observation{
		obs(ViewRecursive, "1.1.1.1:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "9.9.9.9:53", []string{"1.2.3.4"}, now),
	}
	res := Classify(obsList, DefaultClassifierConfig(), fp, now)
	if res.State != PropUnresolvable {
		t.Errorf("no authoritative data: state = %v, want unresolvable", res.State)
	}
}

func TestClassifyInsufficientCoverage(t *testing.T) {
	// Only TWO responsive resolvers with min_responding 3: agreement across
	// two resolvers means nothing, and the classifier must say so.
	now := time.Now()
	obsList := []Observation{
		obs(ViewAuthoritative, "ns1.example.com:53", []string{"1.2.3.4"}, now.Add(-time.Minute)),
		obs(ViewRecursive, "1.1.1.1:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now),
	}
	res := Classify(obsList, ClassifierConfig{MinResponding: 3, Window: time.Hour}, fp, now)
	if res.State != PropInsufficientCoverage {
		t.Errorf("state = %v, want insufficient_coverage", res.State)
	}
}

func TestClassifyConverged(t *testing.T) {
	now := time.Now()
	obsList := []Observation{
		obs(ViewAuthoritative, "ns1.example.com:53", []string{"1.2.3.4"}, now.Add(-30*time.Minute)),
		obs(ViewRecursive, "1.1.1.1:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "9.9.9.9:53", []string{"1.2.3.4"}, now),
	}
	res := Classify(obsList, ClassifierConfig{MinResponding: 3, Window: time.Hour}, fp, now)
	if res.State != PropConverged {
		t.Errorf("state = %v, want converged (divergent=%v)", res.State, res.DivergentResolvers)
	}
	if res.RespondingResolvers != 3 {
		t.Errorf("responding resolvers = %d, want 3", res.RespondingResolvers)
	}
}

func TestClassifyDivergentBeyondWindow(t *testing.T) {
	// Authoritative changed long ago but one resolver still serves the old
	// answer: that is divergence, not convergence.
	now := time.Now()
	obsList := []Observation{
		obs(ViewAuthoritative, "ns1.example.com:53", []string{"9.9.9.9"}, now.Add(-30*time.Minute)),
		obs(ViewRecursive, "1.1.1.1:53", []string{"9.9.9.9"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now), // stale
		obs(ViewRecursive, "9.9.9.9:53", []string{"9.9.9.9"}, now),
	}
	cfg := ClassifierConfig{MinResponding: 3, ConvergenceWindow: 5 * time.Minute, Window: time.Hour}
	res := Classify(obsList, cfg, fp, now)
	if res.State != PropDivergent {
		t.Errorf("state = %v, want divergent", res.State)
	}
	if len(res.DivergentResolvers) != 1 || res.DivergentResolvers[0] != "8.8.8.8:53" {
		t.Errorf("divergent resolvers = %v, want [8.8.8.8:53]", res.DivergentResolvers)
	}
}

func TestClassifyConvergingWithinWindow(t *testing.T) {
	// The authoritative change happened moments ago: partial agreement is
	// expected, so the state is converging rather than alarming.
	now := time.Now()
	obsList := []Observation{
		obs(ViewAuthoritative, "ns1.example.com:53", []string{"9.9.9.9"}, now.Add(-30*time.Second)),
		obs(ViewRecursive, "1.1.1.1:53", []string{"9.9.9.9"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "9.9.9.9:53", []string{"9.9.9.9"}, now),
	}
	cfg := ClassifierConfig{MinResponding: 3, ConvergenceWindow: 5 * time.Minute, Window: time.Hour}
	res := Classify(obsList, cfg, fp, now)
	if res.State != PropConverging {
		t.Errorf("state = %v, want converging (change 30s ago)", res.State)
	}
}

func TestClassifyIgnoresFailedObservations(t *testing.T) {
	// A resolver that timed out must not count as a responsive resolver,
	// and must not be treated as divergent.
	now := time.Now()
	failed := obs(ViewRecursive, "1.1.1.1:53", nil, now)
	failed.ErrClass = errs.ClassTimeout

	obsList := []Observation{
		obs(ViewAuthoritative, "ns1.example.com:53", []string{"1.2.3.4"}, now.Add(-time.Minute)),
		failed,
		obs(ViewRecursive, "8.8.8.8:53", []string{"1.2.3.4"}, now),
		obs(ViewRecursive, "9.9.9.9:53", []string{"1.2.3.4"}, now),
	}
	cfg := ClassifierConfig{MinResponding: 3, Window: time.Hour}
	res := Classify(obsList, cfg, fp, now)
	if res.RespondingResolvers != 2 {
		t.Errorf("responding = %d, want 2 (timeout must not count)", res.RespondingResolvers)
	}
	if res.State != PropInsufficientCoverage {
		t.Errorf("state = %v, want insufficient_coverage", res.State)
	}
	for _, d := range res.DivergentResolvers {
		if d == "1.1.1.1:53" {
			t.Error("a timed-out resolver must not be reported as divergent")
		}
	}
}

func TestClassifyUsesFreshestAuthoritativeAsReference(t *testing.T) {
	// Two authoritative observations disagree (mid-change); the fresher one
	// is the reference.
	now := time.Now()
	obsList := []Observation{
		obs(ViewAuthoritative, "nsOLD.example.com:53", []string{"1.1.1.1"}, now.Add(-10*time.Minute)),
		obs(ViewAuthoritative, "nsNEW.example.com:53", []string{"2.2.2.2"}, now.Add(-10*time.Second)),
		obs(ViewRecursive, "1.1.1.1:53", []string{"2.2.2.2"}, now),
		obs(ViewRecursive, "8.8.8.8:53", []string{"2.2.2.2"}, now),
		obs(ViewRecursive, "9.9.9.9:53", []string{"2.2.2.2"}, now),
	}
	cfg := ClassifierConfig{MinResponding: 3, Window: time.Hour, ConvergenceWindow: time.Minute}
	res := Classify(obsList, cfg, fp, now)
	if res.State != PropConverged {
		t.Errorf("state = %v, want converged against the freshest NS", res.State)
	}
}

// TestIsPropagatedWorldwideAlwaysFalse is the honesty gate: no API in this
// package may ever claim worldwide propagation.
func TestIsPropagatedWorldwideAlwaysFalse(t *testing.T) {
	for s := PropagationState(0); s <= PropConverged; s++ {
		if s.IsPropagatedWorldwide() {
			t.Fatalf("state %v claims worldwide propagation", s)
		}
	}
}

func TestPropagationStateStringClosedSet(t *testing.T) {
	want := map[PropagationState]string{
		PropStateUnknown:         "unknown",
		PropInsufficientCoverage: "insufficient_coverage",
		PropUnresolvable:         "unresolvable",
		PropDivergent:            "divergent",
		PropConverging:           "converging",
		PropConverged:            "converged",
	}
	for s, w := range want {
		if got := s.String(); got != w {
			t.Errorf("state %d String() = %q, want %q", s, got, w)
		}
	}
	if got := PropagationState(200).String(); got != "unknown" {
		t.Errorf("out-of-range String() = %q, want unknown", got)
	}
}

func TestFingerprintExcludesTTL(t *testing.T) {
	// Same addresses, different TTLs: fingerprints must match, because TTL
	// decay alone is not divergence.
	a := []Answer{{Name: "x.", Type: wire.TypeA, Data: "1.1.1.1", TTL: 300}}
	b := []Answer{{Name: "x.", Type: wire.TypeA, Data: "1.1.1.1", TTL: 42}}
	if defaultFingerprint(a) != defaultFingerprint(b) {
		t.Error("TTL must not affect the fingerprint")
	}
}

func TestFingerprintOrderIndependent(t *testing.T) {
	a := []Answer{
		{Name: "x.", Type: wire.TypeA, Data: "1.1.1.1"},
		{Name: "x.", Type: wire.TypeA, Data: "2.2.2.2"},
	}
	b := []Answer{
		{Name: "x.", Type: wire.TypeA, Data: "2.2.2.2"},
		{Name: "x.", Type: wire.TypeA, Data: "1.1.1.1"},
	}
	if defaultFingerprint(a) != defaultFingerprint(b) {
		t.Error("fingerprint must be order-independent")
	}
}
