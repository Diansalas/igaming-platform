// Package store is the database-backed storage layer of the staff MFA
// subsystem (ADR 0119 slice S2). It implements staffmfa.StaffFactor for TOTP
// over the tables of migration 0137 and provides the recovery-code, login
// challenge and enrolment-token stores the S3 flows are built from.
//
// What it is, and is not:
//
//   - Every function takes the caller's pgx.Tx, already scoped by the db
//     package helpers (WithTenant for a tenant staff member, WithoutTenant /
//     WithPlatformAdmin for platform staff). It never opens a transaction of
//     its own, never sets a GUC, and never takes a tenant id from anywhere but
//     the staffmfa.StaffRef the caller derived from a verified challenge or
//     token. The only exception is the pre-tenant lookup (ResolveChallenge,
//     ResolveEnrolmentToken), which uses db.Pool.WithStaffChallengeLookup /
//     WithStaffEnrolmentLookup and returns the staff member's scope.
//   - Replay protection is a monotonic last_used_step burned by a conditional
//     UPDATE (last_used_step < step) while the factor row is held FOR UPDATE.
//     Failure counters are written under the same lock, so parallel guesses
//     serialise (ADR 0119 sections 6.5 and 17.4).
//   - Recovery codes, challenge tokens and enrolment tokens are stored ONLY as
//     keyed HMACs (staffmfa.Keys); a lookup is by MAC, so a planted row
//     written without the key is never found.
//   - Nothing here logs, prints or audits a secret, a code or a token, and no
//     error carries one. The package imports no logging package (pinned).
//   - A refused verification writes failure counters in the caller's
//     transaction and returns a Refusal; the caller MUST commit that
//     transaction (the "record then return the outcome" pattern of ADR 0119
//     section 6.1) or the failure is lost.
//
// There is no HTTP, no login change, no session change and no step-up here:
// those are S3 and later. Staff login on main does not call this package.
package store
