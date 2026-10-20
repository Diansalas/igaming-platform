package operatingmarket

// lic9 (migration 0180): the operating-basis WRITE path (platform scope, executed only by the licensing change executor) and the
// platform READ views. Every change is a NEW version via close-then-insert in ONE transaction (no ON CONFLICT DO UPDATE). The
// database is the control: operating_bases accepts an INSERT / close only inside an executed licensing change request of the
// matching kind (LC040), so calling these functions outside the executor fails with a guard error. They take no tenant, brand or
// licence from a client: the executor passes the request payload's validated values.
//
// Nothing here is a legal position: a basis row is a recorded business decision by named staff (the executed change request).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

// BasisRecord is an operating_bases row.
type BasisRecord struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	BrandID                *uuid.UUID
	CountryCode            string
	OperationCode          string
	ProductCode            *string
	Kind                   BasisKind
	LicenceID              *uuid.UUID
	JurisdictionID         *uuid.UUID
	DeterminationReference string
	DeterminedByRole       string
	EvidenceReference      *string
	RestrictionsNote       *string
	RestrictedProductCodes []string
	ReviewDueAt            *time.Time // a DATE
	NeedsRereview          bool
	LastReviewReference    *string
	Status                 Status
	ReasonCode             string
	PolicyVersion          string
	EffectiveFrom          time.Time
	EffectiveTo            *time.Time
	ChangeRequestID        *uuid.UUID
	CreatedByActorID       *uuid.UUID
	CreatedAt              time.Time
}

// BasisScope is the key of an operating basis. BrandID nil = tenant-wide; ProductCode nil = every product.
type BasisScope struct {
	TenantID      uuid.UUID
	BrandID       *uuid.UUID
	CountryCode   string
	OperationCode string
	ProductCode   *string
}

func (s BasisScope) validate() error {
	if s.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !validation.IsISO3166Alpha2(s.CountryCode) {
		return fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	if strings.TrimSpace(s.OperationCode) == "" {
		return fmt.Errorf("%w: operation_code is required", ErrInvalidInput)
	}
	return nil
}

// SetOperatingBasisParams is SetOperatingBasis' input. Deliberately absent: effective_from / effective_to (DB-stamped),
// change_request_id and created_by_actor_id (the guard trigger stamps them from the executed request), policy_version.
type SetOperatingBasisParams struct {
	Scope                  BasisScope
	Kind                   BasisKind
	LicenceID              *uuid.UUID // required for the two licence kinds, nil for no_licence_documented
	JurisdictionID         *uuid.UUID // required for no_licence_documented, nil otherwise
	DeterminationReference string
	DeterminedByRole       string
	EvidenceReference      *string
	RestrictionsNote       *string
	RestrictedProductCodes []string
	ReviewDueAt            *string // YYYY-MM-DD; required for no_licence_documented
	Actor                  ActorContext
}

// WithdrawOperatingBasisParams is WithdrawOperatingBasis' input.
type WithdrawOperatingBasisParams struct {
	Scope BasisScope
	Actor ActorContext
}

// RecordBasisReviewParams is RecordOperatingBasisReview's input.
type RecordBasisReviewParams struct {
	Scope           BasisScope
	ReviewDueAt     string // YYYY-MM-DD
	NeedsRereview   bool
	ReviewReference string
	Actor           ActorContext
}

const basisDateLayout = "2006-01-02"

const basisColumns = `id, tenant_id, brand_id, country_code, operation_code, product_code, basis_kind, licence_id, jurisdiction_id,
	determination_reference, determined_by_role, evidence_reference, restrictions_note, restricted_product_codes, review_due_at,
	needs_rereview, last_review_reference, status, reason_code, policy_version, effective_from, effective_to, change_request_id,
	created_by_actor_id, created_at`

func scanBasis(row pgx.Row) (BasisRecord, error) {
	var r BasisRecord
	var kind, status string
	if err := row.Scan(&r.ID, &r.TenantID, &r.BrandID, &r.CountryCode, &r.OperationCode, &r.ProductCode, &kind, &r.LicenceID,
		&r.JurisdictionID, &r.DeterminationReference, &r.DeterminedByRole, &r.EvidenceReference, &r.RestrictionsNote,
		&r.RestrictedProductCodes, &r.ReviewDueAt, &r.NeedsRereview, &r.LastReviewReference, &status, &r.ReasonCode,
		&r.PolicyVersion, &r.EffectiveFrom, &r.EffectiveTo, &r.ChangeRequestID, &r.CreatedByActorID, &r.CreatedAt); err != nil {
		return BasisRecord{}, err
	}
	var err error
	if r.Kind, err = parseBasisKind(kind); err != nil {
		return BasisRecord{}, fmt.Errorf("operatingmarket: stored basis kind is invalid: %w", err)
	}
	if r.Status, err = parseStatus(status); err != nil {
		return BasisRecord{}, fmt.Errorf("operatingmarket: stored basis status is invalid: %w", err)
	}
	return r, nil
}

func basisRecordState(r BasisRecord) map[string]any {
	var to, due any
	if r.EffectiveTo != nil {
		to = r.EffectiveTo.Format(time.RFC3339Nano)
	}
	if r.ReviewDueAt != nil {
		due = r.ReviewDueAt.Format(basisDateLayout)
	}
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	id := func(p *uuid.UUID) any {
		if p == nil {
			return nil
		}
		return p.String()
	}
	return map[string]any{
		"id": r.ID.String(), "tenant_id": r.TenantID.String(), "brand_id": id(r.BrandID), "country_code": r.CountryCode,
		"operation_code": r.OperationCode, "product_code": str(r.ProductCode), "basis_kind": string(r.Kind),
		"licence_id": id(r.LicenceID), "jurisdiction_id": id(r.JurisdictionID),
		"determination_reference": r.DeterminationReference, "determined_by_role": r.DeterminedByRole,
		"evidence_reference": str(r.EvidenceReference), "restrictions_note": str(r.RestrictionsNote),
		"restricted_product_codes": r.RestrictedProductCodes, "review_due_at": due, "needs_rereview": r.NeedsRereview,
		"last_review_reference": str(r.LastReviewReference), "status": string(r.Status), "reason_code": r.ReasonCode,
		"policy_version": r.PolicyVersion, "effective_from": r.EffectiveFrom.Format(time.RFC3339Nano), "effective_to": to,
		"change_request_id": id(r.ChangeRequestID),
	}
}

func parseBasisDate(s, name string) (time.Time, error) {
	t, err := time.Parse(basisDateLayout, s)
	if err != nil || t.Format(basisDateLayout) != s {
		return time.Time{}, fmt.Errorf("%w: %s must be a YYYY-MM-DD date", ErrInvalidInput, name)
	}
	return t, nil
}

func cleanText(field string, s string, max int) (string, error) {
	if s != strings.TrimSpace(s) || s == "" || len(s) > max || strings.ContainsAny(s, "\x00\r\n\t") || strings.Contains(s, "@") {
		return "", fmt.Errorf("%w: %s must be an opaque, trimmed value of 1..%d bytes without control characters or contact details", ErrInvalidInput, field, max)
	}
	return s, nil
}

func classifyBasisWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		return ErrConcurrentPolicyWrite
	case "23503", "23514":
		return fmt.Errorf("%w: %s", ErrInvalidInput, pgErr.Message)
	}
	return err
}

// lockOpenBasis locks the latest version of the scope and returns it when it is OPEN (nil when the scope has no open version).
func lockOpenBasis(ctx context.Context, tx pgx.Tx, s BasisScope) (*BasisRecord, error) {
	r, err := scanBasis(tx.QueryRow(ctx, `
		SELECT `+basisColumns+` FROM operating_bases
		 WHERE tenant_id = $1 AND brand_id IS NOT DISTINCT FROM $2 AND country_code = $3 AND operation_code = $4
		   AND product_code IS NOT DISTINCT FROM $5
		 ORDER BY effective_from DESC, created_at DESC LIMIT 1 FOR UPDATE`,
		s.TenantID, s.BrandID, s.CountryCode, s.OperationCode, s.ProductCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("operatingmarket: lock latest operating basis version: %w", err)
	}
	if r.EffectiveTo != nil {
		return nil, nil
	}
	return &r, nil
}

func closeBasis(ctx context.Context, tx pgx.Tx, id uuid.UUID) (time.Time, error) {
	var closed time.Time
	err := tx.QueryRow(ctx, `UPDATE operating_bases SET effective_to = now() WHERE id = $1 AND effective_to IS NULL RETURNING effective_to`, id).Scan(&closed)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgErr) && pgErr.Code == "23514") {
			return time.Time{}, ErrConcurrentPolicyWrite
		}
		return time.Time{}, fmt.Errorf("operatingmarket: close prior operating basis version: %w", classifyBasisWriteError(err))
	}
	return closed, nil
}

func insertBasis(ctx context.Context, tx pgx.Tx, r BasisRecord) (BasisRecord, error) {
	codes := r.RestrictedProductCodes
	if codes == nil {
		codes = []string{}
	}
	out, err := scanBasis(tx.QueryRow(ctx, `
		INSERT INTO operating_bases (tenant_id, brand_id, country_code, operation_code, product_code, basis_kind, licence_id, jurisdiction_id,
			determination_reference, determined_by_role, evidence_reference, restrictions_note, restricted_product_codes, review_due_at,
			needs_rereview, last_review_reference, status, reason_code, policy_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		RETURNING `+basisColumns,
		r.TenantID, r.BrandID, r.CountryCode, r.OperationCode, r.ProductCode, string(r.Kind), r.LicenceID, r.JurisdictionID,
		r.DeterminationReference, r.DeterminedByRole, r.EvidenceReference, r.RestrictionsNote, codes, r.ReviewDueAt,
		r.NeedsRereview, r.LastReviewReference, string(r.Status), r.ReasonCode, PolicyVersion))
	if err != nil {
		return BasisRecord{}, fmt.Errorf("operatingmarket: insert operating basis version: %w", classifyBasisWriteError(err))
	}
	return out, nil
}

func platformBasisPrologue(ctx context.Context, tx pgx.Tx, actor ActorContext, s BasisScope) error {
	if err := actor.validate(); err != nil {
		return err
	}
	if err := assertPlatformScope(ctx, tx); err != nil {
		return err
	}
	return s.validate()
}

func auditBasis(ctx context.Context, tx pgx.Tx, actor ActorContext, action string, before *BasisRecord, after BasisRecord, widening bool) error {
	var b any
	if before != nil {
		b = basisRecordState(*before)
	}
	return recordOperatingMarketAudit(ctx, tx, uuid.Nil, actor, action, "operating_bases", after.ID.String(), map[string]any{
		"before": b, "after": basisRecordState(after), "tenant_id": after.TenantID.String(), "country_code": after.CountryCode,
		"operation_code": after.OperationCode, "basis_kind": string(after.Kind), "status": string(after.Status),
		"version_id": after.ID.String(), "policy_version": PolicyVersion, "widening_capable": widening,
		"change_request_id": func() any {
			if after.ChangeRequestID == nil {
				return nil
			}
			return after.ChangeRequestID.String()
		}(),
	})
}

// SetOperatingBasis appends a new ACTIVE version of the scope's basis, closing the open version (if any) in the same
// transaction. tx MUST be a platform-admin transaction, and the call is admitted by the database only inside an executed
// operating_basis_set request of this transaction. Call at most once per scope per transaction.
func SetOperatingBasis(ctx context.Context, tx pgx.Tx, p SetOperatingBasisParams) (BasisRecord, error) {
	if err := platformBasisPrologue(ctx, tx, p.Actor, p.Scope); err != nil {
		return BasisRecord{}, err
	}
	kind, err := parseBasisKind(string(p.Kind))
	if err != nil {
		return BasisRecord{}, err
	}
	rec := BasisRecord{
		TenantID: p.Scope.TenantID, BrandID: p.Scope.BrandID, CountryCode: p.Scope.CountryCode, OperationCode: p.Scope.OperationCode,
		ProductCode: p.Scope.ProductCode, Kind: kind, Status: StatusActive, ReasonCode: p.Actor.ReasonCode,
		RestrictedProductCodes: p.RestrictedProductCodes,
	}
	if kind == BasisNoLicenceDocumented {
		if p.LicenceID != nil || p.JurisdictionID == nil || p.ReviewDueAt == nil {
			return BasisRecord{}, fmt.Errorf("%w: a no_licence_documented basis needs a jurisdiction_id and a review_due_at and carries no licence_id", ErrInvalidInput)
		}
	} else if p.LicenceID == nil || p.JurisdictionID != nil {
		return BasisRecord{}, fmt.Errorf("%w: a licence basis needs a licence_id and carries no jurisdiction_id", ErrInvalidInput)
	}
	rec.LicenceID, rec.JurisdictionID = p.LicenceID, p.JurisdictionID
	if rec.DeterminationReference, err = cleanText("determination_reference", p.DeterminationReference, 200); err != nil {
		return BasisRecord{}, err
	}
	if rec.DeterminedByRole, err = cleanText("determined_by_role", p.DeterminedByRole, 100); err != nil {
		return BasisRecord{}, err
	}
	for _, f := range []struct {
		name string
		in   *string
		out  **string
		max  int
	}{{"evidence_reference", p.EvidenceReference, &rec.EvidenceReference, 200}, {"restrictions_note", p.RestrictionsNote, &rec.RestrictionsNote, 1000}} {
		if f.in != nil {
			v, err := cleanText(f.name, *f.in, f.max)
			if err != nil {
				return BasisRecord{}, err
			}
			*f.out = &v
		}
	}
	if p.ReviewDueAt != nil {
		t, err := parseBasisDate(*p.ReviewDueAt, "review_due_at")
		if err != nil {
			return BasisRecord{}, err
		}
		rec.ReviewDueAt = &t
	}

	before, err := lockOpenBasis(ctx, tx, p.Scope)
	if err != nil {
		return BasisRecord{}, err
	}
	if before != nil {
		if _, err := closeBasis(ctx, tx, before.ID); err != nil {
			return BasisRecord{}, err
		}
	}
	out, err := insertBasis(ctx, tx, rec)
	if err != nil {
		return BasisRecord{}, err
	}
	if err := auditBasis(ctx, tx, p.Actor, "operating_market.operating_basis_set", before, out, true); err != nil {
		return BasisRecord{}, err
	}
	return out, nil
}

// WithdrawOperatingBasis appends a WITHDRAWN version (the same content, status withdrawn) closing the open active version. Always
// narrowing: the scope no longer relies on any basis (and does NOT fall back to the tenant's licence).
func WithdrawOperatingBasis(ctx context.Context, tx pgx.Tx, p WithdrawOperatingBasisParams) (BasisRecord, error) {
	if err := platformBasisPrologue(ctx, tx, p.Actor, p.Scope); err != nil {
		return BasisRecord{}, err
	}
	before, err := lockOpenBasis(ctx, tx, p.Scope)
	if err != nil {
		return BasisRecord{}, err
	}
	if before == nil || before.Status != StatusActive {
		return BasisRecord{}, fmt.Errorf("%w: there is no active operating basis for this scope", ErrInvalidInput)
	}
	if _, err := closeBasis(ctx, tx, before.ID); err != nil {
		return BasisRecord{}, err
	}
	next := *before
	next.Status, next.ReasonCode = StatusWithdrawn, p.Actor.ReasonCode
	out, err := insertBasis(ctx, tx, next)
	if err != nil {
		return BasisRecord{}, err
	}
	if err := auditBasis(ctx, tx, p.Actor, "operating_market.operating_basis_withdrawn", before, out, false); err != nil {
		return BasisRecord{}, err
	}
	return out, nil
}

// RecordOperatingBasisReview appends an ACTIVE version with a new review date / re-review flag (and the review's own reference),
// content otherwise unchanged. widening_capable in the audit row mirrors the database classification (later date or cleared flag).
func RecordOperatingBasisReview(ctx context.Context, tx pgx.Tx, p RecordBasisReviewParams) (BasisRecord, error) {
	if err := platformBasisPrologue(ctx, tx, p.Actor, p.Scope); err != nil {
		return BasisRecord{}, err
	}
	due, err := parseBasisDate(p.ReviewDueAt, "review_due_at")
	if err != nil {
		return BasisRecord{}, err
	}
	ref, err := cleanText("review_reference", p.ReviewReference, 200)
	if err != nil {
		return BasisRecord{}, err
	}
	before, err := lockOpenBasis(ctx, tx, p.Scope)
	if err != nil {
		return BasisRecord{}, err
	}
	if before == nil || before.Status != StatusActive {
		return BasisRecord{}, fmt.Errorf("%w: there is no active operating basis for this scope", ErrInvalidInput)
	}
	if _, err := closeBasis(ctx, tx, before.ID); err != nil {
		return BasisRecord{}, err
	}
	next := *before
	next.ReviewDueAt, next.NeedsRereview, next.LastReviewReference, next.ReasonCode = &due, p.NeedsRereview, &ref, p.Actor.ReasonCode
	widening := (before.NeedsRereview && !p.NeedsRereview) || (before.ReviewDueAt != nil && due.After(*before.ReviewDueAt))
	out, err := insertBasis(ctx, tx, next)
	if err != nil {
		return BasisRecord{}, err
	}
	if err := auditBasis(ctx, tx, p.Actor, "operating_market.operating_basis_review_recorded", before, out, widening); err != nil {
		return BasisRecord{}, err
	}
	return out, nil
}

// ---- platform read views --------------------------------------------------------------------------------------------------

// BasisDecision is who decided a basis version, derived from its executed change request (no personal data is copied into the
// operating_bases row): the requester, the executor and the approving staff accounts.
type BasisDecision struct {
	RequestID   uuid.UUID
	Kind        string
	ReasonCode  string
	RequestedBy uuid.UUID
	ExecutedBy  *uuid.UUID
	ExecutedAt  *time.Time
	ApproverIDs []uuid.UUID
}

// ListOperatingBasesPage lists a tenant's operating basis versions, newest first, with the total. currentOnly keeps only the open
// version of each scope. tx MUST be platform-admin-scoped (operating_bases has a platform read policy; no tenant binding is used).
func ListOperatingBasesPage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, countryCode string, currentOnly bool, limit, offset int) ([]BasisRecord, int, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, 0, err
	}
	if err := setReadOnly(ctx, tx); err != nil {
		return nil, 0, err
	}
	if tenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	countryCode = strings.TrimSpace(countryCode)
	if countryCode != "" && !validation.IsISO3166Alpha2(countryCode) {
		return nil, 0, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: read tenant: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("%w: unknown tenant_id %s", ErrNotFound, tenantID)
	}
	const where = ` WHERE tenant_id = $1 AND ($2 = '' OR country_code = $2) AND (NOT $3 OR effective_to IS NULL)`
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`+where, tenantID, countryCode, currentOnly).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: count operating bases: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+basisColumns+` FROM operating_bases`+where+`
		ORDER BY country_code, operation_code, effective_from DESC, id LIMIT $4 OFFSET $5`, tenantID, countryCode, currentOnly, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: list operating bases: %w", err)
	}
	defer rows.Close()
	var out []BasisRecord
	for rows.Next() {
		r, err := scanBasis(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: scan operating basis: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetOperatingBasis reads one version and its decision record. tx MUST be platform-admin-scoped.
func GetOperatingBasis(ctx context.Context, tx pgx.Tx, id uuid.UUID) (BasisRecord, *BasisDecision, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return BasisRecord{}, nil, err
	}
	if err := setReadOnly(ctx, tx); err != nil {
		return BasisRecord{}, nil, err
	}
	if id == uuid.Nil {
		return BasisRecord{}, nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	rec, err := scanBasis(tx.QueryRow(ctx, `SELECT `+basisColumns+` FROM operating_bases WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BasisRecord{}, nil, fmt.Errorf("%w: operating basis %s", ErrNotFound, id)
	}
	if err != nil {
		return BasisRecord{}, nil, fmt.Errorf("operatingmarket: read operating basis: %w", err)
	}
	if rec.ChangeRequestID == nil {
		return rec, nil, nil
	}
	d := BasisDecision{RequestID: *rec.ChangeRequestID}
	if err := tx.QueryRow(ctx, `SELECT kind, reason_code, requested_by, executed_by, executed_at FROM licensing_change_requests WHERE id = $1`,
		d.RequestID).Scan(&d.Kind, &d.ReasonCode, &d.RequestedBy, &d.ExecutedBy, &d.ExecutedAt); err != nil {
		return BasisRecord{}, nil, fmt.Errorf("operatingmarket: read the basis' change request: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT approver_id FROM licensing_change_approvals WHERE request_id = $1 ORDER BY decided_at, id`, d.RequestID)
	if err != nil {
		return BasisRecord{}, nil, fmt.Errorf("operatingmarket: read the basis' approvals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a uuid.UUID
		if err := rows.Scan(&a); err != nil {
			return BasisRecord{}, nil, fmt.Errorf("operatingmarket: scan approval: %w", err)
		}
		d.ApproverIDs = append(d.ApproverIDs, a)
	}
	return rec, &d, rows.Err()
}

// ListInForceOperatingBases returns every operating basis version of the tenant that is in force at asOf (active and withdrawn
// alike; the caller decides what a withdrawn row means), for READINESS display. tx MUST be a tenant-scoped, non-player-scoped
// transaction for tenantID (asserted first). It is a read of stored rows, never a permission decision.
func ListInForceOperatingBases(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, asOf time.Time) ([]BasisRecord, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if !inScope {
		return nil, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, tenantID)
	}
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: as_of is required and must not be the zero time", ErrInvalidInput)
	}
	rows, err := tx.Query(ctx, `SELECT `+basisColumns+` FROM operating_bases
		WHERE tenant_id = $1 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
		ORDER BY country_code, operation_code, id`, tenantID, asOf)
	if err != nil {
		return nil, fmt.Errorf("operatingmarket: list in-force operating bases: %w", err)
	}
	defer rows.Close()
	var out []BasisRecord
	for rows.Next() {
		r, err := scanBasis(rows)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: scan operating basis: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
