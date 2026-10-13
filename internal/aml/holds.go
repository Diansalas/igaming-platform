package aml

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// PlaceHoldInput is an officer's request to place a hold on a person.
type PlaceHoldInput struct {
	PersonID uuid.UUID
	// PlayerAccountID narrows the hold to one account; nil holds the person across the tenant.
	PlayerAccountID *uuid.UUID
	Scope           HoldScope
	ReasonClass     ReasonClass
	ReasonCode      string
}

func (in PlaceHoldInput) validate() error {
	if in.PersonID == uuid.Nil {
		return fmt.Errorf("%w: person id is required", ErrInvalidInput)
	}
	if in.PlayerAccountID != nil && *in.PlayerAccountID == uuid.Nil {
		return fmt.Errorf("%w: player account id is nil", ErrInvalidInput)
	}
	switch in.Scope {
	case ScopeAllValueMovement, ScopeWithdrawalAndPayout:
	default:
		return fmt.Errorf("%w: hold scope %q", ErrInvalidInput, in.Scope)
	}
	switch in.ReasonClass {
	case ReasonManual, ReasonConfirmedSanctions:
	default:
		return fmt.Errorf("%w: reason class %q", ErrInvalidInput, in.ReasonClass)
	}
	if in.ReasonClass == ReasonConfirmedSanctions && in.Scope != ScopeAllValueMovement {
		return fmt.Errorf("%w: a confirmed sanctions hold gates all value movement", ErrInvalidInput)
	}
	if !ValidReasonCode(in.ReasonCode) {
		return fmt.Errorf("%w: reason code", ErrInvalidInput)
	}
	return nil
}

// PlaceHold places a hold. The database requires the actor's in-force aml_officer grant, a proof bound to exactly these
// arguments, the officer quorum of the tenant's owner scope (ErrQuorum otherwise) and that the actor owns that scope.
// It returns the new hold's id. The hold is audited by its own append-only row (actor, scope, person, time); it is
// deliberately NOT written to audit_log, which is tenant-readable (ADR 0116 s5.2).
func (s *Service) PlaceHold(ctx context.Context, a Actor, in PlaceHoldInput) (uuid.UUID, error) {
	if err := in.validate(); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(in.PersonID.String()), actorproof.UUIDP(in.PlayerAccountID),
			actorproof.S(string(in.Scope)), actorproof.S(string(in.ReasonClass)), actorproof.S(in.ReasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofHoldPlace, actorproof.TargetNew, hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT aml_hold_place($1, $2, $3, $4, $5, $6)`,
			a.TenantID, in.PersonID, in.PlayerAccountID, string(in.Scope), string(in.ReasonClass), in.ReasonCode).Scan(&id)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// LiftHold lifts a `manual` hold. A confirmed sanctions hold is refused here (ErrHoldRefused): it is lifted only by the
// proof-bound four-eyes approval of a case (Service.ApproveCaseAction).
func (s *Service) LiftHold(ctx context.Context, a Actor, holdID uuid.UUID, reasonCode string) (uuid.UUID, error) {
	if holdID == uuid.Nil || !ValidReasonCode(reasonCode) {
		return uuid.Nil, fmt.Errorf("%w: hold id and reason code are required", ErrInvalidInput)
	}
	var id uuid.UUID
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(holdID.String()), actorproof.S(reasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofHoldLift, holdID.String(), hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT aml_hold_lift($1, $2, $3)`, a.TenantID, holdID, reasonCode).Scan(&id)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Hold is one row of ListHolds.
type Hold struct {
	ID              uuid.UUID
	OwnerScope      string
	PlayerAccountID *uuid.UUID
	Scope           HoldScope
	ReasonClass     ReasonClass
	ReasonCode      string
	PlacedAt        time.Time
	Lifted          bool
	LiftedAt        *time.Time
}

// ListHolds returns the holds of one person that the actor's scope may read.
func (s *Service) ListHolds(ctx context.Context, a Actor, personID uuid.UUID) ([]Hold, error) {
	if personID == uuid.Nil {
		return nil, fmt.Errorf("%w: person id is required", ErrInvalidInput)
	}
	var out []Hold
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(personID.String()))
		if err := s.attachProof(ctx, tx, a, OpProofHoldRead, personID.String(), hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT hold_id, owner_scope, player_account_id, hold_scope, reason_class, reason_code, placed_at, lifted, lifted_at
			FROM aml_hold_list($1, $2)`, a.TenantID, personID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Hold
			var scope, class string
			if err := rows.Scan(&h.ID, &h.OwnerScope, &h.PlayerAccountID, &scope, &class, &h.ReasonCode, &h.PlacedAt, &h.Lifted, &h.LiftedAt); err != nil {
				return err
			}
			h.Scope, h.ReasonClass = HoldScope(scope), ReasonClass(class)
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
