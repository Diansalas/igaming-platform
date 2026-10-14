# ADR 0120: Two-phase recovery-code regeneration

Status: PROPOSED. **NOT IMPLEMENTED** (no backend, OpenAPI or UI work exists for it). Origin: security review of ADR 0119
slice S7b (2026-10-14). Class B (changes an existing API contract). Registered as `RECOVERY-CODES-2PHASE-1`.
Nothing here is a regulatory claim.

## 1. Problem

`POST /v1/staff/mfa/recovery-codes/regenerate` (ADR 0119 S4b) revokes every unconsumed code and returns the ten new
ones **once**, in a single transaction. If the response never reaches a screen that the operator can read (tab closed,
navigation, network drop after commit, a browser crash), the old codes are already revoked and the new ones are lost.
The operator is left with no valid recovery codes and may only discover it when they lose their authenticator.

The S7b UI mitigates this (confirm text, `beforeunload` guard, aborting the re-send when the page is left) but cannot
remove it: the loss window is between the server commit and the operator's acknowledgement.

## 2. Proposal (recommended by the S7b security reviewer)

Two phases, both from the same session and both step-up `critical`:

1. **Create.** The first call generates a *pending* set of ten codes, stores only their keyed hashes (as the active set
   is stored today, ADR 0119 section 6.4), and returns the plaintext once. **The old codes stay valid.**
2. **Confirm.** After the operator has stored the codes and ticked the acknowledgement, a second call (no body, or the
   pending set id) within about 10 minutes, from the same session, activates the pending set and revokes the old set in
   **one transaction**.
3. **Expiry.** An unconfirmed pending set expires (about 10 minutes) and is discarded; a new create call replaces any
   existing pending set. At most one pending set exists per staff member. Neither phase ever weakens the active set
   until phase 2 commits.

Failure behaviour: if the operator loses the screen before confirming, nothing was lost (old codes still work) and they
simply start again. If the confirm response is lost, the new set is already active and the operator holds it (they
acknowledged it before confirming).

Open design points for the implementing slice: the confirm call must prove possession (for example the pending set id plus
one of the new codes typed back, or the id only if the acknowledgement is considered enough); audit both phases
(actor, session, reason); rate limit; whether the lifecycle reset flows (S5 / S6) must also discard a pending set; whether
phase 2 needs its own step-up or relies on the phase 1 freshness window.

## 3. Rejected alternative: a server-side re-display window

Letting the server re-show the same codes for a few minutes after a regeneration would need the **plaintext (or a
reversible encryption) of the codes to be stored** server-side for that window. That contradicts the one-way keyed-hash
design of ADR 0119 section 6.4, creates a secret store that an M5 attacker (arbitrary SQL as the runtime role) or a
backup reader could harvest, and adds a read endpoint that returns recovery codes. Rejected.

## 4. Consequences

- API change (two calls instead of one) and an OpenAPI update; the S7b screen must change to a create, show,
  acknowledge, confirm sequence. Until then S7b-R1 stays an accepted residual.
- Needs `security` review (new secret-bearing state, expiry job or lazy expiry) and `architect` sign-off on the
  migration (a pending-set table or status column, RLS as for the existing recovery-code table).
- Does not change enrolment, login or step-up.
