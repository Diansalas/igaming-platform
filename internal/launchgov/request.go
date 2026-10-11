package launchgov

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// validateRequestContent checks every caller-supplied content field before any SQL: closed
// vocabularies, bounds, the PII lint, and which fields an action requires or forbids.
func validateRequestContent(in RequestInput) error {
	switch in.Action {
	case ActionActivate, ActionReactivate, ActionClose, ActionRatify:
	case ActionSuspend:
		return fmt.Errorf("%w: a suspension is filed through Suspend", ErrInvalidInput)
	default:
		return fmt.Errorf("%w: unknown action %q", ErrInvalidInput, in.Action)
	}
	if !reasonAllowed(requestReasons, in.Action, in.ReasonCode) {
		return fmt.Errorf("%w: reason code %q is not in the closed catalogue for %s", ErrInvalidInput, in.ReasonCode, in.Action)
	}
	if err := LintFreeText("conditions_note", in.ConditionsNote, maxConditionsNote); err != nil {
		return err
	}
	if err := otherDocumentedNeedsNote(in.ReasonCode, in.ConditionsNote); err != nil {
		return err
	}
	needsLicensing := in.Action == ActionActivate || in.Action == ActionReactivate || in.Action == ActionRatify
	if !needsLicensing {
		if in.Licensing != nil || in.ResponsibleOperatorName != "" || in.ResponsibleOperatorRegistration != "" || len(in.JurisdictionIDs) > 0 {
			return fmt.Errorf("%w: licensing, operator and jurisdiction records belong to activate, reactivate and ratify only", ErrInvalidInput)
		}
		return nil
	}
	if in.Licensing == nil {
		return fmt.Errorf("%w: a licensing record is required for %s", ErrInvalidInput, in.Action)
	}
	if err := trimmedNonEmpty("responsible_operator_name", in.ResponsibleOperatorName, maxOperatorName); err != nil {
		return err
	}
	if err := LintFreeText("responsible_operator_name", in.ResponsibleOperatorName, maxOperatorName); err != nil {
		return err
	}
	if err := LintRegistration(in.ResponsibleOperatorRegistration); err != nil {
		return err
	}
	if in.Licensing.DeterminationReference != "" {
		if err := trimmedNonEmpty("determination_reference", in.Licensing.DeterminationReference, maxDeterminationReference); err != nil {
			return err
		}
		if err := LintFreeText("determination_reference", in.Licensing.DeterminationReference, maxDeterminationReference); err != nil {
			return err
		}
	}
	if in.Licensing.DeterminationByRole != "" {
		if err := trimmedNonEmpty("determination_by_role", in.Licensing.DeterminationByRole, maxDeterminationByRole); err != nil {
			return err
		}
		if err := LintFreeText("determination_by_role", in.Licensing.DeterminationByRole, maxDeterminationByRole); err != nil {
			return err
		}
	}
	if n := len(in.JurisdictionIDs); n < 1 || n > maxJurisdictions {
		return fmt.Errorf("%w: 1..%d jurisdictions are required", ErrInvalidInput, maxJurisdictions)
	}
	seen := map[uuid.UUID]bool{}
	for _, j := range in.JurisdictionIDs {
		if j == uuid.Nil || seen[j] {
			return fmt.Errorf("%w: jurisdiction ids must be distinct and non-nil", ErrInvalidInput)
		}
		seen[j] = true
	}
	return nil
}

// latestIsLegacyBaseline returns ErrNotLegacyBaseline unless the subject's latest status
// transition is the legacy baseline (the same ordering and subject match as migration 0128).
func latestIsLegacyBaseline(ctx context.Context, tx pgx.Tx, sub Subject) error {
	subjectID := sub.TenantID
	if sub.BrandID != nil {
		subjectID = *sub.BrandID
	}
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM launch_status_transitions
		WHERE subject_kind = $1 AND COALESCE(brand_id, tenant_id) = $2 ORDER BY created_at DESC, id DESC LIMIT 1`,
		sub.Kind(), subjectID).Scan(&kind)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if kind != "legacy_baseline" {
		return ErrNotLegacyBaseline
	}
	return nil
}

// otherDocumentedNeedsNote: the catch-all reason code "other_documented" is only meaningful with
// the documentation it names, so it requires a non-empty conditions_note (security L5).
func otherDocumentedNeedsNote(reason, note string) error {
	if reason == ReasonOtherDocumented && strings.TrimSpace(note) == "" {
		return fmt.Errorf("%w: reason code %s requires a non-empty conditions_note", ErrInvalidInput, ReasonOtherDocumented)
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// digestFor builds the request digest the database recomputes (migration 0130).
func digestFor(sub Subject, in RequestInput, snapHash string, acks []string, operator string) actorproof.LaunchRequest {
	d := actorproof.LaunchRequest{
		TenantID: sub.TenantID, SubjectKind: sub.Kind(), BrandID: sub.BrandID, Action: in.Action,
		ConditionsNote: in.ConditionsNote, ReadinessSnapshotHash: snapHash, AcknowledgedCheckCodes: acks, ReasonCode: in.ReasonCode,
	}
	if in.Licensing != nil {
		m, st := in.Licensing.Model, in.Licensing.Status
		d.LicensingModel, d.LicensingStatus, d.LicenceID = &m, &st, in.Licensing.LicenceID
		d.DeterminationReference = nilIfEmpty(in.Licensing.DeterminationReference)
		d.DeterminationByRole = nilIfEmpty(in.Licensing.DeterminationByRole)
	}
	if operator != "" {
		d.ResponsibleOperatorName = &operator
	}
	d.ResponsibleOperatorRegistration = in.ResponsibleOperatorRegistration
	d.JurisdictionIDs = in.JurisdictionIDs
	return d
}

// ackErr applies the H-5 acknowledgement rule: the acknowledged codes must equal EXACTLY the set
// of checks that are not pass. Missing = an unacknowledged non-passing check; extra = a blanket
// acknowledgement of a passing or unknown check, which would blind the approvers.
func ackErr(r Readiness, acks []string) error {
	want := map[string]bool{}
	for _, c := range r.NonPassing() {
		want[c] = true
	}
	have := map[string]bool{}
	for _, a := range acks {
		have[a] = true
	}
	var codes []string
	for c := range want {
		if !have[c] {
			codes = append(codes, c)
		}
	}
	if len(codes) > 0 {
		sortStrings(codes)
		return &PreconditionError{Base: ErrUnacknowledged, Codes: codes}
	}
	for a := range have {
		if !want[a] {
			codes = append(codes, "extra:"+a)
		}
	}
	if len(codes) > 0 {
		sortStrings(codes)
		return &PreconditionError{Base: ErrUnacknowledged, Codes: codes}
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// hardErr turns hard-precondition failure codes into the service errors.
func hardErr(fails []string) error {
	if len(fails) == 0 {
		return nil
	}
	for _, f := range fails {
		switch f {
		case H1SubjectClosed:
			return ErrSubjectClosed
		case H1IllegalTransition:
			return ErrIllegalTransition
		case H6WithdrawalsHolding:
			return &PreconditionError{Base: ErrWithdrawalsHolding, Codes: fails}
		}
	}
	return &PreconditionError{Base: ErrPreconditionFailed, Codes: fails}
}

// Request files a launch authorisation request (activate / reactivate / close / ratify). The
// server computes the readiness, binds its hash, applies the hard preconditions and the
// acknowledgement rule, and inserts the request with the requester's signed proof. It never
// executes anything; a suspension is Suspend.
func (s *Service) Request(ctx context.Context, a Actor, in RequestInput) (*RequestResult, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRequest, in.Subject, true); err != nil {
		return nil, err
	}
	if err := validateRequestContent(in); err != nil {
		return nil, err
	}
	as, err := s.assess(ctx, EvalInput{Subject: in.Subject, Action: in.Action, Licensing: in.Licensing, JurisdictionIDs: in.JurisdictionIDs})
	if err != nil {
		return nil, err
	}
	if err := hardErr(as.Failures); err != nil {
		return nil, s.denied(ctx, a, "request", in.Subject, in.Action, err)
	}
	if err := ackErr(as.Readiness, in.AcknowledgedCheckCodes); err != nil {
		return nil, s.denied(ctx, a, "request", in.Subject, in.Action, err)
	}
	snap, err := as.Readiness.SnapshotJSON()
	if err != nil {
		return nil, err
	}
	snapHash := as.Readiness.Hash()
	acks := append([]string{}, in.AcknowledgedCheckCodes...)
	sortStrings(acks)
	digest := digestFor(in.Subject, in, snapHash, acks, in.ResponsibleOperatorName).Digest()

	var out Request
	err = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		// Serialise with a concurrent suspension of the same subject: the subject row is locked
		// FOR SHARE (conflicts with the executor's FOR NO KEY UPDATE, not with gameplay's FK
		// checks), so a suspension either completes before this request is inserted (the request
		// guard then sees the new status) or waits for this request to commit and supersedes it.
		if err := s.lockSubjectShared(ctx, tx, a, in.Subject); err != nil {
			return err
		}
		// Go mirror of the request guard's ratify rule (migration 0128): only a subject whose LATEST
		// transition is the legacy baseline can be ratified. The decision tables are readable only
		// in the actor's principal session (not in the readiness evaluation's tenant read).
		if in.Action == ActionRatify {
			if err := latestIsLegacyBaseline(ctx, tx, in.Subject); err != nil {
				return err
			}
		}
		// Stale pending requests of the subject become 'expired' (audited once) before the insert.
		if _, err := s.expireStale(ctx, tx, a, in.Subject.TenantID, &in.Subject, nil, "request"); err != nil {
			return err
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLaunchRequest, actorproof.TargetNew, digest); err != nil {
			return err
		}
		var lm, ls, lref, lrole *string
		var lid *uuid.UUID
		if in.Licensing != nil {
			m, st := in.Licensing.Model, in.Licensing.Status
			lm, ls, lid = &m, &st, in.Licensing.LicenceID
			lref, lrole = nilIfEmpty(in.Licensing.DeterminationReference), nilIfEmpty(in.Licensing.DeterminationByRole)
		}
		juris := in.JurisdictionIDs
		if juris == nil {
			juris = []uuid.UUID{}
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO launch_authorisation_requests
			    (tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash,
			     licensing_model, licensing_status, licence_id, determination_reference, determination_by_role,
			     responsible_operator_name, responsible_operator_registration, jurisdiction_ids, conditions_note, acknowledged_check_codes)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			RETURNING `+requestColumns,
			in.Subject.TenantID, in.Subject.Kind(), in.Subject.BrandID, in.Action, in.ReasonCode, string(snap), snapHash,
			lm, ls, lid, lref, lrole, nilIfEmpty(in.ResponsibleOperatorName), in.ResponsibleOperatorRegistration, juris, in.ConditionsNote, acks)
		if err := scanRequest(row, &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "requested", out, map[string]any{"readiness_snapshot_hash": snapHash, "acknowledged_check_codes": acks})
	})
	if err != nil {
		return nil, s.mapRequestErr(ctx, a, in, err)
	}
	return &RequestResult{Request: out, Readiness: as.Readiness}, nil
}

// mapRequestErr maps the database refusals of a request INSERT and audits the denial once.
func (s *Service) mapRequestErr(ctx context.Context, a Actor, in RequestInput, err error) error {
	if pe := pgErr(err); pe != nil {
		switch {
		case pe.Code == "23505" && strings.Contains(pe.ConstraintName, "launch_requests_one_pending"):
			return s.denied(ctx, a, "request", in.Subject, in.Action, ErrRequestPending)
		case pe.Code == "LA014":
			return s.denied(ctx, a, "request", in.Subject, in.Action, s.priorReject(pe))
		}
	}
	err = classify(err)
	if errors.Is(err, ErrRetryableLockTimeout) || errors.Is(err, ErrProofUnavailable) || errors.Is(err, ErrIsolationLevel) {
		return err
	}
	return s.denied(ctx, a, "request", in.Subject, in.Action, err)
}

// priorReject names the earlier rejected request an identical request collided with. The
// database (migration 0130 LA014) names the request whose payload_hash MATCHED in the error
// DETAIL ("prior_request_id=<uuid> decided_at=<RFC 3339>"), so nothing is guessed and no second
// query runs. If the detail is unreadable the refusal stands with the worst-case retry time
// (now + TTL), never the zero time.
func (s *Service) priorReject(pe *pgconn.PgError) error {
	e := &PriorRejectError{}
	for _, part := range strings.Fields(pe.Detail) {
		switch {
		case strings.HasPrefix(part, "prior_request_id="):
			if id, err := uuid.Parse(strings.TrimPrefix(part, "prior_request_id=")); err == nil {
				e.RequestID = id
			}
		case strings.HasPrefix(part, "decided_at="):
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(part, "decided_at=")); err == nil {
				e.DecidedAt = t
			}
		}
	}
	if e.DecidedAt.IsZero() {
		e.DecidedAt = s.now()
	}
	e.RetryAfter = e.DecidedAt.Add(RequestTTL)
	return e
}

// Cancel withdraws a pending request. Only its requester may (the database checks the proven
// actor and scope); the proof binds the request id and its payload hash.
func (s *Service) Cancel(ctx context.Context, a Actor, sub Subject, requestID uuid.UUID, reasonCode string) (*Request, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRequest, sub, true); err != nil {
		return nil, err
	}
	if !reasonAllowed(requestReasons, "cancel", reasonCode) {
		return nil, fmt.Errorf("%w: reason code %q is not in the closed catalogue for cancel", ErrInvalidInput, reasonCode)
	}
	var out Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockRequest(ctx, tx, sub.TenantID, requestID)
		if err != nil {
			return err
		}
		if cur.Status != ReqPending || !s.now().Before(cur.ExpiresAt) {
			return ErrNotPending
		}
		if cur.RequestedBy != a.StaffID || cur.RequestedByScope != a.Scope {
			return fmt.Errorf("%w: only the requester may cancel", ErrForbidden)
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLaunchCancel, cur.ID.String(), cur.PayloadHash); err != nil {
			return err
		}
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1 RETURNING `+requestColumns, cur.ID), &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "cancelled", out, map[string]any{"reason_code": reasonCode})
	})
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrNotPending) {
			// The request may have expired by time: materialise (and audit once) the expiry.
			s.sweepBestEffort(ctx, a, sub.TenantID, &requestID)
		}
		if errors.Is(err, ErrRetryableLockTimeout) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotPending) || errors.Is(err, ErrForbidden) {
			return nil, err
		}
		return nil, s.denied(ctx, a, "cancel", sub, "", err, withRequest(requestID, reasonCode))
	}
	return &out, nil
}

// lockRequest reads the request FOR UPDATE, bound to the route tenant: a request of another
// tenant is ErrNotFound (404), never a different error.
func lockRequest(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Request, error) {
	var r Request
	err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM launch_authorisation_requests WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// GetRequest reads one request of the tenant (read-only; foreign ids are ErrNotFound).
func (s *Service) GetRequest(ctx context.Context, a Actor, tenantID, requestID uuid.UUID) (*Request, []Approval, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false); err != nil {
		return nil, nil, err
	}
	var r Request
	var apps []Approval
	s.sweepBestEffort(ctx, a, tenantID, &requestID)
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM launch_authorisation_requests WHERE id = $1 AND tenant_id = $2`, requestID, tenantID), &r)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.PriorRejects, err = loadPriorRejects(ctx, tx, s.now(), r); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM launch_authorisation_approvals WHERE request_id = $1 ORDER BY decided_at, id`, requestID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ap Approval
			if err := scanApproval(rows, &ap); err != nil {
				return err
			}
			apps = append(apps, ap)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return &r, apps, nil
}
