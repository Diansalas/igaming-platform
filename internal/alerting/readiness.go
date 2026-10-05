package alerting

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// Routing readiness (ALERT-DELIVERY-1, ADR 0102 section 18).
//
// Readiness answers one question: "if a p1 or p2 alert fired now, is there a
// CONFIGURED path that would notify a PERSON?" It is computed over
// configuration ONLY - the current routes, the human_notification flag of the
// channel kind, and which sinks this binary has wired. It never looks at live
// delivery success (a vendor outage must not flip it; security L-6), and a
// cached evaluation older than ReadinessStaleAfter counts as NOT READY.
//
// A log or mock route never counts: those channels answer
// HumanNotification()==false. With no human-notification channel in the
// vocabulary, readiness is ALWAYS false today. That is the correct,
// fail-closed answer and is reported, not hidden. /readyz stays REPORT-ONLY.

// ReadinessReason is the closed set of reasons a severity is not ready.
type ReadinessReason string

const (
	ReadinessReady         ReadinessReason = "ready"
	ReadinessNoRoute       ReadinessReason = "no_route_configured"
	ReadinessRouteDisabled ReadinessReason = "route_not_enabled"
	ReadinessNotHuman      ReadinessReason = "channel_not_human_notification"
	ReadinessNotWired      ReadinessReason = "channel_not_wired"
	ReadinessStale         ReadinessReason = "evaluation_stale"
	ReadinessNotEvaluated  ReadinessReason = "not_evaluated"
)

// ReadinessSeverities are the severities readiness is required for. p3 is
// reported by the status API but never gates readiness.
var ReadinessSeverities = []Severity{SeverityP1, SeverityP2}

// MissingOperatorInput names, in fixed text, the exact human input that is
// missing for a not-ready reason. It names no recipient, vendor or secret.
func MissingOperatorInput(r ReadinessReason) string {
	switch r {
	case ReadinessReady:
		return ""
	case ReadinessNoRoute:
		return "H2: a step-0 route naming the on-call recipient (HD-PRH2-4-OPS) has not been configured for this severity"
	case ReadinessRouteDisabled:
		return "H2/H3: the step-0 route exists but is not enabled; enabling needs a recipient and the on-call policy"
	case ReadinessNotHuman:
		return "H1: no channel kind that notifies a person exists (only log and mock, which do not); needs a vendor decision, contract and adapter, plus four-eyes route approval (SR-7)"
	case ReadinessNotWired:
		return "H1/H6: the route's channel has no adapter wired in this binary; needs the vendor adapter and its provisioned credential"
	case ReadinessStale:
		return "the dispatcher has not evaluated routing recently; check the alert dispatcher process (flat alert_dispatcher_passes_total)"
	default:
		return "routing readiness has not been evaluated yet"
	}
}

// RouteConfig is the configuration-only projection of one current route.
type RouteConfig struct {
	ID            string // tie-break only (deterministic when effective_from is equal)
	Severity      Severity
	Step          int
	ChannelKind   ChannelKind
	Enabled       bool
	HasRecipient  bool
	DBHuman       bool // alerting_channel_kind_is_human_notification(channel_kind)
	EffectiveFrom time.Time
}

// SeverityReadiness is one severity's evaluation.
type SeverityReadiness struct {
	Severity           Severity
	Required           bool
	Ready              bool
	Reason             ReadinessReason
	RoutesConfigured   int // current routes at any step
	RoutesEnabled      int
	HumanRoutesEnabled int // enabled routes whose channel is human-notification (DB flag and wired sink)
}

// EvaluateReadiness is the pure evaluator. wired maps each wired channel kind
// to its HumanNotification() answer (presence = wired). Only the latest
// current row per (severity, step) counts, mirroring route resolution, so a
// newer disabled version makes the step not ready.
func EvaluateReadiness(routes []RouteConfig, wired map[ChannelKind]bool) []SeverityReadiness {
	type key struct {
		sev  Severity
		step int
	}
	latest := make(map[key]RouteConfig)
	for _, r := range routes {
		k := key{r.Severity, r.Step}
		if cur, ok := latest[k]; !ok || r.EffectiveFrom.After(cur.EffectiveFrom) ||
			(r.EffectiveFrom.Equal(cur.EffectiveFrom) && r.ID > cur.ID) {
			latest[k] = r
		}
	}
	out := make([]SeverityReadiness, 0, 3)
	for _, sev := range []Severity{SeverityP1, SeverityP2, SeverityP3} {
		res := SeverityReadiness{Severity: sev, Required: sev != SeverityP3, Reason: ReadinessNoRoute}
		var step0 *RouteConfig
		for k, r := range latest {
			if k.sev != sev {
				continue
			}
			res.RoutesConfigured++
			isHuman := r.DBHuman && wired[r.ChannelKind]
			if r.Enabled {
				res.RoutesEnabled++
				if isHuman && r.HasRecipient {
					res.HumanRoutesEnabled++
				}
			}
			if k.step == 0 {
				rc := r
				step0 = &rc
			}
		}
		if step0 != nil {
			human, isWired := wired[step0.ChannelKind]
			switch {
			case !step0.Enabled:
				res.Reason = ReadinessRouteDisabled
			case !step0.DBHuman || (isWired && !human):
				res.Reason = ReadinessNotHuman
			case !isWired:
				res.Reason = ReadinessNotWired
			case !step0.HasRecipient:
				res.Reason = ReadinessRouteDisabled
			default:
				res.Ready, res.Reason = true, ReadinessReady
			}
		}
		out = append(out, res)
	}
	return out
}

// ReadinessSnapshot is a cached evaluation.
type ReadinessSnapshot struct {
	Evaluated   bool
	EvaluatedAt time.Time
	Severities  []SeverityReadiness
}

// Effective returns the snapshot's per-severity view as of now, applying the
// staleness rule: a never-evaluated or stale snapshot reports every severity
// NOT ready with the corresponding reason.
func (s ReadinessSnapshot) Effective(now time.Time, staleAfter time.Duration) []SeverityReadiness {
	if !s.Evaluated {
		return notReadyAll(ReadinessNotEvaluated, nil)
	}
	if now.Sub(s.EvaluatedAt) > staleAfter {
		return notReadyAll(ReadinessStale, s.Severities)
	}
	return s.Severities
}

func notReadyAll(reason ReadinessReason, keep []SeverityReadiness) []SeverityReadiness {
	out := make([]SeverityReadiness, 0, 3)
	for _, sev := range []Severity{SeverityP1, SeverityP2, SeverityP3} {
		r := SeverityReadiness{Severity: sev, Required: sev != SeverityP3, Reason: reason}
		for _, k := range keep {
			if k.Severity == sev {
				r.RoutesConfigured, r.RoutesEnabled, r.HumanRoutesEnabled = k.RoutesConfigured, k.RoutesEnabled, k.HumanRoutesEnabled
			}
		}
		out = append(out, r)
	}
	return out
}

const readinessRepeatLogEvery = 10 * time.Minute

type readinessTracker struct {
	mu         sync.Mutex
	clock      Clock
	staleAfter time.Duration
	snap       ReadinessSnapshot
	prevReady  map[Severity]bool
	lastLogged time.Time
}

// Snapshot returns the effective (staleness-applied) readiness.
func (d *Dispatcher) ReadinessEffective() []SeverityReadiness {
	return d.readiness.effective()
}

// ReadinessEvaluatedAt returns when routing was last evaluated (zero if never).
func (d *Dispatcher) ReadinessEvaluatedAt() time.Time {
	d.readiness.mu.Lock()
	defer d.readiness.mu.Unlock()
	return d.readiness.snap.EvaluatedAt
}

func (t *readinessTracker) effective() []SeverityReadiness {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap.Effective(t.clock.Now(), t.staleAfter)
}

// EvaluateReadiness reads the current routes under the dispatcher identity,
// evaluates readiness over configuration only, updates the cache and logs a
// transition. A read failure leaves the cache untouched (so it goes stale).
func (d *Dispatcher) EvaluateReadiness(ctx context.Context) error {
	now := d.config.Clock.Now()
	routes, err := d.loadRouteConfigs(ctx, now)
	if err != nil {
		return err
	}
	wired := make(map[ChannelKind]bool, len(d.sinks))
	for k, s := range d.sinks {
		wired[k] = s.HumanNotification()
	}
	res := EvaluateReadiness(routes, wired)
	d.readiness.update(d.logger(), now, res)
	return nil
}

func (t *readinessTracker) update(logger interface {
	Error(msg string, args ...any)
}, now time.Time, res []SeverityReadiness) {
	t.mu.Lock()
	defer t.mu.Unlock()
	first := !t.snap.Evaluated
	if t.prevReady == nil {
		t.prevReady = map[Severity]bool{}
	}
	t.snap = ReadinessSnapshot{Evaluated: true, EvaluatedAt: now, Severities: res}
	repeat := now.Sub(t.lastLogged) >= readinessRepeatLogEvery
	for _, r := range res {
		if !r.Required {
			continue
		}
		prev, had := t.prevReady[r.Severity]
		t.prevReady[r.Severity] = r.Ready
		switch {
		case r.Ready && had && !prev:
			logger.Error("alert_routing_ready", "severity", string(r.Severity))
			t.lastLogged = now
		case !r.Ready && (first || !had || prev || repeat):
			logger.Error("alert_routing_not_ready", "severity", string(r.Severity),
				"reason", string(r.Reason), "missing_operator_input", MissingOperatorInput(r.Reason))
			t.lastLogged = now
		}
	}
}

// latestReadiness is the tracker the alert_routing_ready gauge observes: the
// most recently constructed dispatcher in this process (production has one).
var latestReadiness atomic.Pointer[readinessTracker]

func registerReadinessSource(t *readinessTracker) { latestReadiness.Store(t) }

// alertRoutingReady is the gauge alert_routing_ready{severity} (0/1). It is
// observed at scrape time through the staleness rule, so a stalled dispatcher
// reads 0 rather than a frozen 1.
var _, _ = meter.Int64ObservableGauge(
	"alert_routing_ready",
	metric.WithDescription("1 when a configured path would notify a person for the severity; configuration only, stale or never evaluated reads 0 (ADR 0102 section 18)."),
	metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		var eff []SeverityReadiness
		if t := latestReadiness.Load(); t != nil {
			eff = t.effective()
		} else {
			eff = notReadyAll(ReadinessNotEvaluated, nil)
		}
		for _, r := range eff {
			if !r.Required {
				continue
			}
			v := int64(0)
			if r.Ready {
				v = 1
			}
			o.Observe(v, metric.WithAttributes(severityAttr(r.Severity)))
		}
		return nil
	}),
)
