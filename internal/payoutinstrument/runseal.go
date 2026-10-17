package payoutinstrument

import (
	"crypto/hmac"
	"errors"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 / SRF2-1 (migration 0168): the Go seal of a payment_statement RUN row.
//
// HMAC-SHA256 under the "b13-run-v1" HKDF subkey of the active master kid over statement.RunCanon(in) (domain tag
// "run": run id, tenant, provider, stream, the import the run matched, its coverage window, window_truncated and
// run_at). Written by the match transaction in the run's own INSERT; the key never enters the database. The
// payment_statement cursor and the M2 reconciliation-currency gate trust a run row only when this seal verifies
// (and the import it names verifies too), so a run row written by anything other than this process (a SQL-only
// forger) is never a cursor and never M2 evidence.

// ErrRunSealInvalid: a run row is unsealed, its kid is unknown, or the seal does not verify. Fail closed.
var ErrRunSealInvalid = errors.New("payoutinstrument: payment statement run seal does not verify")

// SealPaymentRun seals in under the active master kid's run subkey (statement.RunSealer).
func (k *Keys) SealPaymentRun(in statement.RunSealInput) (seal, kid string, err error) {
	if k == nil {
		return "", "", ErrKeyConfig
	}
	key, ok := k.run[k.masterActive]
	if !ok {
		return "", "", ErrUnknownKID
	}
	return macHex(key, statement.RunCanon(in)), k.masterActive, nil
}

// VerifyPaymentRun checks a stored run seal in constant time. A nil Keys, an empty seal or an unknown kid is
// ErrRunSealInvalid.
func (k *Keys) VerifyPaymentRun(in statement.RunSealInput, kid, seal string) error {
	if k == nil || seal == "" || kid == "" {
		return ErrRunSealInvalid
	}
	key, ok := k.run[kid]
	if !ok {
		return ErrRunSealInvalid
	}
	if !hmac.Equal([]byte(macHex(key, statement.RunCanon(in))), []byte(seal)) {
		return ErrRunSealInvalid
	}
	return nil
}
