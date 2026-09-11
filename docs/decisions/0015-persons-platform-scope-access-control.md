# ADR 0015 — `persons` Access Control: Platform-Scoped, Not Unprotected

Status: Accepted (Stage 2, correcting a defect found in the same stage's
architect review)

## Context

Migration `0009_create_persons` reasoned about `persons` as "platform-
level, like `jurisdictions`/`assets`" and therefore gave it no `tenant_id`
column and no row-level security at all. Stage 2 architect review found
that analogy doesn't hold: `jurisdictions` and `assets` are read-only
reference data, never mutated on a request path, while `persons` is
INSERTed by `internal/identity.RegisterPlayer` (called from the
unauthenticated `POST /v1/auth/register` handler) *inside a tenant-scoped
transaction*. With no RLS at all, any tenant-scoped connection could
`SELECT`/`UPDATE`/`DELETE` every other tenant's `persons` rows - including
`persons.status`, which per `docs/architecture/05-identity-architecture.md`
is where platform-level self-exclusion and AML case history are meant to
attach specifically *because* it must be visible/enforceable across every
tenant a person interacts with. Left as-is, tenant B could read or flip
tenant A's players' platform-level exclusion state.

## Decision

`persons` remains genuinely platform-wide data - it is not given a
`tenant_id` column, and it is not switched to ordinary tenant-scoped RLS
(there is nothing to scope by). Instead, migration
`0015_enable_rls_on_persons` enables RLS with access split by operation:

- `INSERT` is allowed from any scope, unconditionally (`WITH CHECK
  (true)`) - every current caller creates a person from within a
  tenant-scoped registration transaction, and there is no tenant-specific
  data in the row to protect at creation time.
- `SELECT`, `UPDATE`, and `DELETE` are restricted to the platform scope
  only (`USING (NULLIF(current_setting('app.tenant_id', true), '') IS
  NULL)`) - i.e. only a `db.Pool.WithoutTenant` connection can read or
  modify a `persons` row.

No Stage 2 code path currently `SELECT`s or `UPDATE`s `persons` (`internal/
identity/person.go` only exposes `CreatePerson`), so this is a pure
tightening with no behavior change to anything built so far.

## Consequences

- Stage 4's KYC/AML processing (the first code expected to read or update
  `persons.status`/`person_key_hash` for real) must run platform-scoped
  (`WithoutTenant`), not from within a tenant-scoped transaction - this is
  architecturally correct anyway, since that processing is exactly what
  must see a person's status consistently across every tenant/brand they
  touch.
- If a future feature genuinely needs a tenant-scoped read of a person's
  data, it must go through a purpose-built, audited accessor - never a
  raw tenant-scoped `SELECT ... FROM persons` - and that accessor's design
  should be reviewed by `security` and `identity-compliance` together,
  the same review pairing that caught this gap.
- This is the same class of finding as ADR 0013's audit-atomicity
  correction: a table reasoned about as "platform-level like reference
  data" without checking whether anything actually mutates it from a
  tenant-scoped context. Future platform-wide tables should have their
  write paths checked for this before RLS is skipped, not after.

## Owner

`architect` (found the gap), `security` (reviewed the fix),
`identity-compliance` (owns what `persons` is ultimately used for).
