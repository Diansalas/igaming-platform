package jurisdiction

// Licensing slice 1 (docs/plans/lic1-record.md; audit gaps G-4/L-22): the audited, platform-admin-only lifecycle
// writes for `licences`: status, validity dates, review date and evidence reference. Before this file nothing in the
// application could write them (MKT-LICSTATUS-1 / MKT-EXPIRY-1).
//
// Every function: assertPlatformScope first (RLS licences_platform_admin_update is the independent backstop), a
// required reason code, a row lock on the licence, the change, and ONE audit row with before/after, the reason code
// and the ChangeKind, all in the caller's transaction. The licence_versions trigger (migration 0176) writes the
// history row in the same statement.
//
// The status vocabulary is the existing one (active / suspended / expired). A regulator REVOCATION is recorded as
// 'expired' with a reason code; a distinct 'revoked' status is a pending human decision, not invented here.
//
// ChangeKind: a WIDENING change makes the licence MORE permissive to rely on (reinstate / set active, move expiry
// later or clear it, move issue date earlier or clear it, postpone or clear a review date); a NARROWING change makes
// it less permissive (suspend, expire, move expiry earlier, set an expiry, move issue date later); NEUTRAL touches no
// enforcement input (evidence reference). Slice 1 applies single-actor + step-up to all three, as the Blueprint-
// independent interim rule. Slice 2 (maker-checker) routes WIDENING changes through an approval: the kind is returned
// in LicenceChange and recorded in the audit row so that routing needs no reclassification. Narrowing stays
// single-actor by design: it is the fail-closed direction.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChangeKind classifies a lifecycle change by its effect on how far the licence may be relied on.
type ChangeKind string

const (
	ChangeWidening  ChangeKind = "widening"
	ChangeNarrowing ChangeKind = "narrowing"
	ChangeNeutral   ChangeKind = "neutral"
)

// LicenceChange is the result of a lifecycle write.
type LicenceChange struct {
	Licence Licence
	Kind    ChangeKind
}

const maxEvidenceReference = 200

// statusRank orders the status vocabulary by restrictiveness (higher = less reliable).
var statusRank = map[string]int{"active": 0, "suspended": 1, "expired": 2}

// ClassifyStatusChange classifies a status transition. An unknown status is ErrInvalidInput; an unchanged status is
// ErrInvalidInput too (no empty history rows).
func ClassifyStatusChange(from, to string) (ChangeKind, error) {
	rf, okf := statusRank[from]
	rt, okt := statusRank[to]
	if !okt {
		return "", fmt.Errorf("%w: status must be one of active, suspended, expired", ErrInvalidInput)
	}
	if !okf {
		// A stored status outside the vocabulary cannot be classified: fail closed (treat as widening so the
		// stricter control applies).
		return ChangeWidening, nil
	}
	switch {
	case rf == rt:
		return "", fmt.Errorf("%w: licence is already %s", ErrInvalidInput, to)
	case rt > rf:
		return ChangeNarrowing, nil
	default:
		return ChangeWidening, nil
	}
}

func dateOnly(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

func dateString(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

func sameDate(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return dateString(a) == dateString(b)
}

// ClassifyValidityDatesChange classifies a change of issued_at / expires_at (nil = not recorded). Both unchanged is
// ErrInvalidInput. Any widening component makes the whole change widening.
func ClassifyValidityDatesChange(oldIssued, oldExpires, newIssued, newExpires *time.Time) (ChangeKind, error) {
	if sameDate(oldIssued, newIssued) && sameDate(oldExpires, newExpires) {
		return "", fmt.Errorf("%w: validity dates are unchanged", ErrInvalidInput)
	}
	widening, narrowing := false, false
	// expires_at: nil means "no expiry asserted" (the most permissive).
	switch {
	case sameDate(oldExpires, newExpires):
	case newExpires == nil:
		widening = true
	case oldExpires == nil:
		narrowing = true
	case newExpires.After(*oldExpires):
		widening = true
	default:
		narrowing = true
	}
	// issued_at: nil means "no issue date asserted" (treated as already issued, the most permissive).
	switch {
	case sameDate(oldIssued, newIssued):
	case newIssued == nil:
		widening = true
	case oldIssued == nil:
		narrowing = true
	case newIssued.Before(*oldIssued):
		widening = true
	default:
		narrowing = true
	}
	if widening {
		return ChangeWidening, nil
	}
	if narrowing {
		return ChangeNarrowing, nil
	}
	return ChangeNeutral, nil
}

// ClassifyReviewDueChange: postponing or clearing a review date reduces oversight (widening); bringing it forward or
// setting one tightens it (narrowing).
func ClassifyReviewDueChange(old, new *time.Time) (ChangeKind, error) {
	switch {
	case sameDate(old, new):
		return "", fmt.Errorf("%w: review_due_at is unchanged", ErrInvalidInput)
	case new == nil:
		return ChangeWidening, nil
	case old == nil:
		return ChangeNarrowing, nil
	case new.After(*old):
		return ChangeWidening, nil
	default:
		return ChangeNarrowing, nil
	}
}

// validateEvidenceReference accepts an opaque business-supplied reference: 1..200 characters, no control characters,
// no e-mail shape ("@"), already trimmed. "" is not a reference (use the clear operation).
func validateEvidenceReference(s string) error {
	if s != strings.TrimSpace(s) || s == "" {
		return fmt.Errorf("%w: evidence_reference must be non-empty and have no leading or trailing space", ErrInvalidInput)
	}
	if len([]rune(s)) > maxEvidenceReference {
		return fmt.Errorf("%w: evidence_reference is at most %d characters", ErrInvalidInput, maxEvidenceReference)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: evidence_reference must not contain control characters", ErrInvalidInput)
		}
	}
	if strings.Contains(s, "@") {
		return fmt.Errorf("%w: evidence_reference must be an opaque reference, not contact details", ErrInvalidInput)
	}
	return nil
}

const licenceSelect = `SELECT id, jurisdiction_id, licensee, licence_number, status, created_at,
	issued_at, expires_at, review_due_at, COALESCE(evidence_reference, ''), updated_at FROM licences`

func scanLicenceLifecycle(row pgx.Row) (Licence, error) {
	var l Licence
	err := row.Scan(&l.ID, &l.JurisdictionID, &l.Licensee, &l.LicenceNumber, &l.Status, &l.CreatedAt,
		&l.IssuedAt, &l.ExpiresAt, &l.ReviewDueAt, &l.EvidenceReference, &l.UpdatedAt)
	return l, err
}

func lockLicence(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Licence, error) {
	if id == uuid.Nil {
		return Licence{}, fmt.Errorf("%w: licence id is required", ErrInvalidInput)
	}
	l, err := scanLicenceLifecycle(tx.QueryRow(ctx, licenceSelect+` WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Licence{}, fmt.Errorf("%w: licence %s", ErrNotFound, id)
	}
	if err != nil {
		return Licence{}, fmt.Errorf("jurisdiction: read licence %s: %w", id, err)
	}
	return l, nil
}

func finishLicenceChange(ctx context.Context, tx pgx.Tx, actor ActorContext, action string, before Licence, kind ChangeKind) (LicenceChange, error) {
	after, err := scanLicenceLifecycle(tx.QueryRow(ctx, licenceSelect+` WHERE id = $1`, before.ID))
	if err != nil {
		return LicenceChange{}, fmt.Errorf("jurisdiction: re-read licence %s: %w", before.ID, err)
	}
	if err := recordRegistryAudit(ctx, tx, uuid.Nil, actor, action, "licence", after.ID.String(), map[string]any{
		"before": licenceState(before), "after": licenceState(after), "change_kind": string(kind),
	}); err != nil {
		return LicenceChange{}, err
	}
	return LicenceChange{Licence: after, Kind: kind}, nil
}

// SetLicenceStatus sets licences.status. tx MUST be platform-admin-scoped.
func SetLicenceStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string, actor ActorContext) (LicenceChange, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return LicenceChange{}, err
	}
	if err := actor.validate(); err != nil {
		return LicenceChange{}, err
	}
	before, err := lockLicence(ctx, tx, id)
	if err != nil {
		return LicenceChange{}, err
	}
	kind, err := ClassifyStatusChange(before.Status, status)
	if err != nil {
		return LicenceChange{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE licences SET status = $2 WHERE id = $1`, id, status); err != nil {
		return LicenceChange{}, fmt.Errorf("jurisdiction: update licences.status: %w", err)
	}
	return finishLicenceChange(ctx, tx, actor, "jurisdiction_registry.licence_status_set", before, kind)
}

// SetLicenceValidityDates sets issued_at and expires_at together (nil clears). When both are set, expires_at must be
// after issued_at. Dates are UTC calendar dates; the boundary semantics are EvaluateLicenceValidity's, unchanged.
func SetLicenceValidityDates(ctx context.Context, tx pgx.Tx, id uuid.UUID, issuedAt, expiresAt *time.Time, actor ActorContext) (LicenceChange, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return LicenceChange{}, err
	}
	if err := actor.validate(); err != nil {
		return LicenceChange{}, err
	}
	issuedAt, expiresAt = dateOnly(issuedAt), dateOnly(expiresAt)
	if issuedAt != nil && expiresAt != nil && !expiresAt.After(*issuedAt) {
		return LicenceChange{}, fmt.Errorf("%w: expires_at must be after issued_at", ErrInvalidInput)
	}
	before, err := lockLicence(ctx, tx, id)
	if err != nil {
		return LicenceChange{}, err
	}
	kind, err := ClassifyValidityDatesChange(before.IssuedAt, before.ExpiresAt, issuedAt, expiresAt)
	if err != nil {
		return LicenceChange{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE licences SET issued_at = $2, expires_at = $3 WHERE id = $1`, id, issuedAt, expiresAt); err != nil {
		return LicenceChange{}, fmt.Errorf("jurisdiction: update licences validity dates: %w", err)
	}
	return finishLicenceChange(ctx, tx, actor, "jurisdiction_registry.licence_validity_dates_set", before, kind)
}

// SetLicenceReviewDue sets review_due_at (nil clears).
func SetLicenceReviewDue(ctx context.Context, tx pgx.Tx, id uuid.UUID, reviewDueAt *time.Time, actor ActorContext) (LicenceChange, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return LicenceChange{}, err
	}
	if err := actor.validate(); err != nil {
		return LicenceChange{}, err
	}
	reviewDueAt = dateOnly(reviewDueAt)
	before, err := lockLicence(ctx, tx, id)
	if err != nil {
		return LicenceChange{}, err
	}
	kind, err := ClassifyReviewDueChange(before.ReviewDueAt, reviewDueAt)
	if err != nil {
		return LicenceChange{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE licences SET review_due_at = $2 WHERE id = $1`, id, reviewDueAt); err != nil {
		return LicenceChange{}, fmt.Errorf("jurisdiction: update licences.review_due_at: %w", err)
	}
	return finishLicenceChange(ctx, tx, actor, "jurisdiction_registry.licence_review_due_set", before, kind)
}

// SetLicenceEvidenceReference sets the opaque evidence reference; nil clears it. The platform never fabricates one.
func SetLicenceEvidenceReference(ctx context.Context, tx pgx.Tx, id uuid.UUID, ref *string, actor ActorContext) (LicenceChange, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return LicenceChange{}, err
	}
	if err := actor.validate(); err != nil {
		return LicenceChange{}, err
	}
	if ref != nil {
		if err := validateEvidenceReference(*ref); err != nil {
			return LicenceChange{}, err
		}
	}
	before, err := lockLicence(ctx, tx, id)
	if err != nil {
		return LicenceChange{}, err
	}
	cur := before.EvidenceReference
	if (ref == nil && cur == "") || (ref != nil && *ref == cur) {
		return LicenceChange{}, fmt.Errorf("%w: evidence_reference is unchanged", ErrInvalidInput)
	}
	if _, err := tx.Exec(ctx, `UPDATE licences SET evidence_reference = $2 WHERE id = $1`, id, ref); err != nil {
		return LicenceChange{}, fmt.Errorf("jurisdiction: update licences.evidence_reference: %w", err)
	}
	return finishLicenceChange(ctx, tx, actor, "jurisdiction_registry.licence_evidence_reference_set", before, ChangeNeutral)
}

// LicenceVersion mirrors one licence_versions row.
type LicenceVersion struct {
	LicenceID         uuid.UUID
	VersionNo         int
	Operation         string
	ChangedAt         time.Time
	ChangedBy         *uuid.UUID
	Status            string
	IssuedAt          *time.Time
	ExpiresAt         *time.Time
	ReviewDueAt       *time.Time
	EvidenceReference string
	LicenceNumber     string
}

// ListLicenceVersions returns a licence's history, oldest first. tx MUST be platform-admin-scoped.
func ListLicenceVersions(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]LicenceVersion, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, err
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: licence id is required", ErrInvalidInput)
	}
	if _, err := readLicence(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT licence_id, version_no, operation, changed_at, changed_by, status, issued_at, expires_at,
		review_due_at, COALESCE(evidence_reference, ''), licence_number FROM licence_versions WHERE licence_id = $1 ORDER BY version_no`, id)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list licence versions: %w", err)
	}
	defer rows.Close()
	var out []LicenceVersion
	for rows.Next() {
		var v LicenceVersion
		if err := rows.Scan(&v.LicenceID, &v.VersionNo, &v.Operation, &v.ChangedAt, &v.ChangedBy, &v.Status, &v.IssuedAt,
			&v.ExpiresAt, &v.ReviewDueAt, &v.EvidenceReference, &v.LicenceNumber); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan licence version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func readLicence(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Licence, error) {
	l, err := scanLicenceLifecycle(tx.QueryRow(ctx, licenceSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Licence{}, fmt.Errorf("%w: licence %s", ErrNotFound, id)
	}
	if err != nil {
		return Licence{}, fmt.Errorf("jurisdiction: read licence %s: %w", id, err)
	}
	return l, nil
}
