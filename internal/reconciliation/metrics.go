package reconciliation

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// Reconciliation health signals (pending-signals.yml ReconciliationStale and LedgerDriftAsMetric, A9).
//
//	reconciliation_runs_total{stream,result}          result: success | mismatch | failed | skipped
//	reconciliation_sweeps_total{result}               result: ok | listing_failed
//	reconciliation_last_success_unix_seconds{stream}  the OLDEST per-tenant last success of the stream
//	reconciliation_mismatches_open{stream}            mismatches reported by each tenant's latest completed run
//	reconciliation_last_sweep_unix_seconds            end of the last COMPLETE sweep
//	reconciliation_payment_unreconciled{reason}       (tenant, provider) payment_statement pairs whose latest outcome is NOT
//	                                                  "reconciled up to now": no_source | source_not_eligible | truncated | run_failed |
//	                                                  scope_unavailable (A11: a configured provider with no eligible statement
//	                                                  source, or a run stopped at the per-run bound; never reads as clean)
//
// Labels are bounded: stream is the closed set below (anything else is "other"); there is NO tenant label
// (per-tenant state is held in memory and only its min / sum leave the process). The aggregation is chosen
// so a single stale or failing tenant is visible: last_success is the minimum over tenants, so one tenant
// that stops succeeding drags the series back in time even while every other tenant is healthy; a tenant
// that has never succeeded in this process reads 0.
//
// The state is per process and starts empty: the first sweep runs immediately on start (RunSchedulerLoop),
// so a series is present within one sweep of boot. A restart resets it; a multi-instance deployment has one
// series per instance (the lock-skip result keeps an instance that always loses the advisory lock from
// reading as stale: a skip never creates state).
const (
	resultSuccess  = "success"
	resultMismatch = "mismatch"
	resultFailed   = "failed"
	resultSkipped  = "skipped"
	// resultTruncated: a payment_statement run that completed but stopped at the per-run bound (R3-OBS-BOUND-1).
	// It is not a success for freshness (reconciliation_last_success_unix_seconds does not advance), so a
	// backlog that never catches up reads as stale.
	resultTruncated = "truncated"
)

var boundedStreams = map[string]bool{
	string(StreamLedgerVsProjection):   true,
	string(StreamSportsbookSettlement): true,
	string(StreamCasinoConsistency):    true,
	string(StreamCasinoStatement):      true,
	string(StreamPaymentStatement):     true,
}

func boundedStream(s string) string {
	if boundedStreams[s] {
		return s
	}
	return "other"
}

type runKey struct {
	tenant uuid.UUID
	stream string
	source string // payment_statement source (provider) so two sources of one tenant do not overwrite each other
}

type runState struct {
	lastSuccessUnix int64
	mismatches      int64
	seenGen         uint64
	// unreconciled is the NotReconciled reason of the latest non-skipped outcome ("" = reconciled).
	unreconciled string
}

type runTracker struct {
	now func() time.Time

	mu            sync.Mutex
	gen           uint64
	states        map[runKey]*runState
	lastSweepUnix int64

	runs   metric.Int64Counter
	sweeps metric.Int64Counter
}

func newRunTracker(now func() time.Time) *runTracker {
	return &runTracker{now: now, states: map[runKey]*runState{}}
}

// defaultTracker is fed by the production sweep (sweepTenants / observeNonActiveTenants / RunSweep).
var defaultTracker = newRunTracker(time.Now)

func init() {
	// Registration against the global meter provider, exactly like every other package-level instrument:
	// the global delegate forwards to the provider cmd/platform-api installs later.
	_ = defaultTracker.register(otel.Meter("github.com/Diansalas/igaming-platform/internal/reconciliation"))
}

func (t *runTracker) register(meter metric.Meter) error {
	var err error
	if t.runs, err = meter.Int64Counter("reconciliation_runs_total",
		metric.WithDescription("Reconciliation stream runs per tenant by result: success, mismatch (run completed and found mismatches), failed, skipped (advisory-lock contention).")); err != nil {
		return err
	}
	if t.sweeps, err = meter.Int64Counter("reconciliation_sweeps_total",
		metric.WithDescription("Reconciliation sweeps by result: ok (tenant listing succeeded) or listing_failed (the tenant listing failed, so nothing was reconciled).")); err != nil {
		return err
	}
	if _, err = meter.Int64ObservableGauge("reconciliation_last_success_unix_seconds",
		metric.WithDescription("Unix time of the OLDEST per-tenant last successful run of the stream (min over tenants; 0 = some tenant has never succeeded in this process)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for stream, v := range t.lastSuccessByStream() {
				o.Observe(v, metric.WithAttributes(attribute.String("stream", stream)))
			}
			return nil
		})); err != nil {
		return err
	}
	if _, err = meter.Int64ObservableGauge("reconciliation_mismatches_open",
		metric.WithDescription("Mismatches reported by each tenant's most recent completed run of the stream, summed. A failed run does not clear it; a later clean run does."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for stream, v := range t.mismatchesByStream() {
				o.Observe(v, metric.WithAttributes(attribute.String("stream", stream)))
			}
			return nil
		})); err != nil {
		return err
	}
	if _, err = meter.Int64ObservableGauge("reconciliation_payment_unreconciled",
		metric.WithDescription("payment_statement (tenant, provider) pairs whose latest outcome is not 'reconciled up to now', by reason: no_source, source_not_eligible, truncated, run_failed, scope_unavailable."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for reason, v := range t.PaymentUnreconciled() {
				o.Observe(v, metric.WithAttributes(attribute.String("reason", reason)))
			}
			return nil
		})); err != nil {
		return err
	}
	_, err = meter.Int64ObservableGauge("reconciliation_last_sweep_unix_seconds",
		metric.WithDescription("Unix time the last COMPLETE reconciliation sweep finished; stops advancing when the scheduler stalls or tenant listing fails."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			t.mu.Lock()
			v := t.lastSweepUnix
			t.mu.Unlock()
			if v != 0 {
				o.Observe(v)
			}
			return nil
		}))
	return err
}

func (t *runTracker) beginSweep() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gen++
	return t.gen
}

func (t *runTracker) currentGen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// endSweep closes sweep gen. complete is true only when every tenant listing succeeded: then entries not
// seen in this sweep (a tenant that was removed or whose stream no longer applies) are pruned and the sweep
// heartbeat advances. An incomplete sweep prunes nothing and does not advance the heartbeat.
func (t *runTracker) endSweep(gen uint64, complete bool) {
	t.mu.Lock()
	if complete {
		for k, s := range t.states {
			if s.seenGen < gen {
				delete(t.states, k)
			}
		}
		t.lastSweepUnix = t.now().Unix()
	}
	sweeps := t.sweeps
	t.mu.Unlock()
	if sweeps != nil {
		result := "ok"
		if !complete {
			result = "listing_failed"
		}
		sweeps.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

// record folds one stream outcome of one tenant into the state and counts it.
func (t *runTracker) record(gen uint64, tenant uuid.UUID, stream, source string, out StreamOutcome) {
	result := resultSuccess
	switch {
	case out.Err != nil:
		result = resultFailed
	case out.Skipped:
		result = resultSkipped
	case out.Truncated:
		result = resultTruncated
	case out.Run.Status == StatusMismatchesFound:
		result = resultMismatch
	}
	stream = boundedStream(stream)
	key := runKey{tenant: tenant, stream: stream, source: source}

	t.mu.Lock()
	st := t.states[key]
	switch result {
	case resultSkipped:
		if st != nil {
			st.seenGen = gen
		}
	default:
		if st == nil {
			st = &runState{}
			t.states[key] = st
		}
		st.seenGen = gen
		st.unreconciled = out.NotReconciled
		if stream == string(StreamPaymentStatement) && result == resultFailed && st.unreconciled == "" {
			st.unreconciled = "run_failed" // a failed fetch/ingest/match is not "reconciled up to now" either
		}
		if result == resultTruncated {
			// A truncated run matched the window it covered (its mismatches count) but is not "reconciled up to now".
			st.mismatches = int64(out.Mismatches)
			if out.Run.Status == StatusMismatchesFound && out.Mismatches == 0 {
				st.mismatches = 1
			}
		} else if result != resultFailed {
			st.lastSuccessUnix = t.now().Unix()
			st.mismatches = int64(out.Mismatches)
			if result == resultMismatch && out.Mismatches == 0 {
				st.mismatches = 1 // a run that reports mismatches never reads as zero
			}
		}
	}
	runs := t.runs
	t.mu.Unlock()

	if runs != nil {
		runs.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("stream", stream), attribute.String("result", result)))
	}
}

// recordOutcomes folds the outcomes of one sweep into the tracker. An observation-only (suspended or closed)
// tenant ran only ledger_vs_projection and payment_statement, so only those are recorded for it; recording the
// zero-valued other streams would count runs that never happened.
func (t *runTracker) recordOutcomes(gen uint64, outcomes []SweepOutcome, paySources []statement.PaymentStatementSource) {
	for _, o := range outcomes {
		t.record(gen, o.TenantID, string(StreamLedgerVsProjection), "",
			StreamOutcome{Skipped: o.Skipped, Run: o.Run, Err: o.Err, Mismatches: o.Mismatches})
		if !o.ObservationOnly {
			t.record(gen, o.TenantID, string(StreamSportsbookSettlement), "", o.Sportsbook)
			t.record(gen, o.TenantID, string(StreamCasinoConsistency), "", o.Casino)
			t.record(gen, o.TenantID, string(StreamCasinoStatement), "", o.CasinoStatement)
		}
		for i, ps := range o.PaymentStatement {
			// The scoped sweep stamps every outcome with its provider; the legacy sweep's outcomes are
			// positional (one per registered source, in order).
			src := ps.ProviderID
			if src == "" && i < len(paySources) {
				src = sourceKey(paySources[i])
			}
			t.record(gen, o.TenantID, string(StreamPaymentStatement), src, ps)
		}
	}
}

func (t *runTracker) lastSuccessByStream() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int64{}
	for k, s := range t.states {
		if cur, ok := out[k.stream]; !ok || s.lastSuccessUnix < cur {
			out[k.stream] = s.lastSuccessUnix
		}
	}
	return out
}

// PaymentUnreconciled returns, per NotReconciled reason, how many (tenant, provider) payment_statement pairs
// are currently not reconciled (latest outcome per pair). It is the readiness signal for "a configured payment
// provider has no statement source / is only partially covered": non-empty means NOT reconciled.
func (t *runTracker) PaymentUnreconciled() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int64{}
	for k, s := range t.states {
		if k.stream == string(StreamPaymentStatement) && s.unreconciled != "" {
			out[s.unreconciled]++
		}
	}
	return out
}

// PaymentUnreconciled is the process-wide view of the run tracker's unreconciled payment pairs (see
// runTracker.PaymentUnreconciled).
func PaymentUnreconciled() map[string]int64 { return defaultTracker.PaymentUnreconciled() }

func (t *runTracker) mismatchesByStream() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int64{}
	for k, s := range t.states {
		out[k.stream] += s.mismatches
	}
	return out
}
