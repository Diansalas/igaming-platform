package kyc

// PRH-2 E1 (KYC-SUBMIT-OUTBOX-1; ADR 0106 revision 2, ADR 0095 section 38):
// the KYC submission outbox worker. This is the ONLY code that calls a
// KYCProvider's CreateVerification / SubmitVerification (a static test pins
// this), and no exported function in this package reaches them.
//
// Shape (ADR 0106 sections 2.6-2.9, 3):
//
//	claimNext   the worker identity (db.ServiceKYCSubmissionWorker), ONE
//	            statement: discovery + claim on kyc_submission_outbox. Its
//	            RETURNING tenant id is the only tenant id any later step uses.
//	prepare (P) WithTenant(row.tenant_id) on a bounded context: re-select the
//	            claim by (id, claim_token), then decide per section 2.9.
//	callVendor  phase B: NO transaction held (txscope.Held must be false), on
//	(B)         its own bounded context.
//	runPhaseC   phase C: alerting.InTx on a tenant runner over a detached,
//	(C)         bounded context; the claim-token CAS, the verification apply,
//	            the audit row and the alert commit atomically.
//
// An ambiguous or not-sent result NEVER writes kyc_verifications (IC
// condition 2): it schedules a database-clock backoff and ends, at worst, in
// failed_terminal plus an alert. A vendor outage can therefore never
// manufacture a `failed` verification (IC F3). Alert NOTIFICATION is NOT
// IMPLEMENTED (no route or recipient; ALERT-DELIVERY-1 OPEN).
//
// Nothing here logs or audits a vendor reference, credential, document
// content, storage reference or raw error text: only ids and closed classes.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// OutboxConfig holds the worker's technical constants. They are ENGINEERING
// RECOMMENDATIONS, reversible, NOT business, legal or compliance thresholds.
// The retry budget (BackoffBase / BackoffCap / MaxFailedAttempts) is a
// PLACEHOLDER technical bound and must never be presented as a compliance
// deadline: any KYC submission or completion deadline a jurisdiction imposes
// is jurisdiction configuration, never a worker constant (HQ-E1-3 OPEN).
type OutboxConfig struct {
	Lease             time.Duration // claim lease
	PrepareTimeout    time.Duration // bound on prepare (P)
	ResolveBudget     time.Duration // bound on the outbound credential resolve (inside phase B)
	CallTimeout       time.Duration // bound on the vendor call (inside phase B)
	PhaseCTimeout     time.Duration // bound on each phase-C attempt (detached context)
	PhaseCAttempts    int           // in-item phase-C tries before the row stays claimed (or apply_conflict)
	PassItemCap       int           // items per pass
	PerTenantCap      int           // items per tenant per pass
	BackoffBase       time.Duration // PLACEHOLDER
	BackoffCap        time.Duration // PLACEHOLDER
	MaxFailedAttempts int           // PLACEHOLDER
}

// DefaultOutboxConfig returns the ADR 0106 section 2.8 RECOMMENDATIONS.
func DefaultOutboxConfig() OutboxConfig {
	return OutboxConfig{
		Lease:             60 * time.Second,
		PrepareTimeout:    5 * time.Second,
		ResolveBudget:     5 * time.Second,
		CallTimeout:       createVerificationCallTimeout,
		PhaseCTimeout:     phaseCTimeout,
		PhaseCAttempts:    3,
		PassItemCap:       100,
		PerTenantCap:      20,
		BackoffBase:       30 * time.Second,
		BackoffCap:        30 * time.Minute,
		MaxFailedAttempts: 8,
	}
}

// PhaseBTimeout is the single bound on phase B: credential resolve plus the
// vendor call (security O-3: one context for the whole of phase B).
func (c OutboxConfig) PhaseBTimeout() time.Duration { return c.ResolveBudget + c.CallTimeout }

// leaseFloor is the F9 inequality's right-hand side:
// 2 * (prepare + resolve + call + phase C) + 5 s. P, B and C each run on their
// own bounded context, so an item cannot outlive its lease in normal operation.
func (c OutboxConfig) leaseFloor() time.Duration {
	return 2*(c.PrepareTimeout+c.ResolveBudget+c.CallTimeout+c.PhaseCTimeout) + 5*time.Second
}

// OutboxWorker drains kyc_submission_outbox. Build it from the SAME
// orchestrator and outbound credential resolver as the HTTP path
// (cmd/platform-api/registrations.go: buildKYCOutboxWorker).
type OutboxWorker struct {
	Pool         *db.Pool
	Orchestrator *Orchestrator
	Outbound     OutboundCredentialResolver
	Config       OutboxConfig
	Logger       *slog.Logger

	// ClaimScope is a TEST SEAM and is nil in production (nothing in
	// cmd/platform-api sets it; a static test pins that). When set, the claim
	// only considers rows of the returned tenants, so integration tests that
	// share one database across packages run in parallel without one test's
	// worker claiming another test's rows (code review F1). nil means every
	// tenant, the production behaviour; an empty non-nil result claims nothing.
	ClaimScope func() []uuid.UUID
}

// NewOutboxWorker constructs a worker with the default configuration.
func NewOutboxWorker(pool *db.Pool, orch *Orchestrator, outbound OutboundCredentialResolver) *OutboxWorker {
	return &OutboxWorker{Pool: pool, Orchestrator: orch, Outbound: outbound, Config: DefaultOutboxConfig()}
}

// ValidateForLoop reports a worker that must not be started as a process: a
// missing dependency, or a lease too short to cover P, B and C (F9).
func (w *OutboxWorker) ValidateForLoop() error {
	switch {
	case w == nil:
		return errors.New("kyc: outbox worker is nil")
	case w.Pool == nil:
		return errors.New("kyc: outbox worker has no pool")
	case w.Orchestrator == nil:
		return errors.New("kyc: outbox worker has no orchestrator")
	case w.Outbound == nil:
		return errors.New("kyc: outbox worker has no outbound credential resolver")
	}
	c := w.Config
	switch {
	case c.PrepareTimeout <= 0 || c.ResolveBudget <= 0 || c.CallTimeout <= 0 || c.PhaseCTimeout <= 0:
		return errors.New("kyc: outbox worker timeouts must be positive")
	case c.PhaseCAttempts < 1 || c.PassItemCap < 1 || c.PerTenantCap < 1 || c.MaxFailedAttempts < 1:
		return errors.New("kyc: outbox worker caps must be positive")
	case c.BackoffBase <= 0 || c.BackoffCap < c.BackoffBase:
		return errors.New("kyc: outbox worker backoff must satisfy 0 < base <= cap")
	case c.Lease < c.leaseFloor():
		return fmt.Errorf("kyc: outbox worker lease %s is below 2*(prepare+resolve+call+phaseC)+5s = %s: an item could outlive its lease", c.Lease, c.leaseFloor())
	case c.Lease > 10*time.Minute:
		return errors.New("kyc: outbox worker lease exceeds the database bound of 10 minutes")
	}
	return nil
}

func (w *OutboxWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// ---- metrics (no tenant, provider or row labels) ----

var outboxMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/kyc")

var (
	outboxPassesTotal, _ = outboxMeter.Int64Counter("kyc_outbox_passes_total",
		metric.WithDescription("Completed KYC outbox worker passes (PRH-2 E1)."))
	outboxItemsTotal, _ = outboxMeter.Int64Counter("kyc_outbox_items_total",
		metric.WithDescription("KYC outbox items by result: sent, retry, failed_terminal, cancelled, deferred, claim_lost, phase_c_failed, apply_conflict (PRH-2 E1)."))

	outboxLastPassUnix       atomic.Int64
	outboxOldestDueAgeSecs   atomic.Int64
	outboxOldestDeferredSecs atomic.Int64
	_, _                     = outboxMeter.Int64ObservableGauge("kyc_outbox_last_pass_unix_seconds",
		metric.WithDescription("Unix time the last KYC outbox pass completed; a stalled worker stops advancing it."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if v := outboxLastPassUnix.Load(); v != 0 {
				o.Observe(v)
			}
			return nil
		}))
	_, _ = outboxMeter.Int64ObservableGauge("kyc_outbox_oldest_due_age_seconds",
		metric.WithDescription("Largest claim lag (seconds past due) among rows claimed in the latest pass; 0 when the pass found none. Observed at claim, not a table scan."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(outboxOldestDueAgeSecs.Load())
			return nil
		}))
	_, _ = outboxMeter.Int64ObservableGauge("kyc_outbox_oldest_deferred_age_seconds",
		metric.WithDescription("Largest age (seconds since enqueue) among rows deferred for an inactive tenant in the latest pass; 0 when none. Observed at claim, not a table scan."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(outboxOldestDeferredSecs.Load())
			return nil
		}))
)

func recordOutboxItem(ctx context.Context, result string) {
	if outboxItemsTotal != nil {
		outboxItemsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

// Item results (the closed label set of kyc_outbox_items_total).
const (
	resultSent           = "sent"
	resultRetry          = "retry"
	resultFailedTerminal = "failed_terminal"
	resultCancelled      = "cancelled"
	resultDeferred       = "deferred"
	resultClaimLost      = "claim_lost"
	resultPhaseCFailed   = "phase_c_failed"
	resultApplyConflict  = "apply_conflict"
	resultPrepareFailed  = "prepare_failed"
	resultPanic          = "panic"
)

// ---- the claim (the worker identity's ONLY statement) ----

// claimedRow is the claim statement's RETURNING shape. TenantID is the only
// tenant id any later step uses (INV-KYC-OB-4).
type claimedRow struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	VerificationID uuid.UUID
	Operation      OutboxOperation
	ProviderID     string
	IdempotencyKey string
	DocumentIDs    []uuid.UUID
	ClaimToken     uuid.UUID
	Claims         int
	FailedAttempts int
	LastErrorClass string
	CreatedAt      time.Time
	NextAttemptAt  time.Time
}

// claimSQL is ADR 0106 section 2.7. The trigger forces claim_token, claimed_*,
// claims and every pinned column; RETURNING reflects the post-trigger row.
const claimSQL = `
WITH candidate AS (
    SELECT o.id
      FROM kyc_submission_outbox o
     WHERE (   (o.state = 'pending' AND o.next_attempt_at  <= now())
            OR (o.state = 'claimed' AND o.lease_expires_at <= now()))
       AND NOT (o.tenant_id = ANY ($2::uuid[]))
       AND ($3::uuid[] IS NULL OR o.tenant_id = ANY ($3::uuid[]))
       AND NOT (o.operation = 'submit' AND EXISTS (
               SELECT 1 FROM kyc_submission_outbox c
                WHERE c.tenant_id = o.tenant_id AND c.verification_id = o.verification_id
                  AND c.operation = 'create' AND c.state IN ('pending', 'claimed')))
     ORDER BY CASE o.state WHEN 'claimed' THEN o.lease_expires_at ELSE o.next_attempt_at END, o.created_at, o.id
     LIMIT 1
     FOR UPDATE OF o SKIP LOCKED
)
UPDATE kyc_submission_outbox o
   SET state = 'claimed', lease_expires_at = now() + make_interval(secs => $1)
  FROM candidate
 WHERE o.id = candidate.id
RETURNING o.id, o.tenant_id, o.verification_id, o.operation, o.provider_id,
          o.idempotency_key, o.document_ids, o.claim_token, o.claims, o.failed_attempts, o.last_error_class,
          o.created_at, o.next_attempt_at`

// claimNext is the ONLY function that uses db.ServiceKYCSubmissionWorker (a
// static test pins this). Its transaction runs exactly one statement, on
// kyc_submission_outbox. excludeTenants is the per-pass per-tenant cap: the
// tenants whose cap this pass's own claims already reached (never nil: a NULL
// array would make `ANY` filter everything).
func (w *OutboxWorker) claimNext(ctx context.Context, excludeTenants []uuid.UUID) (*claimedRow, error) {
	if excludeTenants == nil {
		excludeTenants = []uuid.UUID{}
	}
	var scope []uuid.UUID // nil = every tenant (production)
	if w.ClaimScope != nil {
		if scope = w.ClaimScope(); scope == nil {
			scope = []uuid.UUID{}
		}
	}
	var row claimedRow
	var found bool
	err := w.Pool.WithPlatformService(ctx, db.ServiceKYCSubmissionWorker, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.AssertPlatformServiceScope(ctx, tx, db.ServiceKYCSubmissionWorker); err != nil {
			return err
		}
		var lastErr *string
		scanErr := tx.QueryRow(ctx, claimSQL, w.Config.Lease.Seconds(), excludeTenants, scope).Scan(
			&row.ID, &row.TenantID, &row.VerificationID, &row.Operation, &row.ProviderID,
			&row.IdempotencyKey, &row.DocumentIDs, &row.ClaimToken, &row.Claims, &row.FailedAttempts, &lastErr,
			&row.CreatedAt, &row.NextAttemptAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		if lastErr != nil {
			row.LastErrorClass = *lastErr
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("kyc: claim outbox row: %w", err)
	}
	if !found {
		return nil, nil
	}
	return &row, nil
}

// ---- tenant-session SQL (every statement carries claim_token = $2) ----

const (
	sqlOutboxLock = `SELECT id FROM kyc_submission_outbox WHERE id = $1 AND state = 'claimed' AND claim_token = $2 FOR UPDATE`

	sqlOutboxSent = `UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1 AND state = 'claimed' AND claim_token = $2`

	sqlOutboxCancel = `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = $3 WHERE id = $1 AND state = 'claimed' AND claim_token = $2`

	sqlOutboxRetry = `UPDATE kyc_submission_outbox SET state = 'pending', last_error_class = $3,
		next_attempt_at = now() + make_interval(secs => $4::float8) WHERE id = $1 AND state = 'claimed' AND claim_token = $2`

	sqlOutboxTerminal = `UPDATE kyc_submission_outbox SET state = 'failed_terminal', last_error_class = $3 WHERE id = $1 AND state = 'claimed' AND claim_token = $2`

	// The one tenant-side UPDATE that is not under a claim token: the
	// trigger-validated pending -> cancelled / verification_not_submitted
	// cascade when a verification's create ended terminal (IC Q-R1(a)).
	sqlOutboxCascadeCancelSubmits = `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'verification_not_submitted'
		WHERE tenant_id = $1 AND verification_id = $2 AND operation = 'submit' AND state = 'pending' RETURNING id`
)

// errLostClaim means the (id, claim_token) CAS found no claimed row: the lease
// expired and another worker re-claimed it, or the tenant id was not the
// claim's. The caller rolls back and discards its result.
var errLostClaim = errors.New("kyc: outbox claim lost")

// workerAudit is the audit identity of every worker-written row (security
// Q-S3): actor system, actor_id NULL (the writer passes uuid.Nil), and
// metadata.platform_service as a compiled constant plus the outbox context.
type workerAudit struct {
	meta map[string]any
}

func (a workerAudit) merge(base map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range a.meta {
		base[k] = v
	}
	return base
}

func newWorkerAudit(row claimedRow) workerAudit {
	return workerAudit{meta: map[string]any{
		"platform_service": workerPlatformService,
		"outbox_id":        row.ID.String(),
		"operation":        string(row.Operation),
		"claims":           row.Claims,
		"failed_attempts":  row.FailedAttempts,
	}}
}

func (w *OutboxWorker) record(ctx context.Context, tx pgx.Tx, row claimedRow, action string, outcome audit.Outcome, extra map[string]any) error {
	meta := newWorkerAudit(row).merge(extra)
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: row.TenantID, ActorType: audit.ActorSystem,
		Action: action, TargetType: "kyc_verification", TargetID: row.VerificationID.String(),
		Outcome: outcome, Metadata: meta,
	})
}

func lockClaim(ctx context.Context, tx pgx.Tx, row claimedRow) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, sqlOutboxLock, row.ID, row.ClaimToken).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errLostClaim
	}
	if err != nil {
		return fmt.Errorf("kyc: lock outbox claim: %w", err)
	}
	return nil
}

func casOne(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("kyc: outbox transition: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errLostClaim
	}
	return nil
}

// ---- prepare (P) ----

type prepKind int

const (
	prepProceed prepKind = iota
	prepHandled
	prepLost
	prepTerminalLeaseExpired
)

type prepared struct {
	verification Verification
	docs         []SubmittedDocument
}

type prepOutcome struct {
	kind   prepKind
	prep   *prepared
	result string
}

// prepare is phase P: one tenant transaction for the CLAIM's tenant id on a
// bounded context. Cancel and defer decisions write their transition and audit
// here; the proceed decision commits with no write. The terminal decision for a
// re-claim whose attempts are exhausted is handed to phase C (it needs the
// alerting.InTx owner).
func (w *OutboxWorker) prepare(ctx context.Context, row claimedRow) (prepOutcome, error) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Config.PrepareTimeout)
	defer cancel()
	var out prepOutcome
	err := w.Pool.WithTenant(pctx, row.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = w.prepareTx(ctx, tx, row)
		return err
	})
	if errors.Is(err, errLostClaim) {
		return prepOutcome{kind: prepLost, result: resultClaimLost}, nil
	}
	if err != nil {
		return prepOutcome{}, err
	}
	return out, nil
}

func (w *OutboxWorker) prepareTx(ctx context.Context, tx pgx.Tx, row claimedRow) (prepOutcome, error) {
	if err := lockClaim(ctx, tx, row); err != nil {
		return prepOutcome{}, err
	}

	// 1. tenant not active: defer without consuming an attempt (HQ-E1-1 default).
	var tenantStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, row.TenantID).Scan(&tenantStatus); err != nil {
		return prepOutcome{}, fmt.Errorf("kyc: read tenant status: %w", err)
	}
	if tenantStatus != "active" {
		delay := backoff(row.Claims, w.Config.BackoffBase, w.Config.BackoffCap)
		if err := casOne(ctx, tx, sqlOutboxRetry, row.ID, row.ClaimToken, string(ClassDeferredTenantInactive), delay.Seconds()); err != nil {
			return prepOutcome{}, err
		}
		return prepOutcome{kind: prepHandled, result: resultDeferred}, nil
	}

	// 2. F4: the pinned provider must still be configured for this tenant.
	configured, err := w.Orchestrator.providerStillConfigured(ctx, tx, row.TenantID, row.ProviderID)
	if err != nil {
		return prepOutcome{}, fmt.Errorf("kyc: re-check provider: %w", err)
	}
	if !configured {
		return w.cancelInTx(ctx, tx, row, CancelProviderDeconfigured)
	}

	// 3. a re-claim whose retry budget is exhausted ends terminal (phase C owns the alert).
	if row.FailedAttempts >= w.Config.MaxFailedAttempts {
		return prepOutcome{kind: prepTerminalLeaseExpired}, nil
	}

	v, err := GetVerificationByID(ctx, tx, row.VerificationID)
	if err != nil {
		return prepOutcome{}, err
	}
	isOrphan := v.Status == StatusUnverified && v.ProviderReference == ""

	if row.Operation == OpCreate {
		// 4. the orphan was decided (staff review, IC C3): never send, never overwrite.
		if !isOrphan {
			return w.cancelInTx(ctx, tx, row, CancelDecidedConcurrently)
		}
		return prepOutcome{kind: prepProceed, prep: &prepared{verification: v}}, nil
	}

	// submit
	// 5. terminal verification.
	if isTerminal(v.Status) {
		return w.cancelInTx(ctx, tx, row, CancelVerificationTerminal)
	}
	// 6. a newer submit row for the same verification is live or sent.
	var newer bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM kyc_submission_outbox n
		                 WHERE n.tenant_id = $1 AND n.verification_id = $2 AND n.operation = 'submit'
		                   AND n.state IN ('pending', 'claimed', 'sent')
		                   AND (n.created_at, n.id) > (SELECT created_at, id FROM kyc_submission_outbox WHERE id = $3))`,
		row.TenantID, row.VerificationID, row.ID).Scan(&newer); err != nil {
		return prepOutcome{}, fmt.Errorf("kyc: read newer submit row: %w", err)
	}
	if newer {
		return w.cancelInTx(ctx, tx, row, CancelSuperseded)
	}
	// 7. no vendor reference to submit against: either still an orphan (its
	// create ended terminal) or decided/changed without ever binding one (a staff
	// decision, or decided_concurrently, whose cascade could not see a submit row
	// committed after it). Submitting with an empty reference would send PII
	// documents the vendor cannot attach (code review F4); phase B refuses the
	// same state independently.
	if isOrphan || v.ProviderReference == "" {
		return w.cancelInTx(ctx, tx, row, CancelVerificationNotSubmitted)
	}
	// 8/9. the current non-rejected set.
	_, docs, ok, err := gatherSubmissionDocuments(ctx, tx, row.VerificationID)
	if err != nil {
		return prepOutcome{}, err
	}
	if !ok {
		return w.cancelInTx(ctx, tx, row, CancelVerificationTerminal)
	}
	if len(docs) == 0 {
		return w.cancelInTx(ctx, tx, row, CancelNoDocuments)
	}
	if !sameDocumentSet(sortedDocumentIDs(docs), row.DocumentIDs) {
		// A staff rejection or a deleted document since enqueue (IC C5): cancel
		// and re-enqueue the current set in the SAME tx; DO NOTHING on a live
		// or sent row of the same set (F12: never a 23505 abort).
		out, err := w.cancelInTx(ctx, tx, row, CancelDocumentSetChanged)
		if err != nil {
			return prepOutcome{}, err
		}
		if _, _, err := enqueueSubmitRow(ctx, tx, v, docs, workerEnqueueActor()); err != nil {
			return prepOutcome{}, err
		}
		return out, nil
	}
	return prepOutcome{kind: prepProceed, prep: &prepared{verification: v, docs: docs}}, nil
}

func sameDocumentSet(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cancelInTx moves the claimed row to cancelled with reason and writes one
// audit row. When a CREATE row is cancelled, every pending submit row of the
// verification is cancelled with it (IC Q-R1(a)).
func (w *OutboxWorker) cancelInTx(ctx context.Context, tx pgx.Tx, row claimedRow, reason OutboxCancelReason) (prepOutcome, error) {
	if err := casOne(ctx, tx, sqlOutboxCancel, row.ID, row.ClaimToken, string(reason)); err != nil {
		return prepOutcome{}, err
	}
	if err := w.record(ctx, tx, row, auditActionSubmissionCancelled, audit.OutcomeSuccess,
		map[string]any{"cancel_reason": string(reason), "provider_id": row.ProviderID}); err != nil {
		return prepOutcome{}, err
	}
	if row.Operation == OpCreate {
		if err := w.cascadeCancelSubmits(ctx, tx, row); err != nil {
			return prepOutcome{}, err
		}
	}
	return prepOutcome{kind: prepHandled, result: resultCancelled}, nil
}

// cascadeCancelSubmits moves every pending submit row of the row's verification
// to cancelled / verification_not_submitted with one audit row each. The
// database validates that the create row is terminal (trigger).
func (w *OutboxWorker) cascadeCancelSubmits(ctx context.Context, tx pgx.Tx, row claimedRow) error {
	rows, err := tx.Query(ctx, sqlOutboxCascadeCancelSubmits, row.TenantID, row.VerificationID)
	if err != nil {
		return fmt.Errorf("kyc: cascade cancel pending submits: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("kyc: scan cascaded submit row: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kyc: cascade cancel pending submits: %w", err)
	}
	for _, id := range ids {
		meta := newWorkerAudit(row).merge(map[string]any{
			"outbox_id": id.String(), "operation": string(OpSubmit),
			"cancel_reason": string(CancelVerificationNotSubmitted), "cascaded_from_outbox_id": row.ID.String(),
		})
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: row.TenantID, ActorType: audit.ActorSystem,
			Action: auditActionSubmissionCancelled, TargetType: "kyc_verification", TargetID: row.VerificationID.String(),
			Outcome: audit.OutcomeSuccess, Metadata: meta,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ageSeconds is the non-negative whole seconds since t (the per-pass deferred
// age maximum, mirroring the due-lag maximum: a gauge of the LATEST pass).
func ageSeconds(t time.Time) int64 {
	age := int64(time.Since(t).Seconds())
	if age < 0 {
		return 0
	}
	return age
}

// ---- phase B ----

type vendorOutcomeKind int

const (
	outcomeDefinitive vendorOutcomeKind = iota
	outcomeAmbiguous
	outcomeNotSent
	outcomeBindingMismatch
)

type vendorOutcome struct {
	kind vendorOutcomeKind
	// result is the provider's result (definitive, or an explicit ProviderError
	// outcome for a submit whose existing failure audit row is kept).
	result    ProviderResult
	hasResult bool
}

// callVendor is phase B: NO transaction is held (txscope.Held must be false),
// on one bounded context covering the credential resolve and the vendor call.
// It is the only place a KYCProvider's CreateVerification or
// SubmitVerification is called. Document content is NOT read here: today
// SubmittedDocument carries ids and types only and no adapter consumes bytes.
// A content-consuming adapter must read content here, in phase B, outside any
// transaction, never in the worker service tx and never persisted in the
// outbox; that needs its own design for the sensitive-document-access audit
// (ADR 0106 section 2.6 B row, IC C5) and is NOT BUILT.
func (w *OutboxWorker) callVendor(ctx context.Context, row claimedRow, prep *prepared) vendorOutcome {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Config.PhaseBTimeout())
	defer cancel()
	if txscope.Held(ctx) || txscope.Held(bctx) {
		return vendorOutcome{kind: outcomeNotSent}
	}
	var provider KYCProvider
	if w.Orchestrator != nil {
		if p, ok := w.Orchestrator.Provider(row.ProviderID); ok && p != nil {
			provider = p
		}
	}
	if provider == nil || w.Outbound == nil {
		return vendorOutcome{kind: outcomeNotSent}
	}
	rctx, rcancel := context.WithTimeout(bctx, w.Config.ResolveBudget)
	cred, err := w.Outbound.Resolve(rctx, w.Pool, row.TenantID, row.ProviderID)
	rcancel()
	if err != nil {
		w.logger().Warn("kyc_outbox_credential_unavailable",
			"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "operation", string(row.Operation), "class", string(ClassNotSent))
		return vendorOutcome{kind: outcomeNotSent}
	}
	// Defence in depth (ADR 0095 section 9.1): the credential must bind to the
	// SAME tenant, provider and domain as the row. A mismatch is an integrity
	// signal, never a retryable not_sent (security F5).
	if cred.TenantID != row.TenantID || cred.ProviderID != row.ProviderID || cred.Domain != "kyc" {
		return vendorOutcome{kind: outcomeBindingMismatch}
	}
	call := CallContext{
		TenantID: row.TenantID, ProviderID: row.ProviderID, Credential: cred,
		IdempotencyKey: row.IdempotencyKey, Deadline: time.Now().Add(w.Config.CallTimeout),
	}
	// IO-1B: refuse the adapter call itself if ctx is marked as holding a
	// pooled database transaction (second control behind the API shape).
	if txscope.Held(bctx) {
		return vendorOutcome{kind: outcomeNotSent}
	}

	switch row.Operation {
	case OpCreate:
		v := prep.verification
		result, callErr := provider.CreateVerification(bctx, CreateVerificationInput{ //nolint:staticcheck // S1016: explicit field-by-field literal so a field later added to CreateVerificationParams is never passed to the provider by accident
			TenantID: v.TenantID, BrandID: v.BrandID,
			PlayerAccountID: v.PlayerAccountID, PersonID: v.PersonID, Call: call,
		})
		if callErr != nil {
			w.logger().Warn("kyc_outbox_provider_call_failed",
				"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "operation", string(row.Operation), "class", string(ClassAmbiguous))
			return vendorOutcome{kind: outcomeAmbiguous}
		}
		if _, ok := statusForOutcome(result.Outcome); !ok && result.ProviderReference == "" {
			// An unrecognized outcome (including ProviderError) with NO
			// reference is never applied (R2, ADR 0096 section 20.5):
			// ambiguous, the orphan is left as is. With a genuine reference
			// the vendor accepted the request, so the reference is bound and
			// the row becomes pending (existing, tested behaviour: discarding
			// it would leave an unbound vendor-side verification and re-send).
			return vendorOutcome{kind: outcomeAmbiguous}
		}
		return vendorOutcome{kind: outcomeDefinitive, result: result, hasResult: true}
	default:
		v := prep.verification
		if v.ProviderReference == "" {
			// Defence in depth behind prepareTx step 7: never send documents
			// against an empty vendor reference.
			return vendorOutcome{kind: outcomeNotSent}
		}
		result, callErr := provider.SubmitVerification(bctx, v.ProviderReference, prep.docs, call)
		if callErr != nil {
			w.logger().Warn("kyc_outbox_provider_call_failed",
				"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "operation", string(row.Operation), "class", string(ClassAmbiguous))
			return vendorOutcome{kind: outcomeAmbiguous}
		}
		if result.Outcome == ProviderError {
			return vendorOutcome{kind: outcomeAmbiguous, result: result, hasResult: true}
		}
		if _, ok := statusForOutcome(result.Outcome); !ok {
			return vendorOutcome{kind: outcomeAmbiguous}
		}
		return vendorOutcome{kind: outcomeDefinitive, result: result, hasResult: true}
	}
}

// ---- phase C ----

// phaseCApplied is one phase-C attempt's committed result.
type phaseCApplied struct {
	result string
}

// runPhaseC applies one vendor outcome (or the terminal decision handed over
// from P) in a NEW tenant transaction opened through alerting.InTx on a
// detached, bounded context: the claim-token CAS, the verification apply, the
// audit row and the alert commit atomically; Pending.Flush runs after the
// commit. A lost claim rolls back and discards the result. A failure after the
// vendor accepted is retried within the bounded budget; a deterministic
// SQLSTATE class 22/23 ends the row failed_terminal / apply_conflict (never a
// re-send loop); any other persistent failure leaves the row claimed, so lease
// expiry re-claims it and re-sends with the SAME idempotency key.
func (w *OutboxWorker) runPhaseC(ctx context.Context, row claimedRow, prep *prepared, out vendorOutcome, terminalLeaseExpired bool) string {
	var lastErr error
	for attempt := 1; attempt <= w.Config.PhaseCAttempts; attempt++ {
		phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Config.PhaseCTimeout)
		var applied phaseCApplied
		pending, err := alerting.InTx(phaseCtx, alerting.NewTenantRunner(w.Pool, row.TenantID), func(ctx context.Context, tx pgx.Tx) error {
			var applyErr error
			applied, applyErr = w.applyOutcome(ctx, tx, row, prep, out, terminalLeaseExpired)
			return applyErr
		})
		cancel()
		if err == nil {
			pending.Flush(ctx)
			return applied.result
		}
		if errors.Is(err, errLostClaim) {
			w.logger().Warn("kyc_outbox_claim_lost", "tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "phase", "C")
			return resultClaimLost
		}
		lastErr = err
	}
	if isApplyConflictError(lastErr) && !terminalLeaseExpired {
		return w.recordApplyConflict(ctx, row)
	}
	w.logger().Error("kyc_outbox_phase_c_failed",
		"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "operation", string(row.Operation), "detail", RedactedProviderErrorDetail(lastErr))
	return resultPhaseCFailed
}

// isApplyConflictError reports a deterministic apply failure: SQLSTATE class 22
// (data exception) or 23 (integrity constraint violation). Class 40
// (serialization/deadlock) is deliberately NOT included (security M37).
func isApplyConflictError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	class := pgErr.Code[:2]
	return class == "22" || class == "23"
}

// recordApplyConflict ends the row failed_terminal / apply_conflict in a short
// new tenant transaction (a second alerting.InTx of the same shape). The audit
// row names that a vendor-side artefact is not reflected on the platform
// (vendor_reference_unbound for a create, never the value).
func (w *OutboxWorker) recordApplyConflict(ctx context.Context, row claimedRow) string {
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Config.PhaseCTimeout)
	defer cancel()
	pending, err := alerting.InTx(phaseCtx, alerting.NewTenantRunner(w.Pool, row.TenantID), func(ctx context.Context, tx pgx.Tx) error {
		if err := lockClaim(ctx, tx, row); err != nil {
			return err
		}
		extra := map[string]any{"vendor_state_unreflected": true}
		if row.Operation == OpCreate {
			extra = map[string]any{"vendor_reference_unbound": true}
		}
		return w.failTerminal(ctx, tx, row, ClassApplyConflict, extra)
	})
	if err != nil {
		if errors.Is(err, errLostClaim) {
			return resultClaimLost
		}
		w.logger().Error("kyc_outbox_apply_conflict_record_failed",
			"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "detail", RedactedProviderErrorDetail(err))
		return resultPhaseCFailed
	}
	pending.Flush(ctx)
	return resultApplyConflict
}

// applyOutcome is one phase-C attempt inside the alerting.InTx closure.
func (w *OutboxWorker) applyOutcome(ctx context.Context, tx pgx.Tx, row claimedRow, prep *prepared, out vendorOutcome, terminalLeaseExpired bool) (phaseCApplied, error) {
	// Lock order (Q-R4): the outbox row (child) first, then the verification.
	if err := lockClaim(ctx, tx, row); err != nil {
		return phaseCApplied{}, err
	}
	if terminalLeaseExpired {
		if err := w.failTerminal(ctx, tx, row, ClassLeaseExpired, nil); err != nil {
			return phaseCApplied{}, err
		}
		return phaseCApplied{result: resultFailedTerminal}, nil
	}
	wa := newWorkerAudit(row)
	switch out.kind {
	case outcomeDefinitive:
		if row.Operation == OpCreate {
			_, err := applyCreateVerificationResult(ctx, tx, row.TenantID, prep.verification, out.result, wa)
			if errors.Is(err, errCreateDecidedConcurrently) {
				// IC C3: never overwrite or demote the staff decision. The
				// vendor accepted a verification that is now bound to no
				// platform row; the audit row says so (never the value).
				if cErr := casOne(ctx, tx, sqlOutboxCancel, row.ID, row.ClaimToken, string(CancelDecidedConcurrently)); cErr != nil {
					return phaseCApplied{}, cErr
				}
				if aErr := w.record(ctx, tx, row, auditActionSubmissionCancelled, audit.OutcomeSuccess, map[string]any{
					"cancel_reason": string(CancelDecidedConcurrently), "provider_id": row.ProviderID, "vendor_reference_unbound": true,
				}); aErr != nil {
					return phaseCApplied{}, aErr
				}
				if cErr := w.cascadeCancelSubmits(ctx, tx, row); cErr != nil {
					return phaseCApplied{}, cErr
				}
				return phaseCApplied{result: resultCancelled}, nil
			}
			if err != nil {
				return phaseCApplied{}, err
			}
		} else {
			if _, err := applySubmissionResult(ctx, tx, prep.verification, prep.docs, out.result, wa); err != nil {
				return phaseCApplied{}, err
			}
		}
		if err := casOne(ctx, tx, sqlOutboxSent, row.ID, row.ClaimToken); err != nil {
			return phaseCApplied{}, err
		}
		return phaseCApplied{result: resultSent}, nil

	case outcomeBindingMismatch:
		// An integrity signal: terminal at once, distinct discriminator (F5).
		if err := w.failTerminal(ctx, tx, row, ClassCredentialBindingMismatch, nil); err != nil {
			return phaseCApplied{}, err
		}
		return phaseCApplied{result: resultFailedTerminal}, nil

	default: // ambiguous, not sent
		class := ClassAmbiguous
		if out.kind == outcomeNotSent {
			class = ClassNotSent
		}
		if out.hasResult && row.Operation == OpSubmit && out.result.Outcome == ProviderError {
			// Keep the existing failure audit row for an explicit ProviderError
			// outcome; the verification status is left exactly as it was (IC
			// condition 2).
			if _, err := applySubmissionResult(ctx, tx, prep.verification, prep.docs, out.result, wa); err != nil {
				return phaseCApplied{}, err
			}
		}
		attemptsAfter := row.FailedAttempts + 1
		if attemptsAfter >= w.Config.MaxFailedAttempts {
			if err := w.failTerminal(ctx, tx, row, class, nil); err != nil {
				return phaseCApplied{}, err
			}
			return phaseCApplied{result: resultFailedTerminal}, nil
		}
		delay := backoff(attemptsAfter, w.Config.BackoffBase, w.Config.BackoffCap)
		if err := casOne(ctx, tx, sqlOutboxRetry, row.ID, row.ClaimToken, string(class), delay.Seconds()); err != nil {
			return phaseCApplied{}, err
		}
		if err := w.record(ctx, tx, row, auditActionSubmissionRetryScheduled, audit.OutcomeFailure, map[string]any{
			"error_class": string(class), "provider_id": row.ProviderID, "failed_attempts": attemptsAfter,
		}); err != nil {
			return phaseCApplied{}, err
		}
		return phaseCApplied{result: resultRetry}, nil
	}
}

// failTerminal moves the locked claimed row to failed_terminal with class,
// writes the audit row, cascades pending submits of a failed CREATE, and raises
// the KYC alert in the same transaction. It never writes kyc_verifications.
func (w *OutboxWorker) failTerminal(ctx context.Context, tx pgx.Tx, row claimedRow, class OutboxErrorClass, extra map[string]any) error {
	if err := casOne(ctx, tx, sqlOutboxTerminal, row.ID, row.ClaimToken, string(class)); err != nil {
		return err
	}
	attempts := row.FailedAttempts + 1
	if class == ClassLeaseExpired {
		attempts = row.FailedAttempts // already counted at re-claim
	}
	meta := map[string]any{"error_class": string(class), "provider_id": row.ProviderID, "failed_attempts": attempts}
	for k, v := range extra {
		meta[k] = v
	}
	if err := w.record(ctx, tx, row, auditActionSubmissionFailedTerminal, audit.OutcomeFailure, meta); err != nil {
		return err
	}
	if row.Operation == OpCreate {
		if err := w.cascadeCancelSubmits(ctx, tx, row); err != nil {
			return err
		}
	}
	return raiseSubmissionFailedTerminal(ctx, tx, row, class)
}

// providerIDForAlert matches migration 0114's provider_id CHECK.
var providerIDForAlert = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// submissionDiscriminator is the closed-set alert discriminator (ADR 0106
// section 4.1): `<operation>:<provider_id>` for outage classes, with a
// `:binding_mismatch` or `:apply_conflict` suffix for the integrity classes.
// The provider id falls back to provider_unclassified defensively.
func submissionDiscriminator(op OutboxOperation, providerID string, class OutboxErrorClass) string {
	p := providerID
	if !providerIDForAlert.MatchString(p) {
		p = "provider_unclassified"
	}
	d := string(op) + ":" + p
	switch class {
	case ClassCredentialBindingMismatch:
		d += ":binding_mismatch"
	case ClassApplyConflict:
		d += ":apply_conflict"
	}
	return d
}

// raiseSubmissionFailedTerminal raises the platform-owned, subject-tenant KYC
// Kind in the CALLER's tenant transaction (alerting.RaiseGuarded inside the
// phase-C alerting.InTx). Allowed attributes only: operation, provider_id,
// last_error_class, outbox_id. The worker identity never raises. Delivery is
// NOT IMPLEMENTED (no route or recipient; ALERT-DELIVERY-1 OPEN).
func raiseSubmissionFailedTerminal(ctx context.Context, tx pgx.Tx, row claimedRow, class OutboxErrorClass) error {
	p := row.ProviderID
	if !providerIDForAlert.MatchString(p) {
		p = "provider_unclassified"
	}
	return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
		Kind:            alerting.KindKYCSubmissionFailedTerminal,
		SubjectTenantID: row.TenantID,
		Discriminator:   submissionDiscriminator(row.Operation, row.ProviderID, class),
		Attributes: map[string]alerting.AttrValue{
			"operation":        string(row.Operation),
			"provider_id":      p,
			"last_error_class": string(class),
			"outbox_id":        row.ID.String(),
		},
	})
}

// ---- the item and the pass ----

// processItem runs P, B and C for one claimed row and returns the closed
// result label. A returned error class leaves the row claimed (lease expiry
// re-claims it).
func (w *OutboxWorker) processItem(ctx context.Context, row claimedRow) string {
	po, err := w.prepare(ctx, row)
	if err != nil {
		w.logger().Error("kyc_outbox_prepare_failed",
			"tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "operation", string(row.Operation), "detail", RedactedProviderErrorDetail(err))
		return resultPrepareFailed
	}
	switch po.kind {
	case prepLost:
		w.logger().Warn("kyc_outbox_claim_lost", "tenant_id", row.TenantID.String(), "outbox_id", row.ID.String(), "phase", "P")
		return resultClaimLost
	case prepHandled:
		return po.result
	case prepTerminalLeaseExpired:
		return w.runPhaseC(ctx, row, nil, vendorOutcome{}, true)
	}
	out := w.callVendor(ctx, row, po.prep)
	return w.runPhaseC(ctx, row, po.prep, out, false)
}

// processItemSafe isolates a panic in one item: the row stays claimed and its
// lease expiry re-claims it.
func (w *OutboxWorker) processItemSafe(ctx context.Context, row claimedRow) (result string) {
	defer func() {
		if r := recover(); r != nil {
			w.logger().Error("kyc_outbox_item_panic", "tenant_id", row.TenantID.String(), "outbox_id", row.ID.String())
			result = resultPanic
		}
	}()
	return w.processItem(ctx, row)
}

// PassStats summarises one pass (for tests and the loop's after-pass hook).
type PassStats struct {
	Claimed    int
	Results    map[string]int
	ClaimError bool
}

// RunPass drains up to PassItemCap due rows (PerTenantCap per tenant), one row
// per claim transaction, sequentially. Once ctx is cancelled no new row is
// claimed; the item in flight finishes on its own detached, bounded contexts.
func (w *OutboxWorker) RunPass(ctx context.Context) PassStats {
	stats := PassStats{Results: map[string]int{}}
	perTenant := map[uuid.UUID]int{}
	var maxDueLag, maxDeferredAge int64
	for i := 0; i < w.Config.PassItemCap; i++ {
		if ctx.Err() != nil {
			break
		}
		exclude := make([]uuid.UUID, 0, len(perTenant))
		for tid, n := range perTenant {
			if n >= w.Config.PerTenantCap {
				exclude = append(exclude, tid)
			}
		}
		row, err := w.claimNext(ctx, exclude)
		if err != nil {
			w.logger().Error("kyc_outbox_claim_failed", "detail", RedactedProviderErrorDetail(err))
			stats.ClaimError = true
			break
		}
		if row == nil {
			break
		}
		stats.Claimed++
		perTenant[row.TenantID]++
		if lag := int64(time.Since(row.NextAttemptAt).Seconds()); lag > maxDueLag {
			maxDueLag = lag
		}
		result := w.processItemSafe(ctx, *row)
		stats.Results[result]++
		recordOutboxItem(ctx, result)
		if result == resultDeferred {
			if age := ageSeconds(row.CreatedAt); age > maxDeferredAge {
				maxDeferredAge = age
			}
		}
	}
	outboxOldestDueAgeSecs.Store(maxDueLag)
	outboxOldestDeferredSecs.Store(maxDeferredAge)
	return stats
}

// RunOutboxWorkerLoop runs RunPass immediately and then on every interval
// until ctx is cancelled, then returns once the pass in flight has drained
// (ADR 0095 section 37.4 shape). A worker that fails ValidateForLoop is
// refused at Error level and never started.
func RunOutboxWorkerLoop(ctx context.Context, w *OutboxWorker, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runOutboxWorkerLoop(ctx, w, logger, ticker.C, nil)
}

// runOutboxWorkerLoop is RunOutboxWorkerLoop with an injectable tick source
// (T-1) and an optional after-pass callback, so tests drive passes
// deterministically.
func runOutboxWorkerLoop(ctx context.Context, w *OutboxWorker, logger *slog.Logger, ticks <-chan time.Time, afterPass func(PassStats)) {
	if err := w.ValidateForLoop(); err != nil {
		if logger != nil {
			logger.Error("kyc outbox worker: refusing to start", "error", err)
		}
		return
	}
	runOnce := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("kyc outbox worker: recovered from panic in pass")
			}
		}()
		st := w.RunPass(ctx)
		if !st.ClaimError {
			outboxLastPassUnix.Store(time.Now().Unix())
		}
		if outboxPassesTotal != nil {
			outboxPassesTotal.Add(ctx, 1)
		}
		if afterPass != nil {
			afterPass(st)
		}
	}
	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			runOnce()
		}
	}
}
