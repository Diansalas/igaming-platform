package payoutinstrument

import (
	"crypto/hmac"
	"errors"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ADR 0111 4.3 (S-5; ADR 0110 T10): the Go seal of a payment statement import.
//
// The seal is HMAC-SHA256 under the "b13-import-v1" HKDF subkey of the active
// master kid over statement.ImportCanon(in) (domain tag "imp", every import
// column the verdict depends on, and lines_digest over every line column it
// reads). It is computed BEFORE the INSERT and written by it (imports and lines
// are immutable; there is no UPDATE path). Retired kids stay verify-only for as
// long as the process holds them. The key never enters the database, so the
// database sees only "sealed or not"; whether a seal VERIFIES is decided here,
// by the M4 executor, before the request commits and again at execution.
//
// What a seal proves: what this process fetched and stored, unchanged since.
// What it does NOT prove (T10 residuals, folded into D-7): that the source is
// genuine or complete (S-3, PROVIDER DEPENDENT), nor anything if the
// application host itself is compromised (T6).

// ErrImportSealInvalid: an import is unsealed, its kid is unknown, or the seal
// does not verify over the stored import and lines. Fail closed.
var ErrImportSealInvalid = errors.New("payoutinstrument: statement import seal does not verify")

// SealStatementImport seals in under the active master kid's import subkey
// (statement.ImportSealer).
func (k *Keys) SealStatementImport(in statement.ImportSealInput) (seal, kid string, err error) {
	if k == nil {
		return "", "", ErrKeyConfig
	}
	key, ok := k.imp[k.masterActive]
	if !ok {
		return "", "", ErrUnknownKID
	}
	return macHex(key, statement.ImportCanon(in)), k.masterActive, nil
}

// VerifyStatementImport checks a stored seal in constant time. A nil Keys, an
// empty seal or an unknown (never held) kid is ErrImportSealInvalid.
func (k *Keys) VerifyStatementImport(in statement.ImportSealInput, kid, seal string) error {
	if k == nil || seal == "" || kid == "" {
		return ErrImportSealInvalid
	}
	key, ok := k.imp[kid]
	if !ok {
		return ErrImportSealInvalid
	}
	if !hmac.Equal([]byte(macHex(key, statement.ImportCanon(in))), []byte(seal)) {
		return ErrImportSealInvalid
	}
	return nil
}
