package kyc

// PRH-2 E1 (KYC-SUBMIT-OUTBOX-1; ADR 0106 revision 2, ADR 0095 section 38):
// the KYC create/submit outbox. This file holds the row model, the closed
// vocabularies, the phase-A enqueue helpers (run on the CALLER's tenant
// transaction, in the same transaction as the domain row they accompany),
// the pure backoff function and the idempotent-create read. The worker that
// drains the outbox is outbox_worker.go; the staff-visible derived state is
// submission_state.go.
//
// What the outbox stores: ids, states and counters ONLY (migration 0114). No
// PII, document content, storage reference, vendor reference, credential or
// raw error text. Enforcement NEVER reads it (INV-KYC-OB-5, a static test
// asserts this): a pending/claimed create row leaves its verification in the
// excluded phase-A orphan shape (ADR 0096 section 2.6(g)), so an outage of any
// length can never manufacture a `failed` (IC F3).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// OutboxOperation is kyc_submission_outbox.operation.
type OutboxOperation string

const (
	OpCreate OutboxOperation = "create"
	OpSubmit OutboxOperation = "submit"
)

// OutboxState is kyc_submission_outbox.state.
type OutboxState string

const (
	OutboxPending        OutboxState = "pending"
	OutboxClaimed        OutboxState = "claimed"
	OutboxSent           OutboxState = "sent"
	OutboxFailedTerminal OutboxState = "failed_terminal"
	OutboxCancelled      OutboxState = "cancelled"
)

// OutboxErrorClass is the closed kyc_submission_outbox.last_error_class set.
// It is the ONLY error text the outbox, its audit rows and its alert carry.
type OutboxErrorClass string

const (
	ClassAmbiguous                 OutboxErrorClass = "ambiguous"
	ClassNotSent                   OutboxErrorClass = "not_sent"
	ClassLeaseExpired              OutboxErrorClass = "lease_expired"
	ClassDeferredTenantInactive    OutboxErrorClass = "deferred_tenant_inactive"
	ClassCredentialBindingMismatch OutboxErrorClass = "credential_binding_mismatch"
	ClassApplyConflict             OutboxErrorClass = "apply_conflict"
)

// OutboxCancelReason is the closed kyc_submission_outbox.cancel_reason set.
type OutboxCancelReason string

const (
	CancelSuperseded               OutboxCancelReason = "superseded"
	CancelVerificationTerminal     OutboxCancelReason = "verification_terminal"
	CancelNoDocuments              OutboxCancelReason = "no_documents"
	CancelDocumentSetChanged       OutboxCancelReason = "document_set_changed"
	CancelVerificationNotSubmitted OutboxCancelReason = "verification_not_submitted"
	CancelProviderDeconfigured     OutboxCancelReason = "provider_deconfigured"
	CancelDecidedConcurrently      OutboxCancelReason = "decided_concurrently"
)

// Audit actions the outbox adds (ADR 0106 section 3.5).
const (
	auditActionSubmissionEnqueued       = "kyc.submission_enqueued"
	auditActionSubmissionRetryScheduled = "kyc.submission_retry_scheduled"
	auditActionSubmissionFailedTerminal = "kyc.submission_failed_terminal"
	auditActionSubmissionCancelled      = "kyc.submission_cancelled"
)

// outboxLiveCreatePerPlayerIndex is the partial unique index that bounds the
// player-triggered create amplification (security F3 / IC C2).
const outboxLiveCreatePerPlayerIndex = "kyc_submission_outbox_one_live_create_per_player"

// requestVerificationTestHook is a TEST-ONLY seam (always nil in production -
// never set outside test code, mirroring reviewVerificationTestRaceHook). It is
// called with stage "after_conflict" in the exact window between
// RequestVerification's 23505 on the per-player live-create index and its read
// of the existing row, and with stage "before_retry" at the start of the retry
// attempt, so a test can move the winning create row (to sent, or to a terminal
// state) in those windows deterministically (security D2).
var requestVerificationTestHook func(stage string, attempt int)

// ErrVerificationCreateContention is returned by RequestVerification when
// concurrent creates for one player kept changing the live row under it and
// no existing verification could be returned. The HTTP layer maps it to a
// retryable 503, never to an empty 200 or a 500.
var ErrVerificationCreateContention = errors.New("kyc: concurrent verification create; retry")

// errCreateDecidedConcurrently is applyCreateVerificationResult's typed CAS
// miss: the orphan row was decided (a staff ReviewVerification) between phase
// A and phase C (IC C3). The worker maps it to cancelled/decided_concurrently;
// the staff decision is never overwritten or demoted.
var errCreateDecidedConcurrently = errors.New("kyc: create verification result not applied: the orphan was decided concurrently")

// backoff is the pure retry-delay function (ADR 0106 section 2.8): the n-th
// failed attempt (n >= 1) waits min(base * 2^(n-1), cap), with no jitter. The
// deadline itself is computed by the DATABASE clock (now() + make_interval).
func backoff(n int, base, ceiling time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if n < 1 {
		n = 1
	}
	d := base
	for i := 1; i < n; i++ {
		if d >= ceiling || d > ceiling/2 {
			return ceiling
		}
		d *= 2
	}
	if ceiling > 0 && d > ceiling {
		return ceiling
	}
	return d
}

// enqueueActor names who is recorded on an enqueue audit row. Phase A rows are
// written by the player's request; rows the worker writes in P are system rows
// carrying metadata.platform_service.
type enqueueActor struct {
	actorType audit.ActorType
	actorID   uuid.UUID
	extra     map[string]any
}

func playerEnqueueActor(playerAccountID uuid.UUID) enqueueActor {
	return enqueueActor{actorType: audit.ActorPlayer, actorID: playerAccountID}
}

// workerPlatformService is the compiled constant every worker-written audit
// row carries as metadata.platform_service (security Q-S3). It is the actor
// label only; the worker's tenant-data work runs in plain tenant transactions.
const workerPlatformService = "kyc_submission_worker"

func workerEnqueueActor() enqueueActor {
	return enqueueActor{actorType: audit.ActorSystem, extra: map[string]any{"platform_service": workerPlatformService}}
}

func (a enqueueActor) metadata(base map[string]any) map[string]any {
	for k, v := range a.extra {
		base[k] = v
	}
	return base
}

// insertCreateOutbox inserts the create row for v in the caller's tenant
// transaction. player_account_id is deliberately not supplied: the guard
// trigger forces it from the verification (ADR 0106 section 2.5).
func insertCreateOutbox(ctx context.Context, tx pgx.Tx, v Verification) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, document_ids, idempotency_key)
		 VALUES ($1, $2, 'create', $3, '{}'::uuid[], $4)
		 RETURNING id`,
		v.TenantID, v.ID, v.ProviderID, "kv:"+v.ID.String(),
	).Scan(&id)
	return id, err
}

func isLiveCreatePerPlayerConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == outboxLiveCreatePerPlayerIndex
}

// RequestVerification is phase A of a create (ADR 0095 section 38, ADR 0106
// section 2.10): in the caller's ONE tenant transaction it inserts the orphan
// verification row plus its audit record plus the outbox `create` row plus the
// enqueue audit. NO provider call happens here or anywhere on an HTTP path.
//
// At most one live (pending/claimed) create exists per player account, enforced
// by a partial unique index (not by check-then-insert). A repeated create
// therefore hits 23505 on that index inside a SAVEPOINT; the savepoint is
// rolled back (no second orphan, no second audit) and the player's EXISTING
// verification is returned with existing=true (HTTP 200, same body shape). The
// existing row is read filtered by the caller's own server-resolved
// player_account_id, never by anything client-supplied (security D2).
//
// The race where the winning create row leaves the live set between the 23505
// and the read: if it is now `sent` the sent create's verification is returned
// (it is the player's current verification); if it ended failed_terminal or
// cancelled the live index no longer blocks and the insert is retried once.
// A second miss is ErrVerificationCreateContention (retryable), never an
// empty 200 and never a 500.
func RequestVerification(ctx context.Context, tx pgx.Tx, params CreateVerificationParams, providerID string) (Verification, bool, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.PersonID == uuid.Nil {
		return Verification{}, false, fmt.Errorf("%w: tenant_id, brand_id, player_account_id, and person_id are all required", ErrInvalidTransition)
	}
	if providerID == "" {
		return Verification{}, false, fmt.Errorf("%w: no KYC provider selected", ErrProviderUnavailable)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && requestVerificationTestHook != nil {
			requestVerificationTestHook("before_retry", attempt)
		}
		v, outboxID, insErr := requestVerificationAttempt(ctx, tx, params, providerID)
		if insErr == nil {
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
				Action: auditActionSubmissionEnqueued, TargetType: "kyc_verification", TargetID: v.ID.String(),
				Outcome:  audit.OutcomeSuccess,
				Metadata: map[string]any{"outbox_id": outboxID.String(), "operation": string(OpCreate), "provider_id": providerID, "duplicate": false},
			}); err != nil {
				return Verification{}, false, fmt.Errorf("kyc: audit submission enqueued: %w", err)
			}
			return v, false, nil
		}
		if !isLiveCreatePerPlayerConflict(insErr) {
			return Verification{}, false, insErr
		}
		if requestVerificationTestHook != nil {
			requestVerificationTestHook("after_conflict", attempt)
		}
		ex, found, readErr := existingCreateForPlayer(ctx, tx, params.TenantID, params.PlayerAccountID)
		if readErr != nil {
			return Verification{}, false, readErr
		}
		if found {
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
				Action: auditActionSubmissionEnqueued, TargetType: "kyc_verification", TargetID: ex.ID.String(),
				Outcome:  audit.OutcomeSuccess,
				Metadata: map[string]any{"operation": string(OpCreate), "provider_id": ex.ProviderID, "duplicate": true},
			}); err != nil {
				return Verification{}, false, fmt.Errorf("kyc: audit submission enqueued (duplicate): %w", err)
			}
			return ex, true, nil
		}
		// The winner left the live set and is not `sent`: retry the insert once.
	}
	return Verification{}, false, ErrVerificationCreateContention
}

// requestVerificationAttempt runs one phase-A insert inside a savepoint so a
// 23505 on the per-player index rolls back the orphan row and its audit.
func requestVerificationAttempt(ctx context.Context, tx pgx.Tx, params CreateVerificationParams, providerID string) (Verification, uuid.UUID, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Verification{}, uuid.Nil, fmt.Errorf("kyc: open savepoint: %w", err)
	}
	v, err := insertOrphanVerification(ctx, sp, params, providerID)
	var outboxID uuid.UUID
	if err == nil {
		outboxID, err = insertCreateOutbox(ctx, sp, v)
		if err != nil && !isLiveCreatePerPlayerConflict(err) {
			err = fmt.Errorf("kyc: enqueue create: %w", err)
		}
	}
	if err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return Verification{}, uuid.Nil, fmt.Errorf("kyc: rollback savepoint after %v: %w", err, rbErr)
		}
		return Verification{}, uuid.Nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return Verification{}, uuid.Nil, fmt.Errorf("kyc: release savepoint: %w", err)
	}
	return v, outboxID, nil
}

// existingCreateForPlayer returns the verification of the caller's most recent
// create row that is live or sent, filtered by the caller's own
// player_account_id (server context) and tenant. found=false means no such row
// is visible (the winner moved to a terminal state).
func existingCreateForPlayer(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) (Verification, bool, error) {
	var verificationID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT verification_id FROM kyc_submission_outbox
		  WHERE tenant_id = $1 AND player_account_id = $2 AND operation = 'create'
		    AND state IN ('pending', 'claimed', 'sent')
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		tenantID, playerAccountID,
	).Scan(&verificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, false, nil
	}
	if err != nil {
		return Verification{}, false, fmt.Errorf("kyc: read existing create: %w", err)
	}
	v, err := GetVerificationByID(ctx, tx, verificationID)
	if err != nil {
		return Verification{}, false, err
	}
	if v.PlayerAccountID != playerAccountID || v.TenantID != tenantID {
		// Unreachable (the outbox row was filtered by the same ids) but never
		// assumed: never hand a player another player's verification.
		return Verification{}, false, ErrNotFound
	}
	return v, true, nil
}

// hasLiveCreate reports whether verificationID has a pending/claimed create row,
// read in the caller's transaction (security Q-R1 condition: an orphan accepts
// an upload only if its live create is read inside the SAME upload tx).
func hasLiveCreate(ctx context.Context, tx pgx.Tx, tenantID, verificationID uuid.UUID) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM kyc_submission_outbox
		                 WHERE tenant_id = $1 AND verification_id = $2 AND operation = 'create'
		                   AND state IN ('pending', 'claimed'))`,
		tenantID, verificationID,
	).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("kyc: read live create: %w", err)
	}
	return live, nil
}

// sortedDocumentIDs returns the ids of docs distinct and in ascending text
// order (the order the database trigger and submissionIdempotencyKey use).
func sortedDocumentIDs(docs []SubmittedDocument) []uuid.UUID {
	ids := make([]uuid.UUID, len(docs))
	for i, d := range docs {
		ids[i] = d.DocumentID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// enqueueSubmitRow inserts a submit row pinning docs with INSERT ... ON CONFLICT
// (tenant_id, idempotency_key) WHERE state IN ('pending','claimed','sent') DO
// NOTHING: a duplicate submit of the same content is a no-op, and a concurrent
// enqueue of the same set never aborts the caller's transaction with 23505
// (security F12). inserted=false means a live or sent row with the key exists.
func enqueueSubmitRow(ctx context.Context, tx pgx.Tx, v Verification, docs []SubmittedDocument, actor enqueueActor) (outboxID uuid.UUID, inserted bool, err error) {
	if len(docs) == 0 {
		return uuid.Nil, false, nil
	}
	key := submissionIdempotencyKey(v.ID, docs)
	err = tx.QueryRow(ctx,
		`INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, document_ids, idempotency_key)
		 VALUES ($1, $2, 'submit', $3, $4::uuid[], $5)
		 ON CONFLICT (tenant_id, idempotency_key) WHERE state IN ('pending', 'claimed', 'sent') DO NOTHING
		 RETURNING id`,
		v.TenantID, v.ID, v.ProviderID, sortedDocumentIDs(docs), key,
	).Scan(&outboxID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Duplicate: name the existing live/sent row for traceability.
		var existingID uuid.UUID
		if qErr := tx.QueryRow(ctx,
			`SELECT id FROM kyc_submission_outbox WHERE tenant_id = $1 AND idempotency_key = $2
			   AND state IN ('pending', 'claimed', 'sent') ORDER BY created_at DESC, id DESC LIMIT 1`,
			v.TenantID, key).Scan(&existingID); qErr != nil && !errors.Is(qErr, pgx.ErrNoRows) {
			return uuid.Nil, false, fmt.Errorf("kyc: read duplicate submit row: %w", qErr)
		}
		if aErr := audit.Record(ctx, tx, audit.Entry{
			TenantID: v.TenantID, ActorType: actor.actorType, ActorID: actor.actorID,
			Action: auditActionSubmissionEnqueued, TargetType: "kyc_verification", TargetID: v.ID.String(),
			Outcome: audit.OutcomeSuccess,
			Metadata: actor.metadata(map[string]any{
				"outbox_id": nilIfZero(existingID), "operation": string(OpSubmit), "provider_id": v.ProviderID,
				"document_count": len(docs), "duplicate": true,
			}),
		}); aErr != nil {
			return uuid.Nil, false, fmt.Errorf("kyc: audit submission enqueued (duplicate): %w", aErr)
		}
		return existingID, false, nil
	case err != nil:
		return uuid.Nil, false, fmt.Errorf("kyc: enqueue submit: %w", err)
	}
	if aErr := audit.Record(ctx, tx, audit.Entry{
		TenantID: v.TenantID, ActorType: actor.actorType, ActorID: actor.actorID,
		Action: auditActionSubmissionEnqueued, TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: actor.metadata(map[string]any{
			"outbox_id": outboxID.String(), "operation": string(OpSubmit), "provider_id": v.ProviderID,
			"document_count": len(docs), "duplicate": false,
		}),
	}); aErr != nil {
		return uuid.Nil, false, fmt.Errorf("kyc: audit submission enqueued: %w", aErr)
	}
	return outboxID, true, nil
}

func nilIfZero(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

// enqueueSubmitForCurrentSet is the upload's phase A (submit): inside the
// upload's own transaction, after the document row and its audit, gather the
// verification's CURRENT non-rejected document set and enqueue it. A terminal
// verification or an empty set enqueues nothing.
func enqueueSubmitForCurrentSet(ctx context.Context, tx pgx.Tx, verificationID uuid.UUID, actor enqueueActor) error {
	v, docs, ok, err := gatherSubmissionDocuments(ctx, tx, verificationID)
	if err != nil {
		return err
	}
	if !ok || len(docs) == 0 {
		return nil
	}
	_, _, err = enqueueSubmitRow(ctx, tx, v, docs, actor)
	return err
}
