package aml

// lic10 (migration 0181, audit G1): the AUTOMATIC SCREENING WORKER. LABELS: IMPLEMENTED against the MOCK provider only; a real
// screening vendor is PROVIDER DEPENDENT; transaction MONITORING is NOT IMPLEMENTED (nothing here is, or may be described as,
// monitoring: R-AML-MONITORING stays failing).
//
// What it does, per active tenant and pass:
//
//  1. scheduled enqueue: periodic re-screen and backfill (aml_screening_enqueue_due; one request per person per re-screen
//     interval of the screening policy);
//  2. claim (aml_screening_request_claim: SKIP LOCKED, a lease; an expired lease is reclaimed under a NEW token);
//  3. per claimed request, in three phases that never nest: (A) a short tenant transaction reads the screening policy (the lists)
//     and the vendor subject reference; (B) the provider call with NO transaction held (txscope.Held is false), bounded, through
//     the SAME fail-closed normalisation as the officer orchestrator (Orchestrator.callProvider: an error, a timeout, a panic,
//     an unknown outcome and an inconsistent answer are all `error`; nothing but a COMPLETED, self-consistent clear is a clear);
//     (C) a tenant transaction that applies the outcome with a claim-token CAS (aml_screening_request_finish) and raises the
//     attention alert in the SAME transaction.
//
// Outcome handling: clear -> done. A potential / confirmed match -> needs_officer (the system NEVER clears or acts on a match:
// an officer records the result under the request's idempotency key, which places the hold with the officer quorum and proof).
// error -> a bounded retry with a database-clock backoff, then failed_terminal plus an alert. The retry budget and the backoff
// are ENGINEERING PLACEHOLDERS (never a compliance deadline; a deadline a jurisdiction imposes is configuration).
//
// Nothing here logs or alerts a person id, a list, a match, a vendor reference or any vendor text: only the request id and
// closed codes. The worker cannot place a hold and cannot read a screening result, a hold or a case.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// ScreeningWorkerConfig holds the worker's TECHNICAL constants (engineering placeholders, not business or compliance values).
type ScreeningWorkerConfig struct {
	EnqueueBatch int           // scheduled requests enqueued per tenant per pass (1..1000)
	ClaimBatch   int           // requests claimed per tenant per pass (1..200)
	TenantCap    int           // tenants served per pass
	Lease        time.Duration // claim lease; must exceed one request's phases A+B+C
	CallTimeout  time.Duration // bound on one provider call
	PhaseTimeout time.Duration // bound on each database phase
	MaxAttempts  int           // attempts (claims) before a request ends failed_terminal; PLACEHOLDER
	BackoffBase  time.Duration // PLACEHOLDER
	BackoffCap   time.Duration // PLACEHOLDER
}

// DefaultScreeningWorkerConfig returns the engineering recommendations.
func DefaultScreeningWorkerConfig() ScreeningWorkerConfig {
	return ScreeningWorkerConfig{EnqueueBatch: 100, ClaimBatch: 4, TenantCap: 500, Lease: 2 * time.Minute, CallTimeout: 10 * time.Second,
		PhaseTimeout: 5 * time.Second, MaxAttempts: 5, BackoffBase: 30 * time.Second, BackoffCap: 30 * time.Minute}
}

// ScreeningWorker drains aml_screening_requests through a ScreeningProvider.
type ScreeningWorker struct {
	Pool     *db.Pool
	Provider ScreeningProvider
	Keys     SubjectKeyResolver
	Config   ScreeningWorkerConfig
	Logger   *slog.Logger
	// Now is the clock for provider timestamps (nil means time.Now). Database-clock decisions are the database's.
	Now func() time.Time
	// TenantScope is a TEST SEAM and nil in production (a static test pins that nothing in cmd/ sets it): when set, only the
	// returned tenants are served, so integration tests that share one database run in parallel.
	TenantScope func() []uuid.UUID
}

// NewScreeningWorker builds a worker with the default configuration.
func NewScreeningWorker(pool *db.Pool, provider ScreeningProvider, keys SubjectKeyResolver) *ScreeningWorker {
	return &ScreeningWorker{Pool: pool, Provider: provider, Keys: keys, Config: DefaultScreeningWorkerConfig()}
}

// ValidateForLoop reports a worker that must not be started: a missing dependency, a malformed provider id, or a lease that
// cannot cover one request.
func (w *ScreeningWorker) ValidateForLoop() error {
	switch {
	case w == nil:
		return errors.New("aml: screening worker is nil")
	case w.Pool == nil:
		return errors.New("aml: screening worker has no pool")
	case w.Provider == nil:
		return errors.New("aml: screening worker has no screening provider")
	case w.Keys == nil:
		return errors.New("aml: screening worker has no subject key resolver")
	case !providerIDRE.MatchString(w.Provider.ID()):
		return errors.New("aml: the screening provider id is malformed")
	}
	c := w.Config
	switch {
	case c.EnqueueBatch < 1 || c.EnqueueBatch > 1000 || c.ClaimBatch < 1 || c.ClaimBatch > 200 || c.TenantCap < 1:
		return errors.New("aml: screening worker batch sizes out of range")
	case c.CallTimeout <= 0 || c.PhaseTimeout <= 0 || c.MaxAttempts < 1 || c.BackoffBase <= 0 || c.BackoffCap < c.BackoffBase:
		return errors.New("aml: screening worker timeouts, attempts and backoff must be positive")
	case c.Lease < time.Duration(c.ClaimBatch)*(3*c.PhaseTimeout+c.CallTimeout)+5*time.Second:
		// L7: every claimed item must be finishable (phase A, the call, phase C with its detached 2x bound) before the LAST item's
		// lease can lapse, otherwise later items of a batch are burned (their attempts counted) before they are even started.
		return errors.New("aml: screening worker lease is shorter than the whole claimed batch (batch x (phase A + call + phase C))")
	case c.Lease < 5*time.Second || c.Lease > time.Hour:
		return errors.New("aml: screening worker lease out of range")
	}
	return nil
}

// PassStats is what one pass did (counts only; no identifier).
type PassStats struct {
	Tenants, Enqueued, Claimed, Done, NeedsOfficer, Retried, Terminal, Lost, Errors, Trapped int
	// ParkedScreening (lic23 N-9): persons whose payout is PARKED right now only because a required screening is outstanding.
	ParkedScreening int
}

// finishPlan is the pure decision of how to end one claimed request.
type finishPlan struct {
	Result     string // clear | potential_match | confirmed | retry | terminal
	MatchLists []ListType
	ErrorCode  ScreeningErrorCode
	Backoff    time.Duration
	Reason     string // alert reason, "" when none
}

// planFinish maps a NORMALISED provider outcome (callProvider's output) to the request's next state. Pure. Only a clear outcome
// yields Result clear; a match is never cleared; any other value (including an unknown one) is a failure and retried or ended.
func planFinish(outcome ScreeningOutcome, matches []ListType, code ScreeningErrorCode, attempts int, cfg ScreeningWorkerConfig) finishPlan {
	switch outcome {
	case OutcomeClear:
		return finishPlan{Result: "clear"}
	case OutcomePotentialMatch, OutcomeConfirmed:
		return finishPlan{Result: string(outcome), MatchLists: matches, Reason: "match_review"}
	}
	if !code.valid() {
		code = ErrCodeUnknown
	}
	if attempts >= cfg.MaxAttempts {
		return finishPlan{Result: "terminal", ErrorCode: code, Reason: "failed_terminal"}
	}
	return finishPlan{Result: "retry", ErrorCode: code, Backoff: backoffFor(attempts, cfg)}
}

// backoffFor is BackoffBase * 2^(attempts-1), capped at BackoffCap. Pure.
func backoffFor(attempts int, cfg ScreeningWorkerConfig) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	f := math.Pow(2, float64(attempts-1))
	d := time.Duration(f * float64(cfg.BackoffBase))
	if d <= 0 || d > cfg.BackoffCap || f > 1e9 {
		return cfg.BackoffCap
	}
	return d
}

type claimedRequest struct {
	ID, PersonID, JurisdictionID, ClaimToken uuid.UUID
	Trigger, IdempotencyKey                  string
	Attempts                                 int
}

// RunPass runs one pass over the active tenants. It never panics out and never returns an identifier.
func (w *ScreeningWorker) RunPass(ctx context.Context) PassStats {
	var st PassStats
	w.heartbeat(ctx)
	tenants, err := w.activeTenants(ctx)
	if err != nil {
		st.Errors++
		w.logErr("list tenants failed")
		return st
	}
	for _, tid := range tenants {
		if ctx.Err() != nil {
			return st
		}
		st.Tenants++
		w.passTenant(ctx, tid, &st)
	}
	return st
}

// heartbeat reports this worker's provider to the database (screening_worker_heartbeat). A withdrawal gate can be configured only
// while a worker has reported in recently, so the gate cannot be enabled where no worker runs (production starts none).
func (w *ScreeningWorker) heartbeat(ctx context.Context) {
	synthetic := w.Provider.ID() == MockProviderID // informational: the only provider in the tree is the MOCK
	pctx, cancel := w.phase(ctx)
	defer cancel()
	if err := w.Pool.WithoutTenant(pctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT screening_worker_heartbeat($1, $2)`, w.Provider.ID(), synthetic)
		return e
	}); err != nil {
		w.logErr("heartbeat failed")
	}
}

// countParked adds the tenant's screening-parked payout persons to the pass summary (a read; failures are counted, never fatal).
func (w *ScreeningWorker) countParked(ctx context.Context, tenantID uuid.UUID, st *PassStats) {
	pctx, cancel := w.phase(ctx)
	defer cancel()
	if err := w.Pool.WithTenant(pctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, _, err := ScreeningParkedSummary(ctx, tx, tenantID)
		st.ParkedScreening += n
		return err
	}); err != nil {
		st.Errors++
		w.logErr("parked summary failed")
	}
}

// alertTrapped raises aml.screening_gate_trapped when persons have been held at the withdrawal gate longer than the policy's alert
// threshold (H-1 f). The attribute is a COUNT only. A worker that is down cannot raise it: the residual is recorded in lic18.
func (w *ScreeningWorker) alertTrapped(ctx context.Context, tenantID uuid.UUID, st *PassStats) {
	var n int
	pctx, cancel := w.phase(ctx)
	err := w.Pool.WithTenant(pctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_overdue($1, 5000)`, tenantID).Scan(&n)
	})
	cancel()
	if err != nil {
		st.Errors++
		w.logErr("overdue check failed")
		return
	}
	if n == 0 {
		return
	}
	st.Trapped += n
	cctx, cancel2 := context.WithTimeout(context.WithoutCancel(ctx), 2*w.Config.PhaseTimeout)
	defer cancel2()
	pending, aerr := alerting.InTx(cctx, alerting.NewTenantRunner(w.Pool, tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
			Kind: alerting.KindAMLScreeningGateTrapped, SubjectTenantID: tenantID, Discriminator: "tenant:" + tenantID.String(),
			Attributes: map[string]alerting.AttrValue{"persons": n},
		})
	})
	defer pending.Flush(cctx)
	if aerr != nil {
		st.Errors++
		w.logErr("trapped alert failed")
	}
}

func (w *ScreeningWorker) activeTenants(ctx context.Context) ([]uuid.UUID, error) {
	if w.TenantScope != nil {
		return w.TenantScope(), nil
	}
	var out []uuid.UUID
	err := w.Pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants WHERE status = 'active' ORDER BY id LIMIT $1`, w.Config.TenantCap)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

func (w *ScreeningWorker) phase(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, w.Config.PhaseTimeout)
}

func (w *ScreeningWorker) passTenant(ctx context.Context, tenantID uuid.UUID, st *PassStats) {
	pctx, cancel := w.phase(ctx)
	var enq int
	err := w.Pool.WithTenant(pctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1, $2)`, tenantID, w.Config.EnqueueBatch).Scan(&enq)
	})
	cancel()
	if err != nil {
		st.Errors++
		w.logErr("scheduled enqueue failed")
	}
	st.Enqueued += enq

	var claimed []claimedRequest
	pctx, cancel = w.phase(ctx)
	err = w.Pool.WithTenant(pctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o_id, o_person_id, o_jurisdiction_id, o_trigger, o_idempotency_key, o_attempts, o_claim_token
			FROM aml_screening_request_claim($1, $2, $3)`, tenantID, w.Config.ClaimBatch, int(w.Config.Lease/time.Second))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c claimedRequest
			if err := rows.Scan(&c.ID, &c.PersonID, &c.JurisdictionID, &c.Trigger, &c.IdempotencyKey, &c.Attempts, &c.ClaimToken); err != nil {
				return err
			}
			claimed = append(claimed, c)
		}
		return rows.Err()
	})
	cancel()
	if err != nil {
		st.Errors++
		w.logErr("claim failed")
		return
	}
	for _, c := range claimed {
		if ctx.Err() != nil {
			return
		}
		st.Claimed++
		w.processRequest(ctx, tenantID, c, st)
	}
	w.countParked(ctx, tenantID, st)
	w.alertTrapped(ctx, tenantID, st)
}

// processRequest runs phases A, B and C for one claimed request.
func (w *ScreeningWorker) processRequest(ctx context.Context, tenantID uuid.UUID, c claimedRequest, st *PassStats) {
	now := w.now
	// Retry budget exhausted by reclaims alone (a crashed worker never reached phase C): end it without another provider call.
	if c.Attempts > w.Config.MaxAttempts {
		w.finish(ctx, tenantID, c, finishPlan{Result: "terminal", ErrorCode: ErrCodeUnknown, Reason: "failed_terminal"}, nil, st)
		return
	}
	// Phase A: the lists (the screening policy's) and the vendor subject reference. A short tenant transaction.
	var lists []ListType
	var vendorRef string
	pctx, cancel := w.phase(ctx)
	err := w.Pool.WithTenant(pctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ls []string
		var pid uuid.UUID
		var a, b int64
		var from time.Time
		err := tx.QueryRow(ctx, `SELECT policy_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from
			FROM aml_screening_policy_effective($1, $2)`, tenantID, c.JurisdictionID).Scan(&pid, &ls, &a, &b, &from)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no policy: lists stay empty, ended below
		}
		if err != nil {
			return err
		}
		lists = toLists(ls)
		// The KYC vendor's opaque subject reference (never a platform id). Attributes are BLOCKED on the Q6 privacy review.
		qerr := tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications
			WHERE tenant_id = $1 AND person_id = $2 AND provider_reference IS NOT NULL ORDER BY created_at DESC, id DESC LIMIT 1`,
			tenantID, c.PersonID).Scan(&vendorRef)
		if qerr != nil && !errors.Is(qerr, pgx.ErrNoRows) {
			return qerr
		}
		return nil
	})
	cancel()
	if err != nil {
		// The claim stays; the lease expires and the request is reclaimed (attempts already counted). Fail closed: nothing is cleared.
		st.Errors++
		w.logErr("phase A failed")
		return
	}
	if len(lists) == 0 {
		// The screening policy the trigger policy requires is gone or not effective: a configuration fault, never a clear.
		w.finish(ctx, tenantID, c, finishPlan{Result: "terminal", ErrorCode: ErrCodeUnknown, Reason: "failed_terminal"}, nil, st)
		return
	}
	key, kerr := w.Keys.Key(ctx, tenantID)
	var subj string
	if kerr == nil {
		subj, kerr = SubjectRef(key, c.PersonID)
	}
	if kerr != nil {
		w.finish(ctx, tenantID, c, planFinish(OutcomeError, nil, ErrCodeUnavailable, c.Attempts, w.Config), nil, st)
		return
	}
	req := ScreeningRequest{IdempotencyKey: c.IdempotencyKey, SubjectRef: subj, Subject: ScreeningSubject{VendorSubjectRef: vendorRef}, Lists: lists}
	if verr := req.Validate(); verr != nil {
		w.finish(ctx, tenantID, c, finishPlan{Result: "terminal", ErrorCode: ErrCodeInvalidResponse, Reason: "failed_terminal"}, nil, st)
		return
	}
	// Phase B: the provider call. NO transaction is held (ctx is the pass context, never a transaction's).
	o := &Orchestrator{cfg: OrchestratorConfig{Provider: w.Provider, CallTimeout: w.Config.CallTimeout, Now: now}}
	res, matches := o.callProvider(ctx, req)
	plan := planFinish(res.Outcome, matches, res.ErrorCode, c.Attempts, w.Config)
	w.finish(ctx, tenantID, c, plan, &providerFacts{id: w.Provider.ID(), ref: validProviderRef(res.ProviderReference), subject: subj, at: res.ScreenedAt,
		fingerprint: matchFingerprint(key, res.matchRefs)}, st)
}

type providerFacts struct {
	id, ref, subject string
	at               time.Time
	fingerprint      string // lic23 N-2: keyed pseudonymous identity of the match ("" when the provider gave no stable references)
}

// matchFingerprint is HMAC-SHA256(tenant key, "match-fp:v1:" + the sorted list:reference pairs), hex. It is a pseudonym: the vendor's
// match references are never stored (screening.go), only this value, so the same hit for the same person can be recognised later.
func matchFingerprint(key []byte, pairs []string) string {
	if len(pairs) == 0 || len(key) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("match-fp:v1:" + strings.Join(pairs, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

func (w *ScreeningWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// finish is phase C: apply the plan with the claim-token CAS and raise the alert in the same transaction.
func (w *ScreeningWorker) finish(ctx context.Context, tenantID uuid.UUID, c claimedRequest, plan finishPlan, f *providerFacts, st *PassStats) {
	var matchArg []string
	if len(plan.MatchLists) > 0 {
		matchArg = sortedLists(plan.MatchLists)
	}
	var errCode, provID, provRef, subj, fp *string
	var at *time.Time
	if plan.ErrorCode != "" {
		s := string(plan.ErrorCode)
		errCode = &s
	}
	if f != nil {
		p := f.id
		provID = &p
		if f.ref != "" {
			r := f.ref
			provRef = &r
		}
		s := f.subject
		subj = &s
		if f.fingerprint != "" {
			fpv := f.fingerprint
			fp = &fpv
		}
		t := f.at.UTC().Truncate(time.Microsecond)
		at = &t
	}
	var applied, reviewed bool
	var status string
	// A detached, bounded context: a cancelled pass must still be able to END a request whose provider answer it holds.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*w.Config.PhaseTimeout)
	defer cancel()
	pending, err := alerting.InTx(cctx, alerting.NewTenantRunner(w.Pool, tenantID), func(ctx context.Context, tx pgx.Tx) error {
		applied, reviewed, status = false, false, ""
		if err := tx.QueryRow(ctx, `SELECT o_applied, o_status, o_reviewed FROM aml_screening_request_finish($1, $2, $3, $4, $5::text[], $6, $7, $8, $9, $10, $11, $12)`,
			tenantID, c.ID, c.ClaimToken, plan.Result, matchArg, errCode, provID, provRef, subj, at, int(plan.Backoff/time.Second), fp).Scan(&applied, &status, &reviewed); err != nil {
			return err
		}
		// A match an officer already reviewed and cleared (same provider-stated identity) needs no attention alert.
		if !applied || plan.Reason == "" || reviewed {
			return nil
		}
		return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
			Kind: alerting.KindAMLScreeningAttention, SubjectTenantID: tenantID, Discriminator: "request:" + c.ID.String(),
			Attributes: map[string]alerting.AttrValue{"request_id": c.ID.String(), "reason": plan.Reason, "trigger": c.Trigger},
		})
	})
	defer pending.Flush(cctx)
	if err != nil {
		st.Errors++
		w.logErr("phase C failed")
		return
	}
	if !applied {
		st.Lost++ // another worker holds it, or it already ended: nothing was applied
		return
	}
	switch status {
	case "done":
		st.Done++
	case "needs_officer":
		st.NeedsOfficer++
	case "pending":
		st.Retried++
	case "failed_terminal":
		st.Terminal++
	}
}

func (w *ScreeningWorker) logErr(msg string) {
	if w.Logger != nil {
		w.Logger.Error("aml screening worker: " + msg)
	}
}

// RunScreeningWorkerLoop runs passes on a ticker until ctx is cancelled. A panic in a pass is recovered and logged (no detail).
func RunScreeningWorkerLoop(ctx context.Context, w *ScreeningWorker, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if err := w.ValidateForLoop(); err != nil {
		if logger != nil {
			logger.Error("aml screening worker: refusing to start", "error", err)
		}
		return
	}
	w.Logger = logger
	t := time.NewTicker(interval)
	defer t.Stop()
	run := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("aml screening worker: recovered from panic in pass")
			}
		}()
		w.RunPass(ctx)
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ScreeningParkedWindow is how far back the parked summary looks for the latest payout-gate decision of a person.
const ScreeningParkedWindow = 7 * 24 * time.Hour

// ScreeningParkedSummary counts, for the tenant, the persons whose latest payout-gate decision in ScreeningParkedWindow was a
// screening-only hold (the payout PARKED, lic18) and the number of such evaluations. Counts only. tx must be tenant-scoped. It is
// read by the screening worker pass AND by the payments sweeper: a dead screening worker cannot report, the sweeper can.
func ScreeningParkedSummary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (persons, evaluations int, err error) {
	err = tx.QueryRow(ctx, `SELECT o_persons, o_evaluations FROM aml_screening_parked_summary($1, clock_timestamp() - make_interval(secs => $2::double precision))`,
		tenantID, ScreeningParkedWindow.Seconds()).Scan(&persons, &evaluations)
	return persons, evaluations, err
}
