# Operating basis (lic9, migration 0180)

How an authorised platform admin records, reviews and withdraws the **operating basis** a tenant (or one of its brands) relies on to
operate an operation in a country, **with no code change and no release**. Software capability only: nothing here is a legal or
regulatory position, and the platform ships **no value** (no country, no jurisdiction, no basis, no exemption).

## 1. Principles (the owner's requirement, restated)

- The platform does **not** hard-code a country allowlist, does **not** assume every brand uses our licence, and does **not** assume no
  licence is needed anywhere. The arrangement is business configuration chosen later by authorised admins under maker-checker.
- **Country configuration is not legal proof.** An enabled country policy says what the operator chose to switch on; it never says that
  switching it on is lawful. A basis row records WHO decided WHAT, on WHICH evidence, until WHEN it must be reviewed. It is a recorded
  business decision, not a licence and not legal advice.
- A **no-licence** basis is an explicit, documented, reviewed decision for one jurisdiction / operation / tenant / brand. It is never a
  default, never implied by the absence of a licence, and never a blanket permission. A tenant with no licence and no basis resolves
  `licensing_unknown` (fail closed), exactly as before.
- Unresolved or unavailable evidence stays fail closed: a basis that is withdrawn, not yet in force, not approved, past its review date,
  flagged for re-review, unreadable, or ambiguous (two bases) never permits.

## 2. The three bases

| `basis_kind` | Meaning | Required fields | What decides the licence side |
|---|---|---|---|
| `platform_licence` | the scope relies on a licence whose licensee is `platform` (our company) | `licence_id` of a `licensee = platform` licence | the existing licence validity and `licence_country_ceilings` row of THAT licence (it overrides `tenants.licence_id` for the scope) |
| `partner_licence` | the scope relies on the partner's own licence (licensee `tenant`), possibly different per brand | `licence_id` of a `licensee = tenant` licence, not bound to a different tenant | the same, for that licence |
| `no_licence_documented` | an explicit decision, for this jurisdiction / operation / tenant / brand, that the scope is operated on a documented no-licence basis | `jurisdiction_id` (the regime relied on), `review_due_at` (mandatory), no `licence_id` | nothing: there is no licence step. It is a configuration conflict (fail closed) if the tenant's own licence ALSO has a ceiling row for that country |

Common fields on every basis:

| Field | Meaning |
|---|---|
| `tenant_id`, `brand_id` (null = tenant-wide), `country_code`, `operation_code`, `product_code` (null = all products) | the scope. A brand-specific row wins for that brand; a withdrawn brand row masks the tenant-wide row (no fall back). |
| `determination_reference` | opaque document id of the recorded determination (no names, contact details or document content; the lint refuses e-mail, phone, IBAN, wallet and card shapes) |
| `determined_by_role` | the responsible **function** (for example `head_of_compliance`), never a person's name |
| `basis_evidence_reference` | optional opaque id of the supporting evidence |
| `restrictions_note`, `restricted_product_codes` | free restrictions (bounded note, no personal data) and a structured list of products the basis does NOT cover |
| `review_due_at` | the last day the basis stands. Mandatory for `no_licence_documented`; optional for the licence kinds. The day after it, the resolver stops permitting. |
| decision-makers | NOT stored in the row: derived from the executed licensing change (`change_request_id`): the requester, the executor and the approving staff accounts, shown by `GET /v1/admin/operating-bases/{id}` |

## 3. Procedure (no release needed)

All three steps use the **existing** route `POST /v1/admin/licensing-changes` (platform_admin; `operating_market_ceiling:manage`;
critical step-up; a signed actor proof per step, ADR 0110). Payload keys are fixed and all present (`null` = none).

1. **Set a basis** (`kind = operating_basis_set`). Always widening: needs **two approvals by two other Persons** (neither the requester
   nor the requester's Person; approvals at most 24 h old). Payload:
   `tenant_id, brand_id, country_code, operation_code, product_code, basis_kind, licence_id, jurisdiction_id, determination_reference,
   determined_by_role, basis_evidence_reference, restrictions_note, restricted_product_codes, review_due_at`.
   A set replaces the scope's open version (close + append in one transaction); the history stays.
2. **Open the market** as before: an enabled tenant-rung `operating_country_policies` version (kind `operating_policy_version`, two
   approvals), then brand / operation rungs if the scope needs narrowing. A basis alone opens nothing: without an enabled tenant policy
   the answer is `not_configured`.
3. **Review** before `review_due_at` (`kind = operating_basis_review_record`, payload: scope keys + `review_due_at`, `needs_rereview`,
   `review_reference`). A later date or clearing `needs_rereview` widens (two approvals); an earlier date or marking
   `needs_rereview = true` narrows (single actor, takes effect at once).
4. **Withdraw** (`kind = operating_basis_withdraw`, payload: scope keys). Always narrowing: single actor with a reason code, executed in
   the same call. The scope then relies on **no** basis and does **not** fall back to the tenant's licence.

Reading: `GET /v1/admin/operating-bases?tenant_id=&country_code=&current=true` and `GET /v1/admin/operating-bases/{id}` (platform_admin,
`jurisdiction_registry:read`). A tenant has no read API of its own (its resolver reads its own rows under row-level security).

## 4. What the resolver answers

For `(tenant, brand?, country, operation, product?)`, at the database clock:

| Situation | Outcome |
|---|---|
| no `operating_bases` row for the scope | exactly the pre-lic9 behaviour (the tenant's licence chain and ceilings; a tenant with no licence is `licensing_unknown`) |
| row withdrawn / not approved (no executed change request) / not in force / not yet effective / review overdue / `needs_rereview` / product restricted | `not_permitted_by_basis` (new outcome; the Back Office / Explain shows the reason) |
| licence kind, licence suspended / expired / not yet issued / no ceiling / ceiling disabled | `not_permitted_by_licence` (existing codes) |
| licence kind, licence missing, wrong licensee, or a partner licence also bound to another tenant | `licensing_unknown` / `invalid_configuration` |
| `no_licence_documented` and the tenant's own licence also has a ceiling row for the country | `configuration_conflict` |
| two in-force rows of the same specificity | `configuration_conflict` |
| basis permits | the tenant, brand and operation policy rungs still must permit; only then `permitted` |

Every consumer treats any outcome other than `permitted` as a refusal. A row written on the owner (provisioning) path has no change
request and is never an approved basis.

## 5. Readiness

`R-OPERATING-BASIS` (snapshot version 7) is an informational, acknowledgeable launch readiness item (never a hard precondition). It
reports how many enabled countries are governed by an explicit basis, how many by the tenant's own licence chain, and lists bases whose
review is overdue or due within 30 days (ids only). See `launch-and-suspension.md`.

## 6. Pending human decisions and interim behaviour

- Which roles may approve, and how many approvals, stay the interim answer to HDR-M-1 (platform_admin, two). Changing it is a reviewed
  migration.
- Whether a particular country / operation may be run on a no-licence basis, and on what evidence, is a legal and business decision for
  the owner and counsel. The platform records the decision; it does not make or validate it.
- The `jurisdiction_id` of a basis is recorded for the audit trail and for review; KYC thresholds, RG floors and reporting formats are
  still selected from the tenant's licence jurisdiction (`tenants.licence_id`). A tenant that relies on a no-licence basis has no licence
  jurisdiction: `R-KYC-POLICY` then reports `no_licence_jurisdiction` and KYC resolves not required until a rulebook selection by basis
  jurisdiction is decided and built (see `docs/plans/lic9-operating-basis-record.md`, residuals).
