# Licensing and jurisdiction: owner decision requests and business evidence (2026-10-19)

Companion to `licensing-decision-triage.md` (row ids L-n refer to it). Nothing here is a legal opinion, a licence or jurisdiction approval, or a launch authorisation. The platform ships NO country list, NO threshold value and NO licence data: every answer below is supplied by a human and entered through the governed Back Office workflow (licensing changes, ADR 0112 launch governance).

Answer format: reply with the question id and the option letter (plus the stated minimum information where it asks for one). "Interim" is what the platform does today if you do not answer; every interim fails closed or is conservative.

## Part 1. Class B: legal / compliance / governance decisions

### Gate brand 1 (own company, Anjouan licence). Answer these first.

| # | Question | Options | Engineering recommendation | Minimum information | Interim until answered |
|---|---|---|---|---|---|
| Q1 (L-26, HDR-J-6) | Which countries (ISO-3166-1 alpha-2), per product (casino, sportsbook) and operation (registration, deposit, wagering), may brand 1 serve? | Explicit enumerated list; start with a single country | Smallest list counsel has approved in writing | Per country: counsel reference, licence ceiling confirmation, product scope, effective date | No country is open. Every registration, deposit and casino/sportsbook wager is refused. |
| Q2 (L-12, SRME-4 / HDR-J-8) | Does the Anjouan licence require blocking by physical location, or is KYC-verified residence sufficient? | (a) residence sufficient; (b) location required (a geolocation vendor becomes a launch prerequisite, class D); (c) location advisory, restriction only | None without counsel | Licence condition text or counsel opinion reference | Residence-only gate; no location evidence exists. |
| Q3 (L-14, SRME-6) | May a visitor from an excluded market create an unverified account? | (a) yes (as built); (b) require declared country at sign-up (courtesy filter); (c) disable registration per brand until confirmed | (a) with legal confirmation, else (c) | Licence terms on account opening | (a) as built. Unverified accounts cannot deposit or play. |
| Q4 (L-18, OQ-2) | Responsible-gaming floor/ceiling values per limit kind (deposit, loss, wager), period and currency for the first licence jurisdiction, and the named signatory | Values, or attested `none` per cell | None. Engineering supplies no value. | Table kind x period x currency, attestation reference, effective date | RG enforcement stays OFF for the brand (owner decision 2026-10-16); the readiness item is acknowledgeable. |
| Q5 (L-17) | Does the first jurisdiction legally require monthly wager or loss limits? | Yes (rollup scheduled before launch) / no (windows up to 7 days) | n/a | Licence condition or counsel statement | Windows up to 7 days only. |
| Q6 (L-20, HD-KYC) | At what thresholds/triggers must KYC be passed before play and before first withdrawal (incl. withdrawal)? | Per ADR 0096 s4 items HD-KYC-1..8 | At minimum passed KYC before first withdrawal; value must be human-supplied | Licence KYC conditions; thresholds with currency | KYC enforcement dormant: no policy means "not required". The launch readiness item now flags a brand with zero policies (lic7). |
| Q7 (L-21) | May a real-money brand activate while transaction monitoring is NOT IMPLEMENTED? Also AML wording and two named AML officers | (a) no (make `R-AML-MONITORING` blocking); (b) yes, with a manual monitoring procedure recorded as a determination reference | (b) only with counsel sign-off recorded, else (a) | AML programme document reference; two named AML officers | The item always fails and must be acknowledged by the approvers. |
| Q8 (L-22, MKT-EXPIRY-1) | Is a licence valid on its expiry date and on its issue date? Expiry-alert lead time and recipient | (a) invalid on the expiry date, valid on the issue date (as built, conservative); (b) valid through the expiry date | (a), no grace period invented | Licence terms or counsel confirmation; days before expiry, recipient | (a). Alerts only at a 30-day interim window; no auto-suspension. |
| Q9 (L-29) | Activation approver counts | Confirm or change: brand activation one approver besides the requester; tenant activation two | Raise brand 1 (first real money) to two approvers; the DB trigger forces counts, so a change is a migration | Names/roles of approvers | As built. |
| Q10 (L-30) | Must an activation request carry a verifiable evidence reference for a licence-backed basis? | (a) optional note (as built); (b) mandatory `determination_reference` for all four bases | (b), small change once chosen | n/a | (a). |
| Q11 (HDR-M-1, L-10) | Who may approve a licence create/bind, a licence widening change, or enabling a country? | Option set: (a) platform_admin x2 distinct Persons (INTERIM, built); (b) (a) plus a compliance-role sign-off recorded as an evidence reference; (c) different approver count | (a), reuse ADR 0112 pattern; widening only, narrowing stays single-actor | Roles holding approval authority; whether the authorisation reference must be verifiable | (a) as built. A single approver cannot bypass; the requester cannot approve. |

### Licence-lifecycle governance (new with this work)

| # | Question | Options | Recommendation | Interim |
|---|---|---|---|---|
| Q12 (G-5 / C2) | While a licence or tenant/brand is suspended or expired, do withdrawals and payouts of existing balances continue? | (a) yes always (as built); (b) yes after a per-jurisdiction hold; (c) no | (a) unless counsel says otherwise; blocking player funds is a legal call | (a). New deposits, casino launch/bets, sportsbook bets and registration stop; withdrawals, payouts, settlement and rollbacks of already-placed bets keep running. |
| Q13 | Does licence expiry suspend automatically, or only alert? | (a) alert only (built); (b) auto-suspend at expiry | (a) until Q8 is answered | (a). The runtime market gate still fails closed on an expired licence. |
| Q14 | Does the platform need a `revoked` status? | (a) model revoked as `expired` plus a reason code (built); (b) add a distinct status (migration) | (a) | (a). |
| Q15 (G-3) | May one tenant operate under more than one licence, or a brand under its own licence (hybrid inside one tenant)? | (a) one licence per tenant (built); (b) several licences per tenant; (c) a licence per brand | (a) for brand 1; decide before the first B2B tenant with a multi-jurisdiction footprint | (a). |
| Q16 (G-11 / D-3) | May a tenant read the history of its own licence? | (a) no (built); (b) a redacted projection limited to its binding window, without staff ids | Product decision; security advises against opening `licence_versions` to tenants | (a). |

### Needed before a second jurisdiction or a B2B tenant (not gating brand 1)

| # | Question | Notes |
|---|---|---|
| Q17 (L-3, HDR-J-3) | Lawful basis, privacy-notice wording and retention for declared/verified residence; permitted corroborating document types. Recommendation: collect verified residence only (leave declared residence OFF). | Counsel/privacy sign-off reference, retention schedule per data category. |
| Q18 (L-7, HDR-J-7 / L-9, HDR-J-9) | For each operation class, which determination is required: identity (verified residence), market-access (residence plus location) or both; maximum age of a location signal if used. | Needed before the player-scoped resolver is wired (L-4). Until then the resolver returns unresolved and every jurisdiction-scoped rule fails closed. |
| Q19 (L-11, HDR-M-2) | After a country closes for a tenant, may existing players withdraw? Options: (a) always (built), (b) after a hold, (c) no. | |
| Q20 (L-13, SRME-5) | Withdrawn residence evidence mid-session or mid-deposit: (a) bounded exposure (built, 4 h cap), (b) revoke open sessions, (c) (b) plus hold deposit credit. | Maximum acceptable exposure window. |
| Q21 (L-15, SRME-1) | Second person for residence determinations: (a) no, step-up plus alert; (b) yes for determinations into an open market; (c) all. Recommendation (b). | Compliance staffing. |
| Q22 (L-19, SR6R-1) | Which jurisdiction's RG floors apply: licence's (built), the player's market, or the stricter. | Before a second jurisdiction or a B2B tenant. |
| Q23 (HDR-J-5) | Counsel validation of the bring-your-own-licence policy before the first B2B onboarding. | |
| Q24 (L-27, HDR-SB-1) | Sportsbook trading-book liability owner and exposure ceiling. | Ask when a sportsbook brand is scheduled. |
| Q25 (HDR-J-4 / L-5) | Authority for in-flight obligations after a jurisdiction change (SAR, data residency). | Only when a second jurisdiction exists. |

## Part 2. Class C: business evidence and configuration the operator must supply

Entered through the Back Office (platform_admin; widening items require two approvers). The platform does not verify any of it; evidence references are opaque operator-supplied identifiers.

1. Jurisdiction record: code, name, regulator, `country_code`.
2. Licence record: licence number, licensee (`platform` or `tenant`), jurisdiction, issue and expiry dates, permitted products, evidence reference, review-due date.
3. Tenant binding: which licence the tenant operates under (`under_platform_licence` or `own_licence`; no default is preselected).
4. Per jurisdiction in use: KYC, AML and RG ruleset ids, allowed currencies and payment methods (`tenant_jurisdiction_configs`).
5. Market rows: licence country ceilings and per-brand operating-country policies with an authorisation reference per enabling row (from Q1).
6. Residence evidence collection switch and lawful-basis reference per tenant (from Q17).
7. RG floor values (Q4) and the KYC policy rows (Q6).
8. Two named AML officers per owner scope; screening policy per referenced jurisdiction (Q7).
9. Responsible operator name/registration number and responsible role for `other_manually_approved` / `not_applicable` bases.
10. Alert recipients (interim: `diego.salas806@gmail.com` plus one webhook), on-call rota.
11. For B2B: the tenant's own jurisdiction row and licence record, the tenant's own provider contracts and credentials (class D).

## Part 3. Items that are NOT decided here

- Class D (provider-dependent): production KYC vendor (without it no production player can have verified residence, so deposit, casino launch and sportsbook placement refuse every production player), geolocation vendor if Q2(b), PSP/casino/sportsbook credentials, screening vendor.
- Class E (AWS-dependent): runtime-role split, kill switch, backups and environment anchor in the deployed environment.
