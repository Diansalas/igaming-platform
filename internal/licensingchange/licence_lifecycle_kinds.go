package licensingchange

// Migration 0178 (lic4 slice 4a): the licence lifecycle writes (licensing slice 1, migration 0176) as licensing change kinds.
// Extension recipe (docs/plans/lic2-maker-checker-record.md section 6): registry rows + licensing_change_classify and
// licensing_change_row_matches branches + the BEFORE UPDATE governed trigger on licences are in migration 0178; this file is the
// Go register() of the four kinds. The executors call slice 1's writers (jurisdiction.SetLicence*) unchanged, so their
// validation, row lock, ChangeKind classification and before/after audit row stay authoritative; the DATABASE classifies the
// request (widening or not) against the locked current licence row and the Go classification is the same rule.
//
// Payload keys are fixed and always all present (a JSON null clears): licence_status_change {licence_id, status};
// licence_validity_dates_change {licence_id, issued_at, expires_at}; licence_review_due_change {licence_id, review_due_at};
// licence_evidence_reference_change {licence_id, licence_evidence_reference}. A missing key is refused (a missing date must
// never be read as "clear").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// LicenceStatusPayload is the payload of KindLicenceStatusChange.
type LicenceStatusPayload struct {
	LicenceID uuid.UUID `json:"licence_id"`
	Status    string    `json:"status"`
}

// LicenceValidityDatesPayload is the payload of KindLicenceValidityDatesChange (dates are "YYYY-MM-DD" or null).
type LicenceValidityDatesPayload struct {
	LicenceID uuid.UUID `json:"licence_id"`
	IssuedAt  *string   `json:"issued_at"`
	ExpiresAt *string   `json:"expires_at"`
}

// LicenceReviewDuePayload is the payload of KindLicenceReviewDueChange.
type LicenceReviewDuePayload struct {
	LicenceID   uuid.UUID `json:"licence_id"`
	ReviewDueAt *string   `json:"review_due_at"`
}

// LicenceEvidenceReferencePayload is the payload of KindLicenceEvidenceReferenceChange.
type LicenceEvidenceReferencePayload struct {
	LicenceID uuid.UUID `json:"licence_id"`
	Reference *string   `json:"licence_evidence_reference"`
}

const dateLayout = "2006-01-02"

// requireKeys refuses a payload that does not carry every key (strictDecode already refuses unknown ones).
func requireKeys(raw json.RawMessage, keys ...string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidInput)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%w: payload key %q is required (use null to clear)", ErrInvalidInput, k)
		}
	}
	return nil
}

func normDate(s *string, name string) (*string, error) {
	if s == nil {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, *s)
	if err != nil || t.Format(dateLayout) != *s {
		return nil, fmt.Errorf("%w: %s must be a YYYY-MM-DD date or null", ErrInvalidInput, name)
	}
	return s, nil
}

func parseDatePtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, _ := time.Parse(dateLayout, *s)
	return &t
}

// currentLicence reads the licence the request is about (the request transaction is platform-scoped).
func currentLicence(ctx context.Context, tx pgx.Tx, id uuid.UUID) (status string, issued, expires, review *time.Time, evidence string, err error) {
	err = tx.QueryRow(ctx, `SELECT status, issued_at, expires_at, review_due_at, COALESCE(evidence_reference, '') FROM licences WHERE id = $1`, id).
		Scan(&status, &issued, &expires, &review, &evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil, nil, "", &DomainError{Err: fmt.Errorf("%w: licence %s", jurisdiction.ErrNotFound, id)}
	}
	return
}

// licenceOutcome is ids and the change kind only (the outcome is rendered by the generic licensing change responses; the
// lifecycle routes re-read the licence for their own response).
func licenceOutcome(ch jurisdiction.LicenceChange) map[string]any {
	return map[string]any{"licence_id": ch.Licence.ID.String(), "change_kind": string(ch.Kind)}
}

func init() {
	platform := func(any) auth.Permission { return "" }

	register(KindLicenceStatusChange, kindSpec{
		platformOnly: true, platformPerm: auth.PermJurisdictionRegistryManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceStatusPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "licence_id", "status"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.LicenceID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
			}
			if p.Status != "active" && p.Status != "suspended" && p.Status != "expired" {
				return nil, uuid.Nil, fmt.Errorf("%w: status must be one of active, suspended, expired", ErrInvalidInput)
			}
			return p, uuid.Nil, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceStatusPayload)
			cur, _, _, _, _, err := currentLicence(ctx, tx, p.LicenceID)
			if err != nil {
				return err
			}
			if _, err := jurisdiction.ClassifyStatusChange(cur, p.Status); err != nil {
				return &DomainError{Err: err}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceStatusPayload)
			ch, err := jurisdiction.SetLicenceStatus(ctx, tx, p.LicenceID, p.Status, ex.jurisdictionActor())
			if err != nil {
				return nil, err
			}
			return licenceOutcome(ch), nil
		},
	})

	register(KindLicenceValidityDatesChange, kindSpec{
		platformOnly: true, platformPerm: auth.PermJurisdictionRegistryManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceValidityDatesPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "licence_id", "issued_at", "expires_at"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.LicenceID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
			}
			var err error
			if p.IssuedAt, err = normDate(p.IssuedAt, "issued_at"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.ExpiresAt, err = normDate(p.ExpiresAt, "expires_at"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.IssuedAt != nil && p.ExpiresAt != nil && *p.ExpiresAt <= *p.IssuedAt {
				return nil, uuid.Nil, fmt.Errorf("%w: expires_at must be after issued_at", ErrInvalidInput)
			}
			return p, uuid.Nil, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceValidityDatesPayload)
			_, oi, oe, _, _, err := currentLicence(ctx, tx, p.LicenceID)
			if err != nil {
				return err
			}
			if _, err := jurisdiction.ClassifyValidityDatesChange(oi, oe, parseDatePtr(p.IssuedAt), parseDatePtr(p.ExpiresAt)); err != nil {
				return &DomainError{Err: err}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceValidityDatesPayload)
			ch, err := jurisdiction.SetLicenceValidityDates(ctx, tx, p.LicenceID, parseDatePtr(p.IssuedAt), parseDatePtr(p.ExpiresAt), ex.jurisdictionActor())
			if err != nil {
				return nil, err
			}
			return licenceOutcome(ch), nil
		},
	})

	register(KindLicenceReviewDueChange, kindSpec{
		platformOnly: true, platformPerm: auth.PermJurisdictionRegistryManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceReviewDuePayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "licence_id", "review_due_at"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.LicenceID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
			}
			var err error
			if p.ReviewDueAt, err = normDate(p.ReviewDueAt, "review_due_at"); err != nil {
				return nil, uuid.Nil, err
			}
			return p, uuid.Nil, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceReviewDuePayload)
			_, _, _, or, _, err := currentLicence(ctx, tx, p.LicenceID)
			if err != nil {
				return err
			}
			if _, err := jurisdiction.ClassifyReviewDueChange(or, parseDatePtr(p.ReviewDueAt)); err != nil {
				return &DomainError{Err: err}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceReviewDuePayload)
			ch, err := jurisdiction.SetLicenceReviewDue(ctx, tx, p.LicenceID, parseDatePtr(p.ReviewDueAt), ex.jurisdictionActor())
			if err != nil {
				return nil, err
			}
			return licenceOutcome(ch), nil
		},
	})

	register(KindLicenceEvidenceReferenceChange, kindSpec{
		platformOnly: true, platformPerm: auth.PermJurisdictionRegistryManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceEvidenceReferencePayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "licence_id", "licence_evidence_reference"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.LicenceID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
			}
			if p.Reference != nil {
				s := *p.Reference
				// The same rules as jurisdiction.validateEvidenceReference; the database classifier also bounds it at 200 octets.
				if s == "" || s != strings.TrimSpace(s) || len(s) > 200 || strings.Contains(s, "@") || strings.IndexFunc(s, unicode.IsControl) >= 0 {
					return nil, uuid.Nil, fmt.Errorf("%w: licence_evidence_reference must be an opaque, trimmed reference of 1..200 bytes without control characters or contact details", ErrInvalidInput)
				}
			}
			return p, uuid.Nil, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceEvidenceReferencePayload)
			_, _, _, _, cur, err := currentLicence(ctx, tx, p.LicenceID)
			if err != nil {
				return err
			}
			if (p.Reference == nil && cur == "") || (p.Reference != nil && *p.Reference == cur) {
				return &DomainError{Err: fmt.Errorf("%w: evidence_reference is unchanged", jurisdiction.ErrInvalidInput)}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceEvidenceReferencePayload)
			ch, err := jurisdiction.SetLicenceEvidenceReference(ctx, tx, p.LicenceID, p.Reference, ex.jurisdictionActor())
			if err != nil {
				return nil, err
			}
			return licenceOutcome(ch), nil
		},
	})
}
