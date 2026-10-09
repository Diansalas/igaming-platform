package payoutinstrument

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State is the instrument verification state (ADR 0111 2.1).
type State string

// Instrument states.
const (
	StatePendingVerification State = "pending_verification"
	StateVerified            State = "verified"
	StateVerificationExpired State = "verification_expired"
	StateSuspended           State = "suspended"
	StateRevoked             State = "revoked"
	StateRejected            State = "rejected"
	StateSuperseded          State = "superseded"
)

// Terminal reports whether s is terminal in every session.
func (s State) Terminal() bool {
	return s == StateRevoked || s == StateRejected || s == StateSuperseded
}

// VerificationSource is the closed source set (A-4).
type VerificationSource string

// Verification sources.
const (
	SourcePSPAccount   VerificationSource = "psp_account_verification"
	SourceKYCVendor    VerificationSource = "kyc_vendor_instrument_verification"
	SourceCustodian    VerificationSource = "custodian_address_verification"
	SourceSynthetic    VerificationSource = "synthetic"
	OwnershipVerified                     = "account_holder_matches_verified_identity"
	OwnershipSynthetic                    = "synthetic_asserted"
)

// IsSynthetic reports whether the source is the synthetic one.
func (s VerificationSource) IsSynthetic() bool { return s == SourceSynthetic }

// Valid reports whether s is in the closed set.
func (s VerificationSource) Valid() bool {
	switch s {
	case SourcePSPAccount, SourceKYCVendor, SourceCustodian, SourceSynthetic:
		return true
	}
	return false
}

// OwnershipFor is the ownership assertion that goes with a source:
// (source = synthetic) = (ownership_assertion = synthetic_asserted).
func OwnershipFor(s VerificationSource) string {
	if s.IsSynthetic() {
		return OwnershipSynthetic
	}
	return OwnershipVerified
}

// Blocking events and actor types.
const (
	EventSuspend = "suspend"
	EventRevoke  = "revoke"

	ActorPlayer   = "player"
	ActorStaff    = "staff"
	ActorProvider = "provider"
)

// Instrument is one payout_instruments row. It carries ciphertext, never
// plaintext detail; the fingerprint is internal and is never serialised by any
// API type (views are separate).
type Instrument struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	BrandID               uuid.UUID
	PlayerAccountID       uuid.UUID
	PersonID              uuid.UUID
	Kind                  string
	Rail                  string
	AssetCodes            []string
	DetailCiphertext      []byte
	DetailNonce           []byte
	DetailKeyKID          string
	DetailSchemaVersion   int
	DisplayMask           string
	Fingerprint           string
	FingerprintKID        string
	SupersedesInstrument  *uuid.UUID
	InstrumentSeal        string
	SealKID               string
	State                 State
	CurrentVerificationID *uuid.UUID
	CreatedAt             time.Time
	StateChangedAt        time.Time
}

// String never renders ciphertext or the fingerprint.
func (i Instrument) String() string {
	return fmt.Sprintf("Instrument{id=%s kind=%s state=%s}", i.ID, i.Kind, i.State)
}

// GoString redacts like String.
func (i Instrument) GoString() string { return i.String() }

// Format redacts under every verb.
func (i Instrument) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(i.String())) }

// Verification is one payout_instrument_verifications row.
type Verification struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	InstrumentID          uuid.UUID
	Source                VerificationSource
	OwnershipAssertion    string
	VerifierProviderID    string
	VerifierReferenceHash *string
	Outcome               string // "verified" | "rejected"
	VerifiedAt            time.Time
	ExpiresAt             time.Time
	Seal                  string
	SealKID               string
}

// BlockingEvent is one payout_instrument_blocking_events row.
type BlockingEvent struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	InstrumentID uuid.UUID
	Event        string
	ActorType    string
	ActorID      string
	ReasonCode   string
	OccurredAt   time.Time
	Seal         string
	SealKID      string
}

// Snapshot is one payout_attempt_destination_snapshots row (written by B13-B's
// T1p through Service.WriteSnapshot; the database makes it write-once).
type Snapshot struct {
	AttemptID             uuid.UUID
	TenantID              uuid.UUID
	WithdrawalRequestID   uuid.UUID
	InstrumentID          uuid.UUID
	Kind                  string
	Rail                  string
	Fingerprint           string
	FingerprintKID        string
	VerificationID        uuid.UUID
	VerificationSource    VerificationSource
	OwnershipAssertion    string
	VerifiedAt            time.Time
	VerificationExpiresAt time.Time
	DisplayMask           string
	Amount                string // canonical decimal minor units
	AssetCode             string
	Seal                  string
	SealKID               string
}

// String never renders the fingerprint.
func (s Snapshot) String() string {
	return fmt.Sprintf("Snapshot{attempt=%s instrument=%s kind=%s}", s.AttemptID, s.InstrumentID, s.Kind)
}

// GoString redacts like String.
func (s Snapshot) GoString() string { return s.String() }

// Format redacts under every verb.
func (s Snapshot) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }
