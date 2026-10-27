// Package kycpolicyproof signs the ADR 0110 actor proofs of the KYC enforcement policy writes (lic19, migration 0185, S4):
// create, activate, withdraw and supersede of a kyc_enforcement_policies row. Scope platform (NULL tenant); the operation family
// is closed in lockstep with actor_proof_require, actorproof.StepUpOperations (all critical) and the claim validation.
//
// It is a pinned signer owner (security review mandatory): it signs only for the platform-admin principal the caller passes,
// which kyc takes from the platform-admin transaction itself (kyc depends on this leaf package for the types; it does not import the issuer itself). The database recomputes every hash from the row (and the
// transaction-local reason code) and verifies and consumes the proof in the same transaction; nothing here is trusted.
package kycpolicyproof

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// PolicyContent is the content a creation proof is bound to (every caller-supplied column of the draft row).
type PolicyContent struct {
	LicensingJurisdictionID uuid.UUID
	TriggerType             string
	ThresholdMinorUnits     *string
	AssetCode               *string
	RequiredTier            *string
	PlayOperation           *string
	LegalReviewReference    string
	ReasonCode              string
}

// PolicyTransition is the status change a transition proof is bound to.
type PolicyTransition struct {
	ID         uuid.UUID
	From, To   string
	Successor  *uuid.UUID
	ReasonCode string
}

// ErrNoIssuer: no actor-proof issuer is configured (the write fails closed).
var ErrNoIssuer = errors.New("kycpolicyproof: no actor-proof issuer configured (policy writes fail closed)")

// Signer implements kyc.PolicyProof (asserted in the external test). A nil Issuer means actorproof.Default() (nil there fails closed).
type Signer struct{ Issuer *actorproof.Issuer }

func (s Signer) issuer() (*actorproof.Issuer, error) {
	if s.Issuer != nil {
		return s.Issuer, nil
	}
	if d := actorproof.Default(); d != nil {
		return d, nil
	}
	return nil, ErrNoIssuer
}

// CreateHash is the digest the creation proof is bound to; it mirrors the SQL k2_sha256_hex(k2_canonical(...)) of the
// kyc_enforcement_policies_require_create_proof trigger.
func CreateHash(c PolicyContent) string {
	return actorproof.Digest(actorproof.S(c.LicensingJurisdictionID.String()), actorproof.S(c.TriggerType), c.ThresholdMinorUnits, c.AssetCode,
		c.RequiredTier, c.PlayOperation, actorproof.SP(c.LegalReviewReference), actorproof.S(c.ReasonCode))
}

// TransitionHash mirrors the lifecycle trigger's digest of (id, from, to, successor, reason code).
func TransitionHash(t PolicyTransition) string {
	return actorproof.Digest(actorproof.S(t.ID.String()), actorproof.S(t.From), actorproof.S(t.To), actorproof.UUIDP(t.Successor), actorproof.S(t.ReasonCode))
}

// TransitionOperation maps a status change to its proof operation (mirrors the trigger): to active is an activation; a draft
// withdrawn is a withdrawal; an active withdrawn (always naming a successor) is a supersession.
func TransitionOperation(from, to string) (string, error) {
	switch {
	case to == "active" && from == "draft":
		return actorproof.OpKYCPolicyActivate, nil
	case to == "withdrawn" && from == "draft":
		return actorproof.OpKYCPolicyWithdraw, nil
	case to == "withdrawn" && from == "active":
		return actorproof.OpKYCPolicySupersede, nil
	}
	return "", fmt.Errorf("kycpolicyproof: no proof operation for %s -> %s", from, to)
}

// AttachCreate signs and attaches the creation proof.
func (s Signer) AttachCreate(ctx context.Context, tx pgx.Tx, actor uuid.UUID, c PolicyContent) error {
	iss, err := s.issuer()
	if err != nil {
		return err
	}
	return iss.Attach(ctx, tx, actorproof.Claims{Actor: actor, Scope: actorproof.ScopePlatform, Operation: actorproof.OpKYCPolicyCreate,
		Target: actorproof.TargetNew, PayloadHash: CreateHash(c)})
}

// AttachTransition signs and attaches the proof of a status change.
func (s Signer) AttachTransition(ctx context.Context, tx pgx.Tx, actor uuid.UUID, t PolicyTransition) error {
	iss, err := s.issuer()
	if err != nil {
		return err
	}
	op, err := TransitionOperation(t.From, t.To)
	if err != nil {
		return err
	}
	return iss.Attach(ctx, tx, actorproof.Claims{Actor: actor, Scope: actorproof.ScopePlatform, Operation: op, Target: t.ID.String(),
		PayloadHash: TransitionHash(t)})
}
