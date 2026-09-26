package providercred

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// Lifecycle (ADR 0093 §3; security review §1 and §6): enabling a key takes
// two people - FileRequest (platform scope) -> DecideRequest by a
// different Person (platform scope) -> ApplyRequest (principal recheck in
// platform scope, then the tenant-scoped predecessor transition + handle
// insert that the DB consume trigger checks). Disabling takes one person -
// TransitionHandle (verify_only, shorten, revoke), the ONLY UPDATE path.
// Every write records an audit row in the same transaction; a rejected
// attempt records a failure row carrying only the generic class. The
// secret value and the confirmation value are never recorded anywhere.

// Reason codes (review §1.5 and the request reason enum).
const (
	RequestReasonInitialRegistration   = "initial_registration"
	RequestReasonScheduledRotation     = "scheduled_rotation"
	RequestReasonCompromiseReplacement = "compromise_replacement"
	RequestReasonVendorMigration       = "vendor_migration"

	TransitionReasonRotation = "rotation"

	ShortenReasonOverlapShortened    = "overlap_shortened"
	ShortenReasonSuspectedCompromise = "suspected_compromise"
	ShortenReasonVendorInstruction   = "vendor_instruction"

	RevokeReasonRotationComplete    = "rotation_complete"
	RevokeReasonSuspectedCompromise = "suspected_compromise"
	RevokeReasonConfirmedCompromise = "confirmed_compromise"
	RevokeReasonVendorOffboarded    = "vendor_offboarded"
	RevokeReasonMisregistration     = "misregistration"
	RevokeReasonTenantRequest       = "tenant_request"
)

// Predecessor dispositions.
const (
	DispositionNone       = "none"
	DispositionVerifyOnly = "verify_only"
	DispositionRevoked    = "revoked"
)

// Transition actions.
const (
	ActionVerifyOnly = "verify_only"
	ActionShorten    = "shorten"
	ActionRevoke     = "revoke"
)

// Audit actions (review §6).
const (
	AuditRequestFiled            = "provider_credential.request_filed"
	AuditRequestDecided          = "provider_credential.request_decided"
	AuditActivated               = "provider_credential.activated"
	AuditPredecessorTransitioned = "provider_credential.predecessor_transitioned"
	AuditTransitioned            = "provider_credential.transitioned"

	auditTargetHandle  = "provider_credential_handle"
	auditTargetRequest = "provider_credential_request"
)

var (
	requestReasons = set(RequestReasonInitialRegistration, RequestReasonScheduledRotation,
		RequestReasonCompromiseReplacement, RequestReasonVendorMigration)
	shortenReasons = set(ShortenReasonOverlapShortened, ShortenReasonSuspectedCompromise, ShortenReasonVendorInstruction)
	revokeReasons  = set(RevokeReasonRotationComplete, RevokeReasonSuspectedCompromise, RevokeReasonConfirmedCompromise,
		RevokeReasonVendorOffboarded, RevokeReasonMisregistration, RevokeReasonTenantRequest)
	dispositions = set(DispositionNone, DispositionVerifyOnly, DispositionRevoked)
	purposes     = set(PurposeWebhookVerify, PurposeOutboundAPI)

	keyIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	contentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rejectReasonShape  = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// ValidRequestReason etc. expose the closed enums to the HTTP layer.
func ValidRequestReason(r string) bool { return requestReasons[r] }

// ValidTransitionReason reports whether reason is valid for action.
func ValidTransitionReason(action, reason string) bool {
	switch action {
	case ActionVerifyOnly:
		return reason == TransitionReasonRotation
	case ActionShorten:
		return shortenReasons[reason]
	case ActionRevoke:
		return revokeReasons[reason]
	default:
		return false
	}
}

// Handle is a provider_credential_handles row (no secret, by construction).
type Handle struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Domain              string
	ProviderID          string
	Purpose             string
	KeyID               string
	SecretRef           string
	Fingerprint         string
	VendorAccountID     *string
	Status              string
	StatusChangedAt     time.Time
	NotBefore           time.Time
	NotAfter            *time.Time
	ActivationRequestID uuid.UUID
	CreatedAt           time.Time
	CreatedBy           uuid.UUID
	RevokedAt           *time.Time
	RevokedBy           *uuid.UUID
	RevokeReason        *string
}

// Request is a provider_credential_change_requests row plus its decisions.
type Request struct {
	ID                     uuid.UUID
	TargetTenantID         uuid.UUID
	Domain                 string
	ProviderID             string
	Purpose                string
	KeyID                  string
	SecretRef              string
	Fingerprint            string
	VendorAccountID        *string
	NotBefore              time.Time
	NotAfter               *time.Time
	PredecessorHandleID    *uuid.UUID
	PredecessorDisposition string
	PredecessorNotAfter    *time.Time
	ContentHash            string
	ReasonCode             string
	RequestedBy            uuid.UUID
	RequestedAt            time.Time
	State                  string
	AppliedAt              *time.Time
	AppliedBy              *uuid.UUID
	AppliedHandleID        *uuid.UUID
	Approvals              []Approval
}

// Approval is one immutable decision.
type Approval struct {
	ID                  uuid.UUID
	RequestID           uuid.UUID
	ApproverPrincipalID uuid.UUID
	Decision            string
	ContentHash         string
	ReasonCode          *string
	DecidedAt           time.Time
}

// AuditContext is the request metadata every audit row carries.
type AuditContext struct {
	ActorID   uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
}

// TxRunner is the pool surface the lifecycle needs (*db.Pool satisfies it).
type TxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error
	WithPlatformAdmin(ctx context.Context, principalID uuid.UUID, fn db.TxFunc) error
}

// FileRequestParams is a registration. The target tenant always comes from
// the authenticated route, never from a request body. Confirmation is the
// operator's transient "sha256:<hex>" value.
type FileRequestParams struct {
	TargetTenantID         uuid.UUID
	Domain                 string
	ProviderID             string
	Purpose                string
	KeyID                  string
	SecretRef              string
	Confirmation           string
	VendorAccountID        *string
	NotBefore              time.Time
	NotAfter               *time.Time
	PredecessorHandleID    *uuid.UUID
	PredecessorDisposition string
	PredecessorNotAfter    *time.Time
	ReasonCode             string
	RequestedBy            uuid.UUID
}

// validateFileSyntax is the 400 class only (review §6: malformed, missing
// field, value outside an enum, key_id charset).
func validateFileSyntax(p FileRequestParams) error {
	switch {
	case p.TargetTenantID == uuid.Nil, p.RequestedBy == uuid.Nil:
		return newErr(KindInvalid, "missing_field")
	case !validDomain(p.Domain):
		return newErr(KindInvalid, "domain")
	case !webhookauth.ValidProviderID(p.ProviderID):
		return newErr(KindInvalid, "provider_id")
	case !purposes[p.Purpose]:
		return newErr(KindInvalid, "purpose")
	case !keyIDPattern.MatchString(p.KeyID):
		return newErr(KindInvalid, "key_id")
	case p.SecretRef == "":
		return newErr(KindInvalid, "secret_ref")
	case !ValidConfirmationShape(p.Confirmation):
		return newErr(KindInvalid, "confirmation")
	case p.NotBefore.IsZero():
		return newErr(KindInvalid, "not_before")
	case !requestReasons[p.ReasonCode]:
		return newErr(KindInvalid, "reason_code")
	case !dispositions[p.PredecessorDisposition]:
		return newErr(KindInvalid, "predecessor_disposition")
	}
	return nil
}

func rejected(class string) error { return newErr(KindRegistrationRejected, class) }

// storeClass names a secretstore failure for the log.
func storeClass(err error) string { return "store_" + secretstore.ClassOf(err).String() }

// FileRequest validates and files a registration (review §1.2, §3, §4.1
// point 2). It fetches the secret by ref OUTSIDE any transaction, compares
// the operator's confirmation in constant time, computes the fp1:
// fingerprint itself, then inserts the request in platform scope (the DB
// forces requested_at/state/content_hash and runs the hardened 0089
// principal check) with its audit row. Every semantic failure is ONE
// KindRegistrationRejected (the specific class goes to the log only).
func (s *Subsystem) FileRequest(ctx context.Context, pool TxRunner, p FileRequestParams, ac AuditContext) (Request, error) {
	if s == nil {
		return Request{}, errNilSubsystem
	}
	req, err := s.fileRequest(ctx, pool, p, ac)
	if err != nil && KindOf(err) != KindInvalid {
		s.recordPlatformFailure(ctx, pool, ac, AuditRequestFiled, auditTargetRequest, "", "registration_rejected", p.TargetTenantID)
	}
	return req, err
}

func (s *Subsystem) fileRequest(ctx context.Context, pool TxRunner, p FileRequestParams, ac AuditContext) (Request, error) {
	if err := validateFileSyntax(p); err != nil {
		return Request{}, err
	}
	ref, err := secretstore.ParseRef(p.SecretRef)
	if err != nil {
		return Request{}, rejected("ref_invalid")
	}
	if _, routed := s.router.Backend(ref.Scheme()); !routed {
		return Request{}, rejected("backend_not_allowed")
	}
	if s.router.Validated() {
		if err := s.cfg.ValidateSecretBackendScheme(ref.Scheme()); err != nil {
			return Request{}, rejected("backend_not_allowed")
		}
	}
	if !ref.InNamespace(p.TargetTenantID, p.Domain, p.ProviderID) {
		return Request{}, rejected("namespace_mismatch")
	}
	if p.NotAfter != nil && !p.NotAfter.After(p.NotBefore) {
		return Request{}, rejected("window_invalid")
	}
	switch p.PredecessorDisposition {
	case DispositionNone:
		if p.PredecessorHandleID != nil || p.PredecessorNotAfter != nil {
			return Request{}, rejected("predecessor_inconsistent")
		}
	case DispositionVerifyOnly:
		if p.PredecessorHandleID == nil || p.PredecessorNotAfter == nil || p.Purpose != PurposeWebhookVerify {
			return Request{}, rejected("predecessor_inconsistent")
		}
	case DispositionRevoked:
		if p.PredecessorHandleID == nil || p.PredecessorNotAfter != nil {
			return Request{}, rejected("predecessor_inconsistent")
		}
	}

	secret, err := s.router.GetDirect(ctx, ref)
	if err != nil {
		return Request{}, rejected(storeClass(err))
	}
	raw := secret.Bytes()
	defer zero(raw)
	if len(raw) < webhookauth.MinSecretBytes {
		return Request{}, rejected("secret_too_short")
	}
	if !ConfirmationMatches(raw, p.Confirmation) {
		return Request{}, rejected("confirmation_mismatch")
	}
	fingerprint := s.key.Fingerprint(raw)

	var out Request
	err = pool.WithPlatformAdmin(ctx, p.RequestedBy, func(ctx context.Context, tx pgx.Tx) error {
		var dupFP, dupKey bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM provider_credential_change_requests
			         WHERE state = 'applied' AND domain = $1 AND provider_id = $2 AND purpose = $3 AND fingerprint = $4),
			EXISTS (SELECT 1 FROM provider_credential_change_requests
			         WHERE state = 'applied' AND target_tenant_id = $5 AND domain = $1 AND provider_id = $2 AND purpose = $3 AND key_id = $6)`,
			p.Domain, p.ProviderID, p.Purpose, fingerprint, p.TargetTenantID, p.KeyID).Scan(&dupFP, &dupKey); err != nil {
			return err
		}
		if dupFP {
			return rejected("duplicate_fingerprint")
		}
		if dupKey {
			return rejected("duplicate_key_id")
		}
		row := tx.QueryRow(ctx, `INSERT INTO provider_credential_change_requests
			(target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, vendor_account_id,
			 not_before, not_after, predecessor_handle_id, predecessor_disposition, predecessor_not_after,
			 reason_code, requested_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			RETURNING `+requestColumns,
			p.TargetTenantID, p.Domain, p.ProviderID, p.Purpose, p.KeyID, ref.String(), fingerprint, p.VendorAccountID,
			p.NotBefore, p.NotAfter, p.PredecessorHandleID, p.PredecessorDisposition, p.PredecessorNotAfter,
			p.ReasonCode, p.RequestedBy)
		r, err := scanRequest(row)
		if err != nil {
			return err
		}
		out = r
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: ac.ActorID,
			Action: AuditRequestFiled, TargetType: auditTargetRequest, TargetID: r.ID.String(),
			Outcome: audit.OutcomeSuccess, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
			Metadata: requestMetadata(r),
		})
	})
	if err != nil {
		return Request{}, classify(err, KindRegistrationRejected)
	}
	return out, nil
}

// DecideParams is an approve/reject decision on one request.
type DecideParams struct {
	TargetTenantID uuid.UUID
	RequestID      uuid.UUID
	Approver       uuid.UUID
	Decision       string // "approve" | "reject"
	ContentHash    string // the hash the approver saw, echoed back
	ReasonCode     string // required for reject
}

// DecideRequest records a decision in platform scope. The DB enforces the
// hardened 0089 rules (distinct principal, distinct Person, active,
// platform-scoped, person-linked approver), the pending state and the
// content hash; holding the approve permission never bypasses them.
func (s *Subsystem) DecideRequest(ctx context.Context, pool TxRunner, p DecideParams, ac AuditContext) (Approval, error) {
	if s == nil {
		return Approval{}, errNilSubsystem
	}
	switch {
	case p.TargetTenantID == uuid.Nil, p.RequestID == uuid.Nil, p.Approver == uuid.Nil:
		return Approval{}, newErr(KindInvalid, "missing_field")
	case p.Decision != "approve" && p.Decision != "reject":
		return Approval{}, newErr(KindInvalid, "decision")
	case !contentHashPattern.MatchString(p.ContentHash):
		return Approval{}, newErr(KindInvalid, "content_hash")
	case p.Decision == "reject" && !rejectReasonShape.MatchString(p.ReasonCode):
		return Approval{}, newErr(KindInvalid, "reason_code")
	case p.Decision == "approve" && p.ReasonCode != "" && !rejectReasonShape.MatchString(p.ReasonCode):
		return Approval{}, newErr(KindInvalid, "reason_code")
	}
	var reason *string
	if p.ReasonCode != "" {
		reason = &p.ReasonCode
	}
	var out Approval
	err := pool.WithPlatformAdmin(ctx, p.Approver, func(ctx context.Context, tx pgx.Tx) error {
		r, err := getRequest(ctx, tx, p.TargetTenantID, p.RequestID)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO provider_credential_change_approvals
			(request_id, approver_principal_id, decision, content_hash, reason_code)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, request_id, approver_principal_id, decision, content_hash, reason_code, decided_at`,
			p.RequestID, p.Approver, p.Decision, p.ContentHash, reason)
		if err := row.Scan(&out.ID, &out.RequestID, &out.ApproverPrincipalID, &out.Decision, &out.ContentHash, &out.ReasonCode, &out.DecidedAt); err != nil {
			return err
		}
		md := requestMetadata(r)
		md["decision"] = out.Decision
		md["approver_principal_id"] = out.ApproverPrincipalID.String()
		md["approval_id"] = out.ID.String()
		if out.ReasonCode != nil {
			md["reason_code"] = *out.ReasonCode
		}
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: ac.ActorID,
			Action: AuditRequestDecided, TargetType: auditTargetRequest, TargetID: r.ID.String(),
			Outcome: audit.OutcomeSuccess, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
			Metadata: md,
		})
	})
	if err != nil {
		err = classify(err, KindApprovalRejected)
		if k := KindOf(err); k == KindApprovalRejected {
			s.recordPlatformFailure(ctx, pool, ac, AuditRequestDecided, auditTargetRequest, p.RequestID.String(), "approval_rejected", p.TargetTenantID)
		}
		return Approval{}, err
	}
	return out, nil
}

// ApplyParams applies one approved request.
type ApplyParams struct {
	TargetTenantID uuid.UUID
	RequestID      uuid.UUID
	Applier        uuid.UUID
}

// ApplyRequest activates an approved request (review §1.1, §1.4):
//
//  1. one WithPlatformAdmin transaction re-resolves the applier, the
//     requester and every approving principal (active, platform-scoped,
//     person-linked) - the binding defence in depth the DB consume step
//     does not re-check;
//  2. the secret is re-fetched fresh and its fp1: fingerprint re-checked
//     against the request, outside any transaction;
//  3. one WithTenant(target) transaction: the predecessor transition (reason
//     rotation / rotation_complete), the plain handle INSERT carrying
//     activation_request_id (the consume trigger checks the approval), and
//     one audit row per changed handle row.
func (s *Subsystem) ApplyRequest(ctx context.Context, pool TxRunner, p ApplyParams, ac AuditContext) (Handle, error) {
	if s == nil {
		return Handle{}, errNilSubsystem
	}
	h, err := s.applyRequest(ctx, pool, p, ac)
	if err != nil {
		if k := KindOf(err); k == KindActivationRejected {
			s.recordTenantFailure(ctx, pool, p.TargetTenantID, ac, AuditActivated, auditTargetRequest, p.RequestID.String(), "activation_rejected")
		}
	}
	return h, err
}

func activationRejected(class string) error { return newErr(KindActivationRejected, class) }

func (s *Subsystem) applyRequest(ctx context.Context, pool TxRunner, p ApplyParams, ac AuditContext) (Handle, error) {
	if p.TargetTenantID == uuid.Nil || p.RequestID == uuid.Nil || p.Applier == uuid.Nil {
		return Handle{}, newErr(KindInvalid, "missing_field")
	}
	var r Request
	err := pool.WithPlatformAdmin(ctx, p.Applier, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = getRequest(ctx, tx, p.TargetTenantID, p.RequestID)
		if err != nil {
			return err
		}
		if r.State != "pending" {
			return activationRejected("not_pending")
		}
		principals := []uuid.UUID{p.Applier, r.RequestedBy}
		for _, a := range r.Approvals {
			if a.Decision == "approve" {
				principals = append(principals, a.ApproverPrincipalID)
			}
		}
		for _, id := range principals {
			su, err := identity.GetStaffUserByID(ctx, tx, id)
			if errors.Is(err, identity.ErrNotFound) {
				return activationRejected("principal_recheck")
			}
			if err != nil {
				return err
			}
			if su.Status != "active" || su.TenantID != uuid.Nil || su.PersonID == nil {
				return activationRejected("principal_recheck")
			}
		}
		return nil
	})
	if err != nil {
		return Handle{}, classify(err, KindActivationRejected)
	}

	ref, err := secretstore.ParseRef(r.SecretRef)
	if err != nil {
		return Handle{}, activationRejected("ref_invalid")
	}
	secret, err := s.router.GetDirect(ctx, ref)
	if err != nil {
		return Handle{}, activationRejected(storeClass(err))
	}
	raw := secret.Bytes()
	fpOK := s.key.Fingerprint(raw) == r.Fingerprint && len(raw) >= webhookauth.MinSecretBytes
	zero(raw)
	if !fpOK {
		return Handle{}, activationRejected("fingerprint_recheck")
	}

	var out Handle
	err = pool.WithTenant(ctx, p.TargetTenantID, func(ctx context.Context, tx pgx.Tx) error {
		if r.PredecessorHandleID != nil {
			switch r.PredecessorDisposition {
			case DispositionVerifyOnly:
				if _, err := transitionHandle(ctx, tx, *r.PredecessorHandleID, ActionVerifyOnly, r.PredecessorNotAfter,
					TransitionReasonRotation, p.Applier, ac, AuditPredecessorTransitioned, &r.ID); err != nil {
					return err
				}
			case DispositionRevoked:
				pred, err := getHandleForUpdate(ctx, tx, *r.PredecessorHandleID)
				if err != nil {
					return err
				}
				if pred.Status != "revoked" {
					if _, err := transitionHandle(ctx, tx, pred.ID, ActionRevoke, nil,
						RevokeReasonRotationComplete, p.Applier, ac, AuditPredecessorTransitioned, &r.ID); err != nil {
						return err
					}
				}
			}
		}
		row := tx.QueryRow(ctx, `INSERT INTO provider_credential_handles
			(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, vendor_account_id,
			 status, not_before, not_after, activation_request_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, $10, $11, $12)
			RETURNING `+handleColumns,
			r.TargetTenantID, r.Domain, r.ProviderID, r.Purpose, r.KeyID, r.SecretRef, r.Fingerprint, r.VendorAccountID,
			r.NotBefore, r.NotAfter, r.ID, p.Applier)
		h, err := scanHandle(row)
		if err != nil {
			return err
		}
		out = h
		md := handleMetadata(h)
		md["change_request_id"] = r.ID.String()
		md["content_hash"] = r.ContentHash
		md["status_before"] = nil
		md["status_after"] = h.Status
		md["reason_code"] = r.ReasonCode
		md["requester_principal_id"] = r.RequestedBy.String()
		md["approver_principal_ids"] = approverIDs(r.Approvals)
		return audit.Record(ctx, tx, audit.Entry{
			TenantID:  p.TargetTenantID,
			ActorType: audit.ActorStaff, ActorID: ac.ActorID,
			Action: AuditActivated, TargetType: auditTargetHandle, TargetID: h.ID.String(),
			Outcome: audit.OutcomeSuccess, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
			Metadata: md,
		})
	})
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) && pe.Kind == KindTransitionRejected {
			return Handle{}, activationRejected("predecessor_" + pe.Class)
		}
		if errors.As(err, &pe) && pe.Kind == KindNotFound {
			return Handle{}, activationRejected("predecessor_not_found")
		}
		return Handle{}, classify(err, KindActivationRejected)
	}
	return out, nil
}

// TransitionHandle is the single-actor transition (review §1.5) and the
// ONLY UPDATE path for a handle: verify_only (reason rotation; inbound
// only; not_after required and at most 7 days out), shorten (not_after may
// only shrink, never into the past) or revoke (a revoke reason). It runs in
// the caller's tenant-scoped transaction and writes the audit row in it.
// It needs no fingerprint key, so revocation works even when the real
// subsystem is not constructed.
func TransitionHandle(ctx context.Context, tx pgx.Tx, handleID uuid.UUID, action string, notAfter *time.Time, reason string, actor uuid.UUID, ac AuditContext) (Handle, error) {
	return transitionHandle(ctx, tx, handleID, action, notAfter, reason, actor, ac, AuditTransitioned, nil)
}

func transitionHandle(ctx context.Context, tx pgx.Tx, handleID uuid.UUID, action string, notAfter *time.Time, reason string,
	actor uuid.UUID, ac AuditContext, auditAction string, changeRequestID *uuid.UUID) (Handle, error) {
	switch {
	case handleID == uuid.Nil || actor == uuid.Nil:
		return Handle{}, newErr(KindInvalid, "missing_field")
	case action != ActionVerifyOnly && action != ActionShorten && action != ActionRevoke:
		return Handle{}, newErr(KindInvalid, "action")
	case !ValidTransitionReason(action, reason):
		return Handle{}, newErr(KindInvalid, "reason_code")
	case (action == ActionVerifyOnly || action == ActionShorten) && notAfter == nil:
		return Handle{}, newErr(KindInvalid, "not_after")
	case action == ActionRevoke && notAfter != nil:
		return Handle{}, newErr(KindInvalid, "not_after")
	}
	before, err := getHandleForUpdate(ctx, tx, handleID)
	if err != nil {
		return Handle{}, err
	}
	var row pgx.Row
	switch action {
	case ActionVerifyOnly:
		row = tx.QueryRow(ctx, `UPDATE provider_credential_handles SET status = 'verify_only', not_after = $2
			WHERE id = $1 RETURNING `+handleColumns, handleID, notAfter)
	case ActionShorten:
		row = tx.QueryRow(ctx, `UPDATE provider_credential_handles SET not_after = $2
			WHERE id = $1 RETURNING `+handleColumns, handleID, notAfter)
	case ActionRevoke:
		row = tx.QueryRow(ctx, `UPDATE provider_credential_handles
			SET status = 'revoked', revoked_by = $2, revoke_reason = $3, revoked_at = now()
			WHERE id = $1 RETURNING `+handleColumns, handleID, actor, reason)
	}
	after, err := scanHandle(row)
	if err != nil {
		return Handle{}, classify(err, KindTransitionRejected)
	}
	md := handleMetadata(after)
	md["action"] = action
	md["reason_code"] = reason
	md["status_before"] = before.Status
	md["status_after"] = after.Status
	md["not_after_before"] = timeOrNil(before.NotAfter)
	md["not_after_after"] = timeOrNil(after.NotAfter)
	if changeRequestID != nil {
		md["change_request_id"] = changeRequestID.String()
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:  after.TenantID,
		ActorType: audit.ActorStaff, ActorID: ac.ActorID,
		Action: auditAction, TargetType: auditTargetHandle, TargetID: after.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
		Metadata: md,
	}); err != nil {
		return Handle{}, err
	}
	return after, nil
}

// RecordTransitionFailure writes the failure audit row for a refused
// transition (generic class only) in its own tenant-scoped transaction.
func RecordTransitionFailure(ctx context.Context, pool TxRunner, tenantID uuid.UUID, handleID uuid.UUID, ac AuditContext) {
	recordTenantFailure(ctx, pool, tenantID, ac, AuditTransitioned, auditTargetHandle, handleID.String(), "transition_rejected")
}

// ListHandles lists the tenant's handles (tenant-scoped tx).
func ListHandles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Handle, error) {
	rows, err := tx.Query(ctx, `SELECT `+handleColumns+` FROM provider_credential_handles
		WHERE tenant_id = $1 ORDER BY domain, provider_id, purpose, created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Handle
	for rows.Next() {
		h, err := scanHandle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListRequests lists a target tenant's requests (platform-scoped tx).
func ListRequests(ctx context.Context, tx pgx.Tx, targetTenantID uuid.UUID) ([]Request, error) {
	rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM provider_credential_change_requests
		WHERE target_tenant_id = $1 ORDER BY requested_at DESC`, targetTenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Approvals, err = listApprovals(ctx, tx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetRequest returns one request of the target tenant with its approvals
// (platform-scoped tx). Not visible -> KindNotFound.
func GetRequest(ctx context.Context, tx pgx.Tx, targetTenantID, requestID uuid.UUID) (Request, error) {
	return getRequest(ctx, tx, targetTenantID, requestID)
}

func getRequest(ctx context.Context, tx pgx.Tx, targetTenantID, requestID uuid.UUID) (Request, error) {
	r, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM provider_credential_change_requests
		WHERE id = $1 AND target_tenant_id = $2`, requestID, targetTenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, newErr(KindNotFound, "request_not_found")
	}
	if err != nil {
		return Request{}, err
	}
	r.Approvals, err = listApprovals(ctx, tx, r.ID)
	return r, err
}

func listApprovals(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]Approval, error) {
	rows, err := tx.Query(ctx, `SELECT id, request_id, approver_principal_id, decision, content_hash, reason_code, decided_at
		FROM provider_credential_change_approvals WHERE request_id = $1 ORDER BY decided_at, id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.RequestID, &a.ApproverPrincipalID, &a.Decision, &a.ContentHash, &a.ReasonCode, &a.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func getHandleForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Handle, error) {
	h, err := scanHandle(tx.QueryRow(ctx, `SELECT `+handleColumns+` FROM provider_credential_handles WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Handle{}, newErr(KindNotFound, "handle_not_found")
	}
	return h, err
}

const handleColumns = `id, tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, vendor_account_id,
	status, status_changed_at, not_before, not_after, activation_request_id, created_at, created_by,
	revoked_at, revoked_by, revoke_reason`

func scanHandle(row pgx.Row) (Handle, error) {
	var h Handle
	err := row.Scan(&h.ID, &h.TenantID, &h.Domain, &h.ProviderID, &h.Purpose, &h.KeyID, &h.SecretRef, &h.Fingerprint,
		&h.VendorAccountID, &h.Status, &h.StatusChangedAt, &h.NotBefore, &h.NotAfter, &h.ActivationRequestID,
		&h.CreatedAt, &h.CreatedBy, &h.RevokedAt, &h.RevokedBy, &h.RevokeReason)
	return h, err
}

const requestColumns = `id, target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint,
	vendor_account_id, not_before, not_after, predecessor_handle_id, predecessor_disposition, predecessor_not_after,
	content_hash, reason_code, requested_by_principal_id, requested_at, state, applied_at, applied_by_principal_id,
	applied_handle_id`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.TargetTenantID, &r.Domain, &r.ProviderID, &r.Purpose, &r.KeyID, &r.SecretRef, &r.Fingerprint,
		&r.VendorAccountID, &r.NotBefore, &r.NotAfter, &r.PredecessorHandleID, &r.PredecessorDisposition, &r.PredecessorNotAfter,
		&r.ContentHash, &r.ReasonCode, &r.RequestedBy, &r.RequestedAt, &r.State, &r.AppliedAt, &r.AppliedBy, &r.AppliedHandleID)
	return r, err
}

// requestMetadata is the audit metadata for a request row: identifiers,
// ref and fingerprint (staff-only, not secret), never a secret value or a
// confirmation value.
func requestMetadata(r Request) map[string]any {
	md := map[string]any{
		"target_tenant_id":        r.TargetTenantID.String(),
		"domain":                  r.Domain,
		"provider_id":             r.ProviderID,
		"purpose":                 r.Purpose,
		"key_id":                  r.KeyID,
		"change_request_id":       r.ID.String(),
		"content_hash":            r.ContentHash,
		"fingerprint":             r.Fingerprint,
		"secret_ref":              r.SecretRef,
		"not_before":              r.NotBefore.UTC().Format(time.RFC3339Nano),
		"not_after":               timeOrNil(r.NotAfter),
		"predecessor_disposition": r.PredecessorDisposition,
		"reason_code":             r.ReasonCode,
		"requester_principal_id":  r.RequestedBy.String(),
	}
	if r.PredecessorHandleID != nil {
		md["predecessor_handle_id"] = r.PredecessorHandleID.String()
	}
	return md
}

func handleMetadata(h Handle) map[string]any {
	return map[string]any{
		"target_tenant_id":      h.TenantID.String(),
		"domain":                h.Domain,
		"provider_id":           h.ProviderID,
		"purpose":               h.Purpose,
		"key_id":                h.KeyID,
		"handle_id":             h.ID.String(),
		"fingerprint":           h.Fingerprint,
		"secret_ref":            h.SecretRef,
		"not_before":            h.NotBefore.UTC().Format(time.RFC3339Nano),
		"activation_request_id": h.ActivationRequestID.String(),
	}
}

func approverIDs(as []Approval) []string {
	var out []string
	for _, a := range as {
		if a.Decision == "approve" {
			out = append(out, a.ApproverPrincipalID.String())
		}
	}
	return out
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// recordPlatformFailure writes a platform-scope failure row (tenant_id
// NULL, metadata.target_tenant_id) with the generic class only. Best
// effort: an audit failure here must not mask the original refusal.
func (s *Subsystem) recordPlatformFailure(ctx context.Context, pool TxRunner, ac AuditContext, action, targetType, targetID, generic string, target uuid.UUID) {
	if ac.ActorID == uuid.Nil {
		return
	}
	err := pool.WithPlatformAdmin(ctx, ac.ActorID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: ac.ActorID,
			Action: action, TargetType: targetType, TargetID: targetID,
			Outcome: audit.OutcomeFailure, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
			Metadata: map[string]any{"target_tenant_id": target.String(), "failure": generic},
		})
	})
	if err != nil {
		s.logger.Error("provider_credential_failure_audit_failed", "action", action)
	}
}

func (s *Subsystem) recordTenantFailure(ctx context.Context, pool TxRunner, tenantID uuid.UUID, ac AuditContext, action, targetType, targetID, generic string) {
	recordTenantFailure(ctx, pool, tenantID, ac, action, targetType, targetID, generic)
}

func recordTenantFailure(ctx context.Context, pool TxRunner, tenantID uuid.UUID, ac AuditContext, action, targetType, targetID, generic string) {
	if ac.ActorID == uuid.Nil || tenantID == uuid.Nil {
		return
	}
	_ = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID:  tenantID,
			ActorType: audit.ActorStaff, ActorID: ac.ActorID,
			Action: action, TargetType: targetType, TargetID: targetID,
			Outcome: audit.OutcomeFailure, IPAddress: ac.IPAddress, UserAgent: ac.UserAgent, RequestID: ac.RequestID,
			Metadata: map[string]any{"target_tenant_id": tenantID.String(), "failure": generic},
		})
	})
}
