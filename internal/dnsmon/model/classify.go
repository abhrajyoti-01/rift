package dnsmodel

import (
	"sort"
	"time"
)

// ClassifierConfig tunes propagation classification.
type ClassifierConfig struct {
	// MinResponding is the smallest number of distinct responsive resolvers
	// required before agreement means anything. Below it the honest answer
	// is "insufficient coverage", not "converged".
	MinResponding int
	// ConvergenceWindow is how long after an authoritative change partial
	// agreement is expected rather than alarming.
	ConvergenceWindow time.Duration
	// Window is the trailing observation window considered.
	Window time.Duration
}

// DefaultClassifierConfig returns the documented defaults.
func DefaultClassifierConfig() ClassifierConfig {
	return ClassifierConfig{
		MinResponding:     3,
		ConvergenceWindow: 5 * time.Minute,
		Window:            10 * time.Minute,
	}
}

func (c ClassifierConfig) withDefaults() ClassifierConfig {
	d := DefaultClassifierConfig()
	if c.MinResponding <= 0 {
		c.MinResponding = d.MinResponding
	}
	if c.ConvergenceWindow <= 0 {
		c.ConvergenceWindow = d.ConvergenceWindow
	}
	if c.Window <= 0 {
		c.Window = d.Window
	}
	return c
}

// Fingerprinter abstracts fingerprint computation so the model package does
// not depend on the probe package.
type Fingerprinter func(answers []Answer) string

// Classification is the classifier's full output.
type Classification struct {
	State PropagationState
	// Reference is the fingerprint taken from the freshest successful
	// authoritative observation.
	Reference string
	// ReferenceTime is when that observation was recorded.
	ReferenceTime time.Time
	// DivergentResolvers names the recursive resolvers whose fingerprint
	// differs from the reference.
	DivergentResolvers []string
	// RespondingResolvers counts distinct responsive recursive resolvers.
	RespondingResolvers int
	// DivergenceDuration is time since the authoritative reference changed.
	DivergenceDuration time.Duration
}

// Classify computes the propagation state for one target from its trailing
// observations.
//
// The classifier never emits a "propagated worldwide" verdict: it reports
// agreement across the resolvers it actually observed, and says
// "insufficient coverage" when it observed too few to mean anything.
func Classify(obs []Observation, cfg ClassifierConfig, fingerprint Fingerprinter, now time.Time) Classification {
	cfg = cfg.withDefaults()
	if fingerprint == nil {
		fingerprint = defaultFingerprint
	}

	cutoff := now.Add(-cfg.Window)
	var auth []Observation
	var rec []Observation
	for _, o := range obs {
		if o.Timestamp.Before(cutoff) {
			continue
		}
		if o.ErrClass != 0 || o.RCode != 0 {
			// Only NOERROR/err-free observations count as successful.
			if o.View == ViewAuthoritative {
				continue
			}
			continue
		}
		switch o.View {
		case ViewAuthoritative:
			auth = append(auth, o)
		case ViewRecursive:
			rec = append(rec, o)
		}
	}

	result := Classification{State: PropStateUnknown}

	if len(auth) == 0 {
		// No authoritative ground truth in the window: we cannot say what
		// is published, only that we cannot say.
		if len(rec) == 0 {
			return result
		}
		result.State = PropUnresolvable
		return result
	}

	// Freshest authoritative observation is the reference.
	sort.Slice(auth, func(i, j int) bool { return auth[i].Timestamp.After(auth[j].Timestamp) })
	ref := auth[0]
	result.Reference = fingerprint(ref.Answers)
	result.ReferenceTime = ref.Timestamp
	result.DivergenceDuration = now.Sub(ref.Timestamp)

	// Distinct responsive recursive resolvers and their fingerprints.
	type resState struct {
		fingerprint string
		newest      time.Time
	}
	byResolver := map[string]resState{}
	for _, o := range rec {
		cur, seen := byResolver[o.Resolver]
		if !seen || o.Timestamp.After(cur.newest) {
			byResolver[o.Resolver] = resState{
				fingerprint: fingerprint(o.Answers),
				newest:      o.Timestamp,
			}
		}
	}
	result.RespondingResolvers = len(byResolver)

	if result.RespondingResolvers < cfg.MinResponding {
		result.State = PropInsufficientCoverage
		return result
	}

	var divergent []string
	allMatch := true
	for name, st := range byResolver {
		if st.fingerprint != result.Reference {
			divergent = append(divergent, name)
			allMatch = false
		}
	}
	sort.Strings(divergent)
	result.DivergentResolvers = divergent

	switch {
	case allMatch:
		result.State = PropConverged
	case now.Sub(result.ReferenceTime) <= cfg.ConvergenceWindow:
		// Change is recent: partial agreement is expected, not alarming.
		result.State = PropConverging
	default:
		result.State = PropDivergent
	}
	return result
}

// defaultFingerprint is used when no Fingerprinter is supplied. It matches
// the probe package's definition: sorted (type, data) pairs, TTL excluded.
func defaultFingerprint(answers []Answer) string {
	keys := make([]string, 0, len(answers))
	seen := map[string]struct{}{}
	for _, a := range answers {
		k := a.Data
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += "\x00"
		}
		out += k
	}
	return out
}

// IsPropagatedWorldwide always returns false. It exists so that a caller
// reaching for a "worldwide" concept finds an explicit refusal instead of
// inventing one from the enum.
func (p PropagationState) IsPropagatedWorldwide() bool { return false }
