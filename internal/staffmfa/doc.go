// Package staffmfa is the pure-Go staff MFA library of ADR 0119 (slice S1):
//
//   - TOTP (RFC 6238; package totp) with the monotonic replay rule;
//   - the two platform key families STAFF_MFA_FACTOR_KEYS (AES-256-GCM factor
//     secrets) and STAFF_MFA_MAC_KEYS (recovery-code, challenge,
//     enrolment-token and session-assurance HMAC keys), expanded with
//     HKDF-SHA256 and info = label || "|" || kid;
//   - factor-secret sealing with the AAD "sf-v1|factor|staff|tenant|type";
//   - keyed hashes HMAC(subkey, purpose || "|" || staff_user_id || "|" || value)
//     for recovery codes, challenge tokens and enrolment tokens;
//   - recovery-code generation and normalisation;
//   - the per-factor failure counters and the policy constants as pure
//     functions;
//   - redacting secret types (fmt, slog and JSON never render a secret);
//   - the StaffFactor interface and the VerifyStartup gate.
//
// There is no database access, no HTTP and no wiring into login here: the
// store layer is S2 and the flows are S3. There is no development bypass and
// no random-key fallback.
package staffmfa
