package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// B13-B helpers (ADR 0111 section 16): what the withdrawal and payments paths
// need around EvaluateGate / CheckTier that is not part of the gate rule itself.

// Closed reasons added by B13-B. All are integrity failures: a bound
// withdrawal whose snapshot is absent, unsealed, or inconsistent with the
// withdrawal / attempt / instrument is never "just not usable" (decision 4:
// missing or ambiguous attribution fails closed).
const (
	ReasonSnapshotMissing  = "snapshot_missing"
	ReasonSnapshotMismatch = "snapshot_mismatch"
	ReasonBindingMismatch  = "binding_mismatch"
	ReasonNoGate           = "destination_gate_unavailable"
)

// RefuseIntegrity builds an integrity GateRefusal for a caller-side relational
// failure (for example the withdrawal's stored fingerprint differs from the
// instrument's). The reason must be one of the closed Reason* values.
func RefuseIntegrity(reason string) error { return refuse(reason, true) }

// RefuseNotUsable builds a non-integrity GateRefusal (destination_not_usable).
func RefuseNotUsable(reason string) error { return refuse(reason, false) }

// RailOf returns the instrument's rail for routing. It is a plain read with NO
// integrity or usability decision: the destination gate, applied inside the
// claim transaction, is the safeguard. The rail replaces the staff-supplied
// payment_method (ADR 0111 2.4).
func (s *Service) RailOf(ctx context.Context, tx pgx.Tx, tenantID, instrumentID uuid.UUID) (string, error) {
	var rail string
	err := tx.QueryRow(ctx, `SELECT rail FROM payout_instruments WHERE id = $1 AND tenant_id = $2`, instrumentID, tenantID).Scan(&rail)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("payoutinstrument: rail: %w", err)
	}
	return rail, nil
}

// SnapshotExpect is what a loaded snapshot must equal.
type SnapshotExpect struct {
	TenantID            uuid.UUID
	AttemptID           uuid.UUID
	WithdrawalRequestID uuid.UUID
	InstrumentID        uuid.UUID
	Fingerprint         string
	Amount              int64
	AssetCode           string
}

// CheckSnapshot loads the attempt's write-once snapshot, verifies its seal and
// requires it to equal the withdrawal, the attempt and the bound instrument
// (ADR 0111 2.2 rule (5)). Every failure is an INTEGRITY refusal:
//
//	no snapshot            -> snapshot_missing
//	seal does not verify   -> seal_invalid
//	any field differs      -> snapshot_mismatch
//
// It never reads the instrument's CURRENT state: evidence on an attempt that
// may already have been sent is not blocked by a later suspension or
// revocation (ADR 0111 2.4, LF95-C10(d)); it is only refused when the
// destination record of that attempt cannot be trusted.
func (s *Service) CheckSnapshot(ctx context.Context, tx pgx.Tx, e SnapshotExpect) (Snapshot, error) {
	snap, err := s.LoadSnapshot(ctx, tx, e.TenantID, e.AttemptID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Snapshot{}, refuse(ReasonSnapshotMissing, true)
	case errors.Is(err, ErrSealInvalid):
		return Snapshot{}, refuse(ReasonSealInvalid, true)
	case err != nil:
		return Snapshot{}, err
	}
	if snap.TenantID != e.TenantID || snap.AttemptID != e.AttemptID || snap.WithdrawalRequestID != e.WithdrawalRequestID ||
		snap.InstrumentID != e.InstrumentID || snap.Fingerprint != e.Fingerprint ||
		snap.Amount != strconv.FormatInt(e.Amount, 10) || snap.AssetCode != e.AssetCode {
		return Snapshot{}, refuse(ReasonSnapshotMismatch, true)
	}
	return snap, nil
}

// EchoVerdict is the outcome of comparing a provider's destination echo with
// the attempt's snapshot (ADR 0111 2.6).
type EchoVerdict int

const (
	// EchoAbsent: the provider reported no destination evidence.
	EchoAbsent EchoVerdict = iota
	// EchoMatch: the echo equals the snapshot fingerprint under the snapshot kid.
	EchoMatch
	// EchoMismatch: the echo differs, or names an unknown/different kid, or is
	// malformed. A provider-reported destination can only ever be compared; it
	// never determines, replaces or changes the destination.
	EchoMismatch
)

// CompareEcho compares echo with the snapshot. An echo under a kid other than
// the snapshot's counts as a mismatch (A-8: an unknown kid is not "equal").
func CompareEcho(snap Snapshot, echo *DestinationEcho) EchoVerdict {
	if echo == nil {
		return EchoAbsent
	}
	if echo.Kid != snap.FingerprintKID || echo.Fingerprint == "" || echo.Fingerprint != snap.Fingerprint {
		return EchoMismatch
	}
	return EchoMatch
}
