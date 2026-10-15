# AML confidentiality duty for manual-adjustment approvers (D-AML-TIP-1, option (c))

Status: **owner-approved duty (option c), legal wording pending (E-class).** The owner decided D-AML-TIP-1 on 2026-10-15: option (c)
now (accept the visible signal, with a documented AML confidentiality duty for adjustment approvers), option (a) (a deliberately
shared token) later; option (b) (generic `conflict` wording) stays as built. The duty is approved as a duty; the WORDING below was
written by engineering, is **not legal advice and not approved by counsel**, and counsel must approve or replace it before any
`under_platform_licence` tenant is launched. Software capability only: nothing here is a regulatory approval.

## Why this exists

When an AML hold (or an AML hold-evaluator fault) is the reason a manual adjustment cannot be approved, the platform shows
tenant finance staff the same generic `conflict` response it shows for an ordinary conflict (ADR 0116 F4 option (b), kept).
That hides the wording, **not the fact**: a `conflict:` refusal on a request that is still `pending` with no approval row can
only mean "a hold or an evaluator fault" (the only other producer of that shape needs a request that is no longer pending), and
the response time of the two paths differs slightly. Option (c) accepts that residual signal as a known, documented limit and
puts the protection on people rather than on a hidden status code.

## What the platform does and does not do (so the duty is not mistaken for a control)

- Keeps the generic cover (b): status 409, headers, body and denied-audit shape are identical to an ordinary conflict. A test
  pins this (`TestManualAdjustmentAPI_F4_AMLRefusalIsIndistinguishableFromAnOrdinaryConflict`).
- Does **not** hide the pending-with-no-approval shape or the timing difference. That is the accepted residual.
- Never tells the player anything: the player-facing codes for a held operation are identical to KYC pending
  (`account_review`).
- Writes the real reason only to the AML-scoped decision log, readable by AML officers.

## The duty (for every person who can approve or reject a manual adjustment)

1. Treat a persistent `conflict` on a pending adjustment, where nobody has decided it, as a possibly confidential cause. Do not
   try to find out the cause, and do not ask the player, the initiator or other staff for one.
2. Never tell the player, or anyone without AML access, that an account may be under AML review, or that an adjustment was
   refused for such a reason. Do not suggest it, hint at it, or confirm a guess. This holds after the matter is closed.
3. Refer the request to an AML officer of your tenant (or, for a platform-licensed tenant, to platform compliance) and take no
   further action on it; a hold clears only through the AML process.
4. Do not copy request details into tickets, chat or e-mail to explain a delay. Use a neutral statement: "the request is under
   review".
5. Breach of this duty can itself be a legal offence in some jurisdictions (tipping off). Which, and the penalties, are for
   legal to state; this text does not.

## Operating requirements (owner / legal decisions, not engineering)

- **Acknowledgement.** Every adjustment approver acknowledges this duty at onboarding and yearly. Where it is recorded (staff
  record, HR system) is an owner decision. Not built.
- **Who counts as an approver.** Staff holding the `ledger_adjustment:approve` capability grant (finance, and platform_admin
  acting) and anyone who can read the denied-audit rows.
- **In-product notice.** An on-screen reminder in the adjustment approval screens is NOT built: its wording would itself tell
  approvers that a `conflict` may be AML-related, so it needs `security` and `ux-design` sign-off. Recorded as a follow-up.
- **Option (a) later.** One deliberately shared token that ordinary non-AML causes also produce (so the signal is no longer
  deterministic) remains the recommended follow-up. Not built.

## Status

| Item | Label |
|---|---|
| Generic `conflict` cover (F4 option (b)) | IMPLEMENTED (unchanged; reversible) |
| Documented duty (this text) | Owner-approved duty (option c, 2026-10-15); legal wording pending (E-class) |
| Acknowledgement recording | NOT IMPLEMENTED (owner decision) |
| In-product reminder | NOT IMPLEMENTED (needs security + ux-design) |
| Option (a), shared token | NOT IMPLEMENTED (later) |
| Owner decision | DECIDED 2026-10-15: (c) now, (a) later, (b) as built (the register row is updated by the orchestrator; see `docs/plans/a7-aml-record.md`) |
