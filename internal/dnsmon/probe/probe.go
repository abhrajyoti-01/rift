package probe

import (
	"context"
	"sort"
	"sync"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/resolver"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// ResolverProvider yields the resolver engines for a view. The authoritative
// view uses a different engine set (zone NS servers) than the recursive view
// (configured public resolvers), so the prober asks for engines by view.
type ResolverProvider interface {
	EnginesFor(view dnsmodel.View) []*resolver.Engine
}

// Config configures a prober.
type Config struct {
	View    dnsmodel.View
	Targets []dnsmodel.Target
	// MaxNSProbe caps how many authoritative nameservers are queried per
	// zone. The cap is an fd-safety measure, not a tuning knob.
	MaxNSProbe int
}

const defaultMaxNSProbe = 4

// Prober produces observations for its configured view.
type Prober struct {
	view       dnsmodel.View
	targets    []dnsmodel.Target
	engines    []*resolver.Engine
	maxNSProbe int
}

// New builds a prober for the configured view and targets.
func New(view dnsmodel.View) *Prober {
	return &Prober{view: view, maxNSProbe: defaultMaxNSProbe}
}

// NewWithConfig builds a prober with explicit targets and engines.
func NewWithConfig(cfg Config, engines []*resolver.Engine) *Prober {
	if cfg.MaxNSProbe <= 0 {
		cfg.MaxNSProbe = defaultMaxNSProbe
	}
	return &Prober{
		view:       cfg.View,
		targets:    cfg.Targets,
		engines:    engines,
		maxNSProbe: cfg.MaxNSProbe,
	}
}

// ProbeAll queries every configured target once and returns the
// observations. Failures become observations carrying an ErrClass rather
// than being dropped: a monitor must report gaps, not hide them.
func (p *Prober) ProbeAll(ctx context.Context) []dnsmodel.Observation {
	if len(p.engines) == 0 || len(p.targets) == 0 {
		return nil
	}
	out := make([]dnsmodel.Observation, 0, len(p.targets)*len(p.engines))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, target := range p.targets {
		// The target's View selects which engine set applies. A target with
		// no view is treated as belonging to this prober's view.
		if target.View != 0 && target.View != p.view {
			continue
		}
		for _, eng := range p.engines {
			wg.Add(1)
			go func(target dnsmodel.Target, eng *resolver.Engine) {
				defer wg.Done()
				obs := p.queryOne(ctx, target, eng)
				mu.Lock()
				out = append(out, obs)
				mu.Unlock()
			}(target, eng)
		}
	}
	wg.Wait()
	return out
}

// queryOne performs a single query and normalizes the result into an
// Observation.
func (p *Prober) queryOne(ctx context.Context, target dnsmodel.Target, eng *resolver.Engine) dnsmodel.Observation {
	q := wire.Question{
		Name:  canonicalName(target.Name),
		Type:  target.Type,
		Class: 1, // IN
	}

	obs := dnsmodel.Observation{
		View:      p.view,
		Resolver:  eng.ResolverAddr(),
		QName:     q.Name,
		QType:     q.Type,
		Transport: "udp",
		Timestamp: time.Now().UTC(),
	}

	msg, latency, err := eng.Query(ctx, q)
	obs.Latency = latency
	if err != nil {
		obs.ErrClass = errs.ClassOf(err)
		if obs.ErrClass == errs.ClassUnknown {
			obs.ErrClass = errs.ClassNetwork
		}
		return obs
	}

	obs.RCode = msg.RCode
	obs.Truncated = msg.TC
	if msg.TC {
		obs.Transport = "tcp"
	}
	obs.Answers = extractAnswers(msg, target.Type)
	return obs
}

// extractAnswers pulls answer records of the requested type (plus CNAMEs,
// which are always relevant to resolution) and canonicalizes them.
func extractAnswers(msg wire.Message, want wire.Type) []dnsmodel.Answer {
	var out []dnsmodel.Answer
	for _, rr := range msg.Answers {
		if rr.Type != want && rr.Type != wire.TypeCNAME {
			continue
		}
		data := rdataString(rr.Data)
		if data == "" {
			continue
		}
		out = append(out, dnsmodel.Answer{
			Name: canonicalName(rr.Name),
			Type: rr.Type,
			TTL:  rr.TTL,
			Data: data,
		})
	}
	// Canonical order makes fingerprints comparable across resolvers,
	// which is the entire basis of divergence detection.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Data < out[j].Data
	})
	return out
}

// rdataString renders RData canonically. Addresses are normalized through
// netip so textual variants compare equal.
func rdataString(rd wire.RData) string {
	switch v := rd.(type) {
	case wire.A:
		return v.Addr.String()
	case wire.AAAA:
		return v.Addr.String()
	case wire.CNAME:
		return canonicalName(v.Target)
	case wire.MX:
		return canonicalName(v.Host)
	case wire.NS:
		return canonicalName(v.Host)
	case wire.TXT:
		joined := ""
		for i, s := range v.Strings {
			if i > 0 {
				joined += " "
			}
			joined += s
		}
		return joined
	case wire.Unknown:
		return string(v.Raw)
	default:
		return ""
	}
}

// canonicalName lowercases and ensures a trailing dot so two resolvers'
// answers compare equal textually.
func canonicalName(name string) string {
	if name == "" {
		return ""
	}
	b := []byte(name)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	s := string(b)
	if s[len(s)-1] != '.' {
		s += "."
	}
	return s
}

// Fingerprint is the comparison unit for divergence: the sorted set of
// (type, data) pairs. TTL is deliberately excluded — TTL decay alone must
// never read as propagation divergence.
func Fingerprint(answers []dnsmodel.Answer) string {
	seen := make(map[string]struct{}, len(answers))
	keys := make([]string, 0, len(answers))
	for _, a := range answers {
		key := wire.TypeName(a.Type) + "|" + a.Data
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
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