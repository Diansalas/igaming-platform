package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Closed gate refusal reasons (ADR 0111 2.4: `destination_not_usable` vs
// `destination_integrity_failure` are decided by Integrity()).
const (
	ReasonNotFound            = "instrument_not_found"
	ReasonRelationMismatch    = "relation_mismatch"
	ReasonNotVerified         = "state_not_verified"
	ReasonAssetNotListed      = "asset_not_listed"
	ReasonSealInvalid         = "seal_invalid"
	ReasonFingerprintMismatch = "fingerprint_mismatch"
	ReasonVerificationMissing = "verification_missing"
	ReasonVerificationNotLast = "verification_not_latest"
	ReasonVerificationExpired = "verification_expired"
	ReasonRevoked             = "blocked_revoked"
	ReasonSuspended           = "blocked_suspended"
	ReasonTierRefused         = "tier_refused"
	ReasonVerifierNotRegd     = "verifier_not_registered"
	ReasonKindDisabled        = "kind_disabled"
	ReasonDetailUnavailable   = "detail_unavailable"
)

// GateRefusal is a closed-reason refusal of the gate rule. Reason is safe for
// audit and for a closed escalation vocabulary; it never carries a value.
type GateRefusal struct {
	Reason    string
	integrity bool
}

func (g *GateRefusal) Error() string {
	return "payoutinstrument: destination gate refused: " + g.Reason
}

// Integrity reports whether the refusal is an integrity failure (a seal,
// decrypt or fingerprint failure, or a relational inconsistency) rather than
// an ordinary "not usable" state. Integrity failures escalate as
// destination_integrity_failure.
func (g *GateRefusal) Integrity() bool { return g.integrity }

func refuse(reason string, integrity bool) error {
	return &GateRefusal{Reason: reason, integrity: integrity}
}

// IsGateRefusal extracts a GateRefusal.
func IsGateRefusal(err error) (*GateRefusal, bool) {
	var g *GateRefusal
	if errors.As(err, &g) {
		return g, true
	}
	return nil, false
}

// GateParams is one application of the gate rule.
type GateParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID
	InstrumentID    uuid.UUID
	AssetCode       string
	// Adapter is the EXACT payment adapter value about to be (or already)
	// used; the tiering predicate keys only on its Go type marker. Nil means
	// "no adapter involved" (request creation): the tiering predicate is then
	// not applied, every other rule is.
	Adapter any
	// Lock takes the instrument FOR SHARE (request creation and T1p). Revocation
	// locks FOR UPDATE and takes no withdrawal lock, so there is no cycle.
	Lock bool
	// WantDetail returns the decrypted detail (phase B builds PayoutDestination
	// from the snapshot plus this).
	WantDetail bool
}

// GateResult is what a passing gate yields.
type GateResult struct {
	Instrument   Instrument
	Verification Verification
	// Detail is the decrypted, kind-validated detail JSON (only when
	// WantDetail). The caller must treat it as secret.
	Detail []byte
}

// EvaluateGate applies the ADR 0111 2.2 gate rule, in Go, at every gate
// (request creation, T1p, phase B, T2/T12). It refuses unless:
//
//	(1) all seals verify and the fingerprint recomputed from the decrypted
//	    detail equals the stored one;
//	(2) the in-force verification is the LATEST verified row of the instrument
//	    and expires_at > now();
//	(3) no revoke event exists;
//	(4) no suspend event is later than that verification's verified_at;
//	(5) relational consistency: instrument tenant/brand/player/person = the
//	    caller's, and the asset is listed;
//	(6) when the adapter is non-Synthetic, verifier_provider_id names a
//	    currently registered non-Synthetic verifier;
//
// plus the tiering predicate on Adapter. Every refusal has a closed reason.
func (s *Service) EvaluateGate(ctx context.Context, tx pgx.Tx, p GateParams) (GateResult, error) {
	lock := lockNone
	if p.Lock {
		lock = lockShare
	}
	inst, err := loadInstrument(ctx, tx, p.TenantID, p.InstrumentID, lock)
	if errors.Is(err, ErrNotFound) {
		return GateResult{}, refuse(ReasonNotFound, false)
	}
	if err != nil {
		return GateResult{}, err
	}
	// (5) relational.
	if inst.TenantID != p.TenantID || inst.BrandID != p.BrandID || inst.PlayerAccountID != p.PlayerAccountID || inst.PersonID != p.PersonID {
		return GateResult{}, refuse(ReasonRelationMismatch, true)
	}
	// (1) seals + AEAD + fingerprint recomputation.
	detail, ierr := s.checkIntegrity(&inst)
	if ierr != nil {
		reason := ReasonSealInvalid
		var ie *integrityError
		if errors.As(ierr, &ie) {
			switch ie.reason {
			case "decrypt":
				reason = ReasonDetailUnavailable
			case "fingerprint", "mask", "detail":
				reason = ReasonFingerprintMismatch
			}
		}
		return GateResult{}, refuse(reason, true)
	}
	if inst.State != StateVerified {
		return GateResult{}, refuse(ReasonNotVerified, false)
	}
	if !contains(inst.AssetCodes, p.AssetCode) {
		return GateResult{}, refuse(ReasonAssetNotListed, false)
	}
	// (2) latest verification, in force.
	if inst.CurrentVerificationID == nil {
		return GateResult{}, refuse(ReasonVerificationMissing, true)
	}
	latest, ok, err := latestVerified(ctx, tx, p.TenantID, inst.ID)
	if err != nil {
		return GateResult{}, err
	}
	if !ok {
		return GateResult{}, refuse(ReasonVerificationMissing, true)
	}
	if latest.ID != *inst.CurrentVerificationID {
		return GateResult{}, refuse(ReasonVerificationNotLast, true)
	}
	if err := s.keys.VerifyVerification(&latest, inst.Fingerprint); err != nil {
		return GateResult{}, refuse(ReasonSealInvalid, true)
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		return GateResult{}, fmt.Errorf("payoutinstrument: gate time: %w", err)
	}
	if !latest.ExpiresAt.After(dbNow) {
		return GateResult{}, refuse(ReasonVerificationExpired, false)
	}
	// (3), (4) blocking events, each sealed.
	events, err := loadBlockingEvents(ctx, tx, p.TenantID, inst.ID)
	if err != nil {
		return GateResult{}, err
	}
	for k := range events {
		e := &events[k]
		if err := s.keys.VerifyBlockingEvent(e); err != nil {
			return GateResult{}, refuse(ReasonSealInvalid, true)
		}
		if e.Event == EventRevoke {
			return GateResult{}, refuse(ReasonRevoked, false)
		}
		if e.Event == EventSuspend && e.OccurredAt.After(latest.VerifiedAt) {
			return GateResult{}, refuse(ReasonSuspended, false)
		}
	}
	// (6) + tiering.
	if p.Adapter != nil {
		var kindEnabled bool
		if err := tx.QueryRow(ctx, `SELECT non_synthetic_enabled FROM payout_instrument_kinds WHERE code = $1`, inst.Kind).Scan(&kindEnabled); err != nil {
			return GateResult{}, fmt.Errorf("payoutinstrument: gate kind: %w", err)
		}
		v, regd := s.verifierByID(latest.VerifierProviderID)
		in := TierInput{
			Bound: true, Source: latest.Source, KindNonSyntheticEnabled: kindEnabled, SealsValid: true,
			VerifierRegisteredNonSynthetic: regd && !IsSyntheticComponent(v),
		}
		if !IsSyntheticComponent(p.Adapter) {
			if !regd || IsSyntheticComponent(v) {
				return GateResult{}, refuse(ReasonVerifierNotRegd, false)
			}
			if !kindEnabled {
				return GateResult{}, refuse(ReasonKindDisabled, false)
			}
		}
		if err := CheckTier(p.Adapter, in); err != nil {
			return GateResult{}, refuse(ReasonTierRefused, false)
		}
	}
	res := GateResult{Instrument: inst, Verification: latest}
	if p.WantDetail {
		res.Detail = detail
	}
	return res, nil
}

// Destination builds the adapter-facing PayoutDestination from a passing gate
// (phase B). The caller supplies the kid recorded in the snapshot.
func (r GateResult) Destination(fingerprintKid string) PayoutDestination {
	return PayoutDestination{InstrumentID: r.Instrument.ID, Kind: r.Instrument.Kind, Detail: append([]byte(nil), r.Detail...), FingerprintKid: fingerprintKid}
}

// ---- snapshot --------------------------------------------------------------

// SnapshotParams is what T1p supplies after InsertSubmittingAttempt.
type SnapshotParams struct {
	AttemptID           uuid.UUID
	WithdrawalRequestID uuid.UUID
	Amount              string // canonical decimal minor units
	AssetCode           string
}

// WriteSnapshot seals and inserts the write-once destination snapshot for a
// payout attempt from a passing gate result. It is the ONLY snapshot writer
// (a static test pins the call site list); the database makes it write-once
// and checks it equals the withdrawal, the attempt, the instrument and the
// verification.
func (s *Service) WriteSnapshot(ctx context.Context, tx pgx.Tx, g GateResult, p SnapshotParams) (Snapshot, error) {
	snap := Snapshot{
		AttemptID: p.AttemptID, TenantID: g.Instrument.TenantID, WithdrawalRequestID: p.WithdrawalRequestID,
		InstrumentID: g.Instrument.ID, Kind: g.Instrument.Kind, Rail: g.Instrument.Rail,
		Fingerprint: g.Instrument.Fingerprint, FingerprintKID: g.Instrument.FingerprintKID,
		VerificationID: g.Verification.ID, VerificationSource: g.Verification.Source,
		OwnershipAssertion: g.Verification.OwnershipAssertion, VerifiedAt: g.Verification.VerifiedAt,
		VerificationExpiresAt: g.Verification.ExpiresAt, DisplayMask: g.Instrument.DisplayMask,
		Amount: p.Amount, AssetCode: p.AssetCode,
	}
	if err := s.keys.SealSnapshot(&snap); err != nil {
		return Snapshot{}, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO payout_attempt_destination_snapshots
		(attempt_id, tenant_id, withdrawal_request_id, instrument_id, kind, rail, fingerprint, fingerprint_kid, verification_id,
		 verification_source, ownership_assertion, verified_at, verification_expires_at, display_mask, amount, asset_code,
		 snapshot_seal, seal_kid, created_txid)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::numeric,$16,$17,$18,0)`,
		snap.AttemptID, snap.TenantID, snap.WithdrawalRequestID, snap.InstrumentID, snap.Kind, snap.Rail, snap.Fingerprint,
		snap.FingerprintKID, snap.VerificationID, string(snap.VerificationSource), snap.OwnershipAssertion, snap.VerifiedAt,
		snap.VerificationExpiresAt, snap.DisplayMask, snap.Amount, snap.AssetCode, snap.Seal, snap.SealKID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("payoutinstrument: insert snapshot: %w", err)
	}
	return snap, nil
}

// LoadSnapshot reads and seal-verifies the attempt's snapshot.
func (s *Service) LoadSnapshot(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Snapshot, error) {
	var snap Snapshot
	var src string
	err := tx.QueryRow(ctx, `SELECT attempt_id, tenant_id, withdrawal_request_id, instrument_id, kind, rail, fingerprint, fingerprint_kid,
		verification_id, verification_source, ownership_assertion, verified_at, verification_expires_at, display_mask, amount::text,
		asset_code, snapshot_seal, seal_kid FROM payout_attempt_destination_snapshots WHERE attempt_id = $1 AND tenant_id = $2`,
		attemptID, tenantID).Scan(&snap.AttemptID, &snap.TenantID, &snap.WithdrawalRequestID, &snap.InstrumentID, &snap.Kind, &snap.Rail,
		&snap.Fingerprint, &snap.FingerprintKID, &snap.VerificationID, &src, &snap.OwnershipAssertion, &snap.VerifiedAt,
		&snap.VerificationExpiresAt, &snap.DisplayMask, &snap.Amount, &snap.AssetCode, &snap.Seal, &snap.SealKID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("payoutinstrument: load snapshot: %w", err)
	}
	snap.VerificationSource = VerificationSource(src)
	if err := s.keys.VerifySnapshot(&snap); err != nil {
		return Snapshot{}, ErrSealInvalid
	}
	return snap, nil
}
