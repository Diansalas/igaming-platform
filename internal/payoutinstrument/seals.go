package payoutinstrument

import (
	"errors"
)

// ErrSealInvalid: a stored seal does not verify (tamper, wrong key, unknown
// kid or an unsealed row). Fail closed.
var ErrSealInvalid = errors.New("payoutinstrument: seal does not verify")

// ADR 0111 2.2 seals. Each is HMAC-SHA256 under the HKDF seal subkey of the
// row's seal_kid over the canonical (length-prefixed) encoding below. The
// column lists are the ADR's, plus the additions recorded in the ADR 0111
// appendix (B13A-5): display_mask / detail_key_kid / detail_schema_version on
// the instrument seal, the verifier reference hash on the verification seal,
// actor_id / reason_code on the blocking-event seal, and verified_at /
// display_mask on the snapshot seal. They only widen coverage.

func instrumentCanon(i *Instrument) string {
	return canon(cs("inst"), cu(i.ID), cu(i.TenantID), cu(i.BrandID), cu(i.PlayerAccountID), cu(i.PersonID),
		cs(i.Kind), cs(i.Rail), carr(i.AssetCodes), cs(i.Fingerprint), cs(i.FingerprintKID), cup(i.SupersedesInstrument),
		cs(i.DisplayMask), cs(i.DetailKeyKID), cs(itoa(i.DetailSchemaVersion)))
}

func verificationCanon(v *Verification, instrumentFingerprint string) string {
	var ref *string
	if v.VerifierReferenceHash != nil {
		ref = v.VerifierReferenceHash
	}
	return canon(cs("ver"), cu(v.ID), cu(v.TenantID), cu(v.InstrumentID), cs(instrumentFingerprint),
		cs(string(v.Source)), cs(v.OwnershipAssertion), cs(v.Outcome), cs(v.VerifierProviderID),
		ct(v.VerifiedAt), ct(v.ExpiresAt), ref)
}

func blockingCanon(e *BlockingEvent) string {
	return canon(cs("blk"), cu(e.ID), cu(e.TenantID), cu(e.InstrumentID), cs(e.Event), cs(e.ActorType),
		ct(e.OccurredAt), cs(e.ActorID), cs(e.ReasonCode))
}

func snapshotCanon(s *Snapshot) string {
	return canon(cs("snap"), cu(s.AttemptID), cu(s.TenantID), cu(s.WithdrawalRequestID), cu(s.InstrumentID),
		cs(s.Fingerprint), cs(s.FingerprintKID), cs(s.Kind), cs(s.Rail), cu(s.VerificationID),
		cs(string(s.VerificationSource)), cs(s.OwnershipAssertion), ct(s.VerificationExpiresAt),
		cs(s.Amount), cs(s.AssetCode), ct(s.VerifiedAt), cs(s.DisplayMask))
}

// SealInstrument computes and stores the seal (active master kid).
func (k *Keys) SealInstrument(i *Instrument) error {
	m, err := k.sealMAC(k.masterActive, instrumentCanon(i))
	if err != nil {
		return err
	}
	i.InstrumentSeal, i.SealKID = m, k.masterActive
	return nil
}

// VerifyInstrument checks the instrument seal.
func (k *Keys) VerifyInstrument(i *Instrument) error {
	if !k.sealEqual(i.SealKID, instrumentCanon(i), i.InstrumentSeal) {
		return ErrSealInvalid
	}
	return nil
}

// SealVerification seals v; instrumentFingerprint is the instrument's.
func (k *Keys) SealVerification(v *Verification, instrumentFingerprint string) error {
	m, err := k.sealMAC(k.masterActive, verificationCanon(v, instrumentFingerprint))
	if err != nil {
		return err
	}
	v.Seal, v.SealKID = m, k.masterActive
	return nil
}

// VerifyVerification checks the verification seal.
func (k *Keys) VerifyVerification(v *Verification, instrumentFingerprint string) error {
	if !k.sealEqual(v.SealKID, verificationCanon(v, instrumentFingerprint), v.Seal) {
		return ErrSealInvalid
	}
	return nil
}

// SealBlockingEvent seals e.
func (k *Keys) SealBlockingEvent(e *BlockingEvent) error {
	m, err := k.sealMAC(k.masterActive, blockingCanon(e))
	if err != nil {
		return err
	}
	e.Seal, e.SealKID = m, k.masterActive
	return nil
}

// VerifyBlockingEvent checks the blocking-event seal.
func (k *Keys) VerifyBlockingEvent(e *BlockingEvent) error {
	if !k.sealEqual(e.SealKID, blockingCanon(e), e.Seal) {
		return ErrSealInvalid
	}
	return nil
}

// SealSnapshot seals s.
func (k *Keys) SealSnapshot(s *Snapshot) error {
	m, err := k.sealMAC(k.masterActive, snapshotCanon(s))
	if err != nil {
		return err
	}
	s.Seal, s.SealKID = m, k.masterActive
	return nil
}

// VerifySnapshot checks the snapshot seal.
func (k *Keys) VerifySnapshot(s *Snapshot) error {
	if !k.sealEqual(s.SealKID, snapshotCanon(s), s.Seal) {
		return ErrSealInvalid
	}
	return nil
}
