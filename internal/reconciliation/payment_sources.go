package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// A11: PAY-K3-STATEMENT-SOURCE-WIRING-1 (per-(tenant, provider) source resolution) and R3-OBS-BOUND-1
// (bounded, resumable observation work) for the payment_statement stream.
//
// The legacy sweep (RunSweep with a flat source list) runs EVERY registered source for EVERY tenant and
// says nothing about a provider that has no source. The scoped sweep (RunSweepScoped, the production loop
// since A11) instead asks, per tenant, "which payment providers does this tenant have?" - its
// provider_capabilities rows plus any provider it has payment attempts with (a provider removed from the
// configuration can still have money in flight) - and reconciles each with the source the resolver returns:
//
//	configured provider, eligible source   -> the payment_statement stream (fetch / ingest / match), bounded
//	                                         by RunLineBound and continuing from the persisted cursor;
//	configured provider, NO eligible source -> NOT reconciled: an audited failed run, a durable
//	                                         reconciliation.run_failed alert (phase no_statement_source) and
//	                                         the reconciliation_payment_unreconciled{reason} gauge. NEVER a
//	                                         clean result and never silently skipped;
//	registered source, provider not configured for the tenant -> not run for that tenant (no credential
//	                                         resolution, no fetch, no noise for a tenant that never used it).
//
// A source is "eligible" when the resolver returns it, it is non-nil, it speaks for exactly that provider id,
// and it is not Synthetic unless the config allows Synthetic (the wiring allows it only where mock components
// are allowed; the production startup guard refuses them otherwise - cmd/platform-api).

// NotReconciled reasons (the closed vocabulary of StreamOutcome.NotReconciled and of the gauge's reason label).
const (
	// NotReconciledNoSource: a configured provider has no registered statement source.
	NotReconciledNoSource = "no_source"
	// NotReconciledIneligible: a source is registered but not eligible (provider id mismatch, or Synthetic where
	// Synthetic is not allowed).
	NotReconciledIneligible = "source_not_eligible"
	// NotReconciledTruncated: the run stopped at the per-run bound; the next run continues from the cursor.
	NotReconciledTruncated = "truncated"
	// NotReconciledScope: the tenant's configured providers could not be listed.
	NotReconciledScope = "scope_unavailable"
)

// PaymentSourceResolver maps a provider id to the statement source that speaks for it. Implementations are
// read-only after construction (the sweep calls them concurrently with nothing else and never mutates them).
type PaymentSourceResolver interface {
	SourceFor(providerID string) (statement.PaymentStatementSource, bool)
}

// PaymentSweepConfig drives the scoped payment_statement stream.
type PaymentSweepConfig struct {
	// Resolver resolves a configured provider's source. A nil Resolver means no provider has a source: every
	// configured provider is reported as not reconciled (fail closed).
	Resolver PaymentSourceResolver
	// RunLineBound is the per-run, per-(tenant, provider) line bound (R3-OBS-BOUND-1); 0 = unbounded. When a
	// statement holds more lines the run is TRUNCATED: it matches only the window the source vouches for,
	// records window_truncated, raises a durable alert and reports reconciliation_payment_unreconciled
	// {reason="truncated"}; the next run continues from the persisted cursor. A truncated run is never a
	// clean claim for the requested window.
	RunLineBound int
	// AllowSynthetic admits a Synthetic (MOCK) source. The wiring sets it only where mock components are
	// allowed (never in production).
	AllowSynthetic bool
}

// paymentPlan is how the sweep runs the payment_statement stream: legacy (a flat source list, every source for
// every tenant, no cursor) or scoped (cfg).
type paymentPlan struct {
	legacy []statement.PaymentStatementSource
	cfg    *PaymentSweepConfig
}

func legacyPaymentPlan(srcs []statement.PaymentStatementSource) paymentPlan {
	return paymentPlan{legacy: srcs}
}

func scopedPaymentPlan(cfg PaymentSweepConfig) paymentPlan {
	c := cfg
	return paymentPlan{cfg: &c}
}

// paymentTarget is one (tenant's) configured provider and the source to use for it; src is nil when none is
// eligible, with the reason.
type paymentTarget struct {
	providerID string
	src        statement.PaymentStatementSource
	reason     string
}

func isNilPaymentSource(src statement.PaymentStatementSource) bool {
	if src == nil {
		return true
	}
	rv := reflect.ValueOf(src)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// resolvePaymentTargets lists the tenant's configured providers and resolves a source for each. Pure with
// respect to the sweep: it reads provider_capabilities / payment_attempts in the tenant's own scope and
// calls the resolver; it performs no provider I/O.
func resolvePaymentTargets(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, cfg *PaymentSweepConfig) ([]paymentTarget, error) {
	providers, err := listConfiguredPaymentProviders(ctx, pool, tenantID)
	if err != nil {
		return nil, err
	}
	targets := make([]paymentTarget, 0, len(providers))
	for _, p := range providers {
		tg := paymentTarget{providerID: p}
		var src statement.PaymentStatementSource
		ok := false
		if cfg.Resolver != nil {
			src, ok = cfg.Resolver.SourceFor(p)
		}
		switch {
		case !ok || isNilPaymentSource(src):
			tg.reason = NotReconciledNoSource
		case src.ProviderID() != p:
			tg.reason = NotReconciledIneligible // a source that speaks for another provider can never vouch for this one
		case isSyntheticSource(src) && !cfg.AllowSynthetic:
			tg.reason = NotReconciledIneligible // a MOCK statement never vouches for a provider where mocks are not allowed
		default:
			tg.src = src
		}
		targets = append(targets, tg)
	}
	return targets, nil
}

// listConfiguredPaymentProviders is the tenant's payment provider set, sorted: the providers it has capability
// rows for (any status: 'disabled' is a routing switch only, money may still be in flight) plus the providers
// its payment attempts name. A variable ONLY so a test can force the listing to fail.
var listConfiguredPaymentProviders = func(ctx context.Context, pool *db.Pool, tenantID uuid.UUID) ([]string, error) {
	var out []string
	err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT provider_id FROM provider_capabilities WHERE tenant_id = $1
			 UNION
			 SELECT provider_id FROM payment_attempts WHERE tenant_id = $1 AND provider_id IS NOT NULL
			 ORDER BY 1`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reconciliation: list configured payment providers: %w", err)
	}
	return out, nil
}

// reconcilePaymentStreamsForTenant runs the payment_statement stream for one tenant under plan and returns one
// outcome per provider handled (legacy: per registered source; scoped: per configured provider). obsStatus is
// "" for an active tenant, else the non-active tenant's status (evidence-only observation, PRH-2 R3). It calls
// only ReconcilePaymentStatementForTenant and the not-reconciled recorders (pinned by
// TestR3_Static_PaymentStreamsHelperCallsOnlyTheStreamAndRecorders): read plus findings/alerts only.
func reconcilePaymentStreamsForTenant(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, periodStart, periodEnd time.Time, plan paymentPlan, obsStatus string) []StreamOutcome {
	var outs []StreamOutcome
	if plan.cfg == nil {
		for _, src := range plan.legacy {
			outs = append(outs, ReconcilePaymentStatementForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd, src,
				PaymentStatementOptions{ObservationOnlyStatus: obsStatus}))
		}
		return outs
	}
	targets, err := resolvePaymentTargets(ctx, pool, tenantID, plan.cfg)
	if err != nil {
		return append(outs, reconcilePaymentNotReconciled(ctx, pool, logger, tenantID, "", NotReconciledScope, obsStatus, err))
	}
	for _, tg := range targets {
		if tg.src == nil {
			outs = append(outs, reconcilePaymentNotReconciled(ctx, pool, logger, tenantID, tg.providerID, tg.reason, obsStatus,
				fmt.Errorf("%w: provider %q (%s)", ErrPaymentStatementSourceMissing, tg.providerID, tg.reason)))
			continue
		}
		outs = append(outs, ReconcilePaymentStatementForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd, tg.src,
			PaymentStatementOptions{ObservationOnlyStatus: obsStatus, RunLineBound: plan.cfg.RunLineBound, Continuation: true}))
	}
	return outs
}

// readPaymentCursor returns the (tenant, provider) continuation cursor: the maximum validated coverage_end of
// the payment_statement runs that COMMITTED for the pair (migration 0160), or the zero time when there is none.
// Like casino_statement's anchor (A3 SR3B-3) it is read from the run records the stream itself writes under its
// advisory lock, never from audit rows; the maximum is used, so a forged LOWER value cannot lower it (at worst
// a window is re-matched, which is idempotent), and a row dated in the future or claiming coverage beyond
// now + PaymentCoverageMaxClockSkew is ignored, so a forged HIGH value cannot move the cursor into the future
// (residual, documented: a forged value inside that skew can skip at most that many minutes).
func readPaymentCursor(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, providerID string) (time.Time, error) {
	var cur time.Time
	err := pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cur, err = queryPaymentCursor(ctx, tx, tenantID, providerID)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return cur, nil
}

func queryPaymentCursor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string) (time.Time, error) {
	var prev *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT max(coverage_end) FROM reconciliation_runs
		  WHERE tenant_id = $1 AND stream = $2 AND provider_id = $3 AND coverage_end IS NOT NULL
		    AND run_at <= now() AND coverage_end <= now() + $4::interval`,
		tenantID, string(StreamPaymentStatement), providerID,
		fmt.Sprintf("%d seconds", int(PaymentCoverageMaxClockSkew.Seconds()))).Scan(&prev); err != nil {
		return time.Time{}, fmt.Errorf("reconciliation: read payment statement cursor: %w", err)
	}
	if prev == nil {
		return time.Time{}, nil
	}
	return prev.UTC().Truncate(time.Microsecond), nil
}

// errPaymentCursorStale means that, under the stream's advisory lock, the cursor is no longer the one this
// run's statement was fetched from: another instance committed a run for the same (tenant, provider) between
// this run's cursor read and its match. Matching now would cover a window twice, so the run is SKIPPED (not a
// failure; the next sweep continues from the new cursor). The import this run already stored is plain,
// content-addressed evidence and stays. Residual: the check reads the cursor in the match transaction's
// snapshot, so a run that commits inside the sub-millisecond gap between that snapshot and the advisory lock
// call can still be missed; the consequence is one re-matched window (idempotent), never a skipped one.
var errPaymentCursorStale = errors.New("reconciliation: payment statement cursor moved since fetch")

var discPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// discPart renders a provider id as a charset-safe alert discriminator part (a configured provider id comes
// from a database row, not from a validated source).
func discPart(providerID string) string {
	if discPartRE.MatchString(providerID) {
		return providerID
	}
	return "invalid_provider_id"
}

// reconcilePaymentNotReconciled records that a (tenant, provider) pair was NOT reconciled this sweep: an
// error-level log, an audited reconciliation.sweep_run_failed (phase "source" / "scope"), the durable
// reconciliation.run_failed alert, and an outcome carrying Err and NotReconciled so the tracker counts a
// FAILED run (never a success) and the unreconciled gauge shows it. providerID is empty for a scope failure.
func reconcilePaymentNotReconciled(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, providerID, reason, obsStatus string, cause error) StreamOutcome {
	stream := string(StreamPaymentStatement)
	phase := "source"
	if reason == NotReconciledScope {
		phase = "scope"
	}
	out := StreamOutcome{ProviderID: providerID, NotReconciled: reason,
		Err: fmt.Errorf("reconciliation: payment_statement %s phase: %w", phase, cause)}
	shown := providerID
	if shown == "" {
		shown = "<none>"
	}
	if logger != nil {
		logger.Error("reconciliation sweep: payment provider NOT reconciled (P1)", "tenant_id", tenantID, "stream", stream,
			"phase", phase, "reason", reason, "provider_id", shown, "error", out.Err)
	}
	if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
			TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
			Metadata: observationMetadata(obsStatus, map[string]any{"stream": stream, "phase": phase, "reason": reason,
				"error": out.Err.Error(), "statement_source": "<none>", "provider_id": shown, "severity": "P1"}),
		})
	}); auditErr != nil && logger != nil {
		logger.Error("reconciliation sweep: failed to audit not-reconciled provider", "tenant_id", tenantID, "stream", stream, "error", auditErr)
	}
	alertPhase := "no_statement_source"
	switch reason {
	case NotReconciledIneligible:
		alertPhase = "source_not_eligible"
	case NotReconciledScope:
		alertPhase = "scope"
	}
	raiseRunNotReconciled(ctx, pool, tenantID, stream, providerID, alertPhase)
	return out
}
