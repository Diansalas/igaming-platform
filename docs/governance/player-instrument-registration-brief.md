# Decision brief - player payout-instrument registration architecture (2026-10-09)

Status: **decision material only; nothing is implemented or enabled.** Prepared under owner decision 8 of
2026-10-09 (ADR 0095 s48): "Do NOT implement raw player-facing payment credential registration ... The eventual
registration architecture should use an appropriate PSP/payment-tokenization/provider flow. A verified/tokenized
instrument may be selected by the player, but the platform remains authoritative for eligibility."

## Current state (inspected in the code, not assumed)

- **Frontend (b2c):** lists and selects already-existing verified instruments only. There is NO registration UI and none is
  to be built (decision 8).
- **Backend route that already exists (B13-A, MOCK):** `POST /v1/me/payout-instruments` takes a client-supplied `detail`
  object per kind and stores it encrypted (AEAD) with a tenant-bound fingerprint. Kinds: `bank_account` (account/IBAN-style
  identifier), `ewallet_account` (account id/email), `card_token` (a PSP token plus CLIENT-supplied `psp_card_fingerprint`,
  `last4`, `network`), `crypto_address` and `synthetic_test` (both disabled for real use). A raw card number (Luhn-valid PAN,
  including separator variants) is refused at every field of every kind.
- **Why nothing real is reachable today:** a non-Synthetic instrument can only become `verified` through a registered
  non-Synthetic verifier, and no such verifier exists (only the MOCK verifier). The startup check also refuses any
  non-Synthetic payout adapter that is not tiering-ready.

## The tension to resolve (not resolved here)

Decision 8 forbids a UI/flow in which the platform frontend collects raw sensitive payment credentials. The existing route
lets an authenticated client submit a bank/e-wallet identifier directly to the platform API. Whether a bank account/IBAN or an
e-wallet id counts as a "bank credential" for decision 8 is an interpretation that the human has not stated. `card_token`
already uses a token, but its `psp_card_fingerprint`, `last4`, `network` are client-supplied (launch flag L-1).

## Proposed target architecture (recommendation, for a later decision)

1. **PSP-hosted or tokenized capture:** the player enters credentials only on a PSP-hosted field/redirect; the platform
   receives a provider token plus provider-signed metadata (fingerprint, last4, network, ownership/verification proof)
   through a server-side, authenticated provider callback or fetch. The browser never supplies the metadata.
2. **Registration = "begin" + "complete":** `begin` returns a provider session reference (no credentials); `complete` is driven
   by the provider callback (signed, replay-protected, tenant-scoped) and creates the instrument row from provider-attested data.
3. **Verification source = the provider attestation** (HD-R15-2: which ownership assertions count as "verified" is still an
   open owner decision), with the max-age/re-verification cadence (HD-R15-1).
4. **Raw-detail route:** restrict `POST /v1/me/payout-instruments` with raw `detail` to Synthetic/MOCK environments, or
   remove it for non-Synthetic kinds once the provider flow exists.
5. **PCI/PII review:** security review of the provider flow, the callback authentication (S-3 style) and the data minimisation.

## Decisions required (human/security)

- D-REG-1: is a raw bank-account/IBAN or e-wallet identifier submitted to the platform API a "raw sensitive payment
  credential" under decision 8? If YES, the raw-detail route should be disabled outside Synthetic environments now
  (small change, recommended); if NO, it may remain until the provider flow exists.
- D-REG-2: HD-R15-2 (what counts as "verified") and HD-R15-1 (max-age cadence) are prerequisites for any real registration.
- D-REG-3: which PSP/tokenization provider flow (provider-dependent; no provider is authorised yet).

## What stays gated

Any registration UI, any provider-specific flow, any non-MOCK verifier, and any change that lets a browser supply attested
instrument metadata. No code was changed for this brief.
