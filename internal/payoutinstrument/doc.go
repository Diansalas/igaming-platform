// Package payoutinstrument is the provider-neutral PAYOUT INSTRUMENT subsystem
// (B13-A, ADR 0111 section 2, migration 0123; owner decisions ADR 0095 section
// 44 decisions 1-8).
//
// A payout instrument is the destination a player's withdrawal is paid to
// (bank account, tokenised card, e-wallet, ...). The platform owns the entity,
// its verification state machine, its encryption at rest, its tamper-evident
// seals and its fingerprint; vendors verify ownership behind the
// PayoutInstrumentVerifier interface and pay behind the PaymentProvider
// interface. Nothing in this package imports a PSP SDK type.
//
// Hard rules (each pinned by tests):
//
//   - The kind-specific detail (IBAN, token, address, ...) is AEAD-encrypted
//     (AES-256-GCM, HKDF subkey, AAD = tenant|instrument|kind|schema version)
//     with a key that is never stored in the database. Plaintext is never
//     stored, logged, rendered or audited; the only detail-derived value any
//     API returns is display_mask.
//   - The fingerprint is a tenant-bound HMAC under a SEPARATE key family with
//     its own lifecycle. It is never returned by any API.
//   - Go-side HMAC seals (instrument, verification, blocking event, snapshot)
//     cover the rows an arbitrary-SQL runtime session could otherwise forge
//     (ADR 0110 threat model). Seal keys are never in the database.
//   - A card number (PAN) is never accepted: a Luhn-valid 12-19 digit string in
//     any field of any kind is refused.
//   - Verification source is FORCED from the verifier's providerkind.Synthetic
//     marker; a synthetic verification can never satisfy a non-Synthetic
//     payment adapter (tiering predicate).
//
// B13-A delivers this package, the routes and migration 0123. The Go
// integration of the binding into withdrawal / payments (B13-B) is separate.
package payoutinstrument
