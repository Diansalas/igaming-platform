#!/usr/bin/env python3
"""ADR 0119 S1 mutation driver. Usage: driver.py <sandbox-root> <out-file>

The sandbox is a private copy of the worktree (go.mod, go.sum, internal/, .env.example).
Each mutant replaces exactly one occurrence of `old` with `new` in one file, runs the
named packages' unit tests, and is KILLED when the run fails (build failure is reported
as ERROR, not evidence). The file is always restored.
"""
import subprocess, sys, pathlib, time

root = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
SM = "./internal/staffmfa/..."
CF = "./internal/config/..."
T = "internal/staffmfa/totp/totp.go"
K = "internal/staffmfa/keys.go"
S = "internal/staffmfa/secrets.go"
R = "internal/staffmfa/recovery.go"
P = "internal/staffmfa/policy.go"
F = "internal/staffmfa/factor.go"
U = "internal/staffmfa/startup.go"
C = "internal/config/staff_mfa.go"

M = [
 # id, file, old, new, pkgs, description
 ("T01", T, "eligible := boolToInt(s > lastUsedStep)", "eligible := boolToInt(s >= lastUsedStep)", [SM], "replay rule > changed to >="),
 ("T02", T, "Skew = 1", "Skew = 2", [SM], "window widened to +-2 steps"),
 ("T03", T, "accepted = (s & mask) | (accepted &^ mask)", "if found == 0 {\n\t\t\taccepted = (s & mask) | (accepted &^ mask)\n\t\t}", [SM], "lowest matching step burned instead of highest"),
 ("T04", T, "if code[i] < '0' || code[i] > '9' {", "if false {", [SM], "code character class not checked"),
 ("T05", T, "func Verify(secret []byte, code string, now time.Time, lastUsedStep int64) (int64, error) {\n\tif len(secret) != SecretBytes {", "func Verify(secret []byte, code string, now time.Time, lastUsedStep int64) (int64, error) {\n\tif len(secret) == 0 {", [SM], "Verify secret length not checked"),
 ("T06", T, "\tif lastUsedStep < 0 {\n\t\treturn 0, ErrInvalidState", "\tif false {\n\t\treturn 0, ErrInvalidState", [SM], "negative last_used_step admitted"),
 ("T07", T, "\tif u < 0 {", "\tif false {", [SM], "time before epoch admitted"),
 ("T08", T, "off := sum[len(sum)-1] & 0x0f", "off := sum[len(sum)-1] & 0x07", [SM], "HOTP dynamic-truncation offset mask"),
 ("T09", T, "bin := (uint32(sum[off])&0x7f)<<24", "bin := (uint32(sum[off]))<<24", [SM], "HOTP top-bit mask dropped"),
 ("T10", T, "Period = 30", "Period = 60", [SM], "time step changed"),
 ("T11", T, "eq := subtle.ConstantTimeCompare(want, got)", "eq := boolToInt(string(want) == string(got))\n\t\t_ = subtle.ConstantTimeCompare", [SM], "constant-time comparison replaced by =="),
 ("K01", K, 'label+"|"+kid, 32)', "label, 32)", [SM, CF], "HKDF info omits the kid"),
 ("K02", K, 'label+"|"+kid, 32)', "label+kid, 32)", [SM, CF], "HKDF info separator dropped"),
 ("K03", K, "\t\tif len(b) < MinKeyBytes {\n\t\t\treturn nil, fmt.Errorf(\"%w: %s key %q decodes to fewer", "\t\tif false {\n\t\t\treturn nil, fmt.Errorf(\"%w: %s key %q decodes to fewer", [SM, CF], "ParseKeySet admits short keys"),
 ("K04", K, "\t\tif part == \"\" {\n\t\t\treturn nil, fmt.Errorf(\"%w: %s entry %d is empty\", ErrKeyConfig, family, i+1)\n\t\t}", "\t\tif part == \"\" {\n\t\t\tcontinue\n\t\t}", [SM, CF], "empty entries skipped"),
 ("K05", K, "\t\tif other, dup := seen[string(b)]; dup {", "\t\tif other, dup := seen[string(b)]; dup && false {", [SM, CF], "ParseKeySet duplicate value admitted"),
 ("K06", K, "\t\tif other, dup := seen[string(m)]; dup {", "\t\tif other, dup := seen[string(m)]; dup && false {", [SM], "NewKeys cross-family duplicate admitted"),
 ("K07", K, "\tif _, ok := factor[factorActiveKID]; !ok {", "\tif _, ok := factor[factorActiveKID]; !ok && false {", [SM, CF], "unknown factor active kid admitted"),
 ("K08", K, '"|" + a.StaffUserID.String() + "|" + tenant', '"|" + tenant', [SM], "AAD omits staff_user_id"),
 ("K09", K, "\tif a.TenantID != uuid.Nil {\n\t\ttenant = a.TenantID.String()\n\t}", "", [SM], "AAD omits tenant (constant marker)"),
 ("K10", K, 'return []byte("sf-v1|" + a.FactorID.String() + "|"', 'return []byte("sf-v1|" + "|"', [SM], "AAD omits factor_id"),
 ("K11", K, "tenant := \"platform\"", "tenant := \"\"", [SM], "platform AAD marker dropped"),
 ("K12", K, "gcm.Seal(nil, nonce, secret.b, ad)", "gcm.Seal(nil, nonce, secret.b, ad[:0])", [SM], "seal ignores the AAD"),
 ("K13", K, 'return FactorSecret{}, fmt.Errorf("%w: %w", ErrFactorIntegrity, ErrUnknownKID)', "return FactorSecret{}, ErrUnknownKID", [SM], "unknown kid not reported as integrity"),
 ("K14", K, "if err != nil || len(pt) != secretLen(aad.FactorType) {", "if err != nil {", [SM], "opened plaintext length not checked"),
 ("K15", K, "\tif len(s.Nonce) != NonceBytes {\n\t\treturn FactorSecret{}, ErrFactorIntegrity", "\tif len(s.Nonce) == 0 {\n\t\treturn FactorSecret{}, ErrFactorIntegrity", [SM], "nonce length not checked"),
 ("K16", K, 'm.Write([]byte(purpose + "|" + staff.String() + "|"))', 'm.Write([]byte(purpose + "|"))', [SM], "MAC omits staff_user_id"),
 ("K17", K, 'm.Write([]byte(purpose + "|" + staff.String() + "|"))', 'm.Write([]byte(purpose[:0] + staff.String() + "|"))', [SM], "MAC omits purpose"),
 ("K18", K, "\tcase MACChallengeMFA, MACChallengeEnrol:\n\t\treturn s.challenge", "\tcase MACChallengeMFA, MACChallengeEnrol:\n\t\treturn s.enrolment", [SM], "challenges use the enrolment subkey"),
 ("K19", K, "if err != nil || len(stored) != MACBytes {", "if err != nil {", [SM], "stored MAC length not checked"),
 ("K20", K, "\tout := []string{k.macActive}\n\trest", "\treturn []string{k.macActive}\n\tout := []string{k.macActive}\n\trest", [SM], "lookups only under the active kid"),
 ("K21", K, "\t\tk.factor[kid] = sk", "\t\tk.factor[kid] = sk\n\t\t_ = sk\n\t\tk.factor[kid] = append([]byte(nil), m[:32]...)", [SM], "factor master used directly (no HKDF)"),
 ("K22", K, "\tif !ok || staff == uuid.Nil || len(value) == 0 {", "\tif !ok || len(value) == 0 {", [SM], "MAC admits a nil staff id"),
 ("S01", S, "\tsameStaff := rowStaff == t.staff", "\tsameStaff := true", [SM], "token row staff binding not checked"),
 ("S02", S, "func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }", "", [SM], "Token.Format removed"),
 ("S03", S, "func (EnrolmentMaterial) String() string { return Redacted }", "func (m EnrolmentMaterial) String() string { return m.uri }", [SM], "EnrolmentMaterial.String leaks the URI"),
 ("S04", S, "func (FactorSecret) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }", "", [SM], "FactorSecret.MarshalJSON removed"),
 ("S05", S, "func (Assertion) LogValue() slog.Value { return slog.StringValue(Redacted) }", "func (a Assertion) LogValue() slog.Value { return slog.StringValue(a.value) }", [SM], "Assertion.LogValue leaks the code"),
 ("S06", S, "\tif err != nil || staff == uuid.Nil {\n\t\treturn Token{}, ErrInvalidInput", "\tif err != nil {\n\t\treturn Token{}, ErrInvalidInput", [SM], "ParseToken admits a nil staff id"),
 ("S07", S, "raw, err := base64.RawURLEncoding.Strict().DecodeString(s)", "raw, err := base64.RawURLEncoding.DecodeString(s)", [SM], "token decoding not strict"),
 ("R01", R, "\t\tcase 'I', 'L':\n\t\t\tc = '1'", "\t\tcase 'I':\n\t\t\tc = '1'", [SM], "L not read as 1"),
 ("R02", R, "\t\tcase c >= 0x80:\n\t\t\treturn RecoveryCode{}, ErrInvalidInput", "\t\tcase c >= 0xff:\n\t\t\treturn RecoveryCode{}, ErrInvalidInput", [SM], "non-ASCII bytes not refused explicitly"),
 ("R03", R, "\tif len(s) == 0 || len(s) > maxRecoveryInput {", "\tif len(s) == 0 {", [SM], "presented length not bounded"),
 ("R04", R, "\t\tif seen[s] {\n\t\t\tcontinue\n\t\t}", "\t\tif seen[s] && false {\n\t\t\tcontinue\n\t\t}", [SM], "duplicate codes admitted in a batch"),
 ("R05", R, 'const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"', 'const crockford = "0123456789ABCDEFGHJKMNPQRSTUWXYZ"', [SM], "alphabet contains U"),
 ("R06", R, "out[i] = crockford[b&0x1f]", "out[i] = crockford[b%31]", [SM], "biased symbol mapping"),
 ("R07", R, "\tif a.kind != AssertionRecoveryCode {", "\tif false {", [SM], "any assertion kind read as a recovery code"),
 ("P01", P, "SoftLockThreshold = 5", "SoftLockThreshold = 6", [SM], "soft-lock threshold raised"),
 ("P02", P, "\t\tout.FailedCount, out.FailedWindowStart = 0, time.Time{}\n\t}\n\tif windowOpen(c.Failed24hStart", "\t}\n\tif windowOpen(c.Failed24hStart", [SM], "counter not reset when the soft lock is set"),
 ("P03", P, "return !start.IsZero() && now.Sub(start) < length", "return !start.IsZero() && now.Sub(start) <= length", [SM], "window boundary inclusive"),
 ("P04", P, "if !c.LockedUntil.IsZero() && now.Before(c.LockedUntil) {", "if !c.LockedUntil.IsZero() && !now.After(c.LockedUntil) {", [SM], "still locked at locked_until"),
 ("P05", P, "if out.Failed24hCount >= HardLockThreshold && c.HardLockedAt.IsZero() {", "if out.Failed24hCount > HardLockThreshold && c.HardLockedAt.IsZero() {", [SM], "hard lock at 11 not 10"),
 ("P06", P, "\tout.FailedCount, out.FailedWindowStart = 0, time.Time{}\n\treturn out\n}", "\tout.FailedCount, out.FailedWindowStart = 0, time.Time{}\n\tout.Failed24hCount, out.Failed24hStart = 0, time.Time{}\n\treturn out\n}", [SM], "success also resets the 24h count"),
 ("P07", P, "\tif !c.valid() {\n\t\treturn LockInvalid", "\tif false {\n\t\treturn LockInvalid", [SM], "invalid counters treated as open"),
 ("P08", P, "\tif !c.HardLockedAt.IsZero() {\n\t\treturn LockHard\n\t}\n\tif !c.valid() {\n\t\treturn LockInvalid\n\t}", "\tif !c.valid() {\n\t\treturn LockInvalid\n\t}\n\tif !c.HardLockedAt.IsZero() {\n\t\treturn LockHard\n\t}", [SM], "hard lock evaluated after validity"),
 ("P09", P, "return !start.IsZero() && now.Sub(start) < length", "return now.Sub(start) < length", [SM], "zero window start treated as open window"),
 ("P10", P, "\tLockInvalid LockState = iota\n\tLockOpen", "\tLockOpen LockState = iota\n\tLockInvalid", [SM], "zero lock state is open"),
 ("F01", F, "\tswitch f.Counters.LockState(now) {\n\tcase LockOpen:\n\tcase LockHard:\n\t\treturn Result{Outcome: OutcomeHardLocked}\n\tdefault: // LockSoft, LockInvalid\n\t\treturn Result{Outcome: OutcomeLocked}\n\t}\n\tif f.AAD.FactorType != FactorTOTP {\n\t\treturn Result{Outcome: OutcomeIntegrity}\n\t}\n\tsecret, err := k.OpenFactorSecret(f.AAD, f.Sealed)\n\tif err != nil {\n\t\treturn Result{Outcome: OutcomeIntegrity}\n\t}", "\tsecret, err := k.OpenFactorSecret(f.AAD, f.Sealed)\n\tif err != nil {\n\t\treturn Result{Outcome: OutcomeIntegrity}\n\t}\n\tswitch f.Counters.LockState(now) {\n\tcase LockOpen:\n\tcase LockHard:\n\t\treturn Result{Outcome: OutcomeHardLocked}\n\tdefault: // LockSoft, LockInvalid\n\t\treturn Result{Outcome: OutcomeLocked}\n\t}", [SM], "locked factor opened/compared before refusing"),
 ("F02", F, "\tif a.kind != AssertionTOTP {\n\t\treturn Result{Outcome: OutcomeRejected}", "\tif false {\n\t\treturn Result{Outcome: OutcomeRejected}", [SM], "assertion kind not checked"),
 ("F03", F, "\tOutcomeRejected Outcome = iota\n\t// OutcomeAccepted: the assertion verified and was burned.\n\tOutcomeAccepted", "\tOutcomeAccepted Outcome = iota\n\t// OutcomeAccepted: the assertion verified and was burned.\n\tOutcomeRejected", [SM], "zero Result is accepted"),
 ("F04", F, "\tcase LockHard:\n\t\treturn Result{Outcome: OutcomeHardLocked}", "\tcase LockHard:\n\t\treturn Result{Outcome: OutcomeLocked}", [SM], "hard lock reported as soft lock"),
 ("U01", U, "\tif _, err := keys.OpenFactorSecret(aad, sealed); err != nil {", "\tif _, err := keys.OpenFactorSecret(aad, sealed); err != nil && false {", [SM], "startup open self-check removed"),
 ("U03", U, "\tif _, err := keys.SealFactorSecret(aad, secret); err != nil {", "\tif _, err := keys.SealFactorSecret(aad, secret); err != nil && false {", [SM], "startup seal self-check removed"),
 ("U02", U, "\t\tif _, err := keys.macUnder(kid, MACRecoveryCode, probe, []byte(\"self-check\")); err != nil {", "\t\tif _, err := keys.macUnder(kid, MACRecoveryCode, probe, []byte(\"self-check\")); err != nil && false {", [SM], "startup MAC self-check removed"),
 ("C01", C, "\t{staffmfa.EnvFactorKeys, func(c Config) (map[string][]byte, error) {", "\t{\"X\" + staffmfa.EnvFactorKeys, func(c Config) (map[string][]byte, error) {", [CF], "registry entry renamed (completeness pin)"),
 ("C02", C, "if prev, dup := seen[v]; dup && prev.family != f.name {", "if prev, dup := seen[v]; dup && false && prev.family != f.name {", [CF], "registry pairwise check disabled"),
 ("C03", C, "\t\tif c.Environment == \"production\" {\n\t\t\treturn fmt.Errorf(\"config: %s / %s and %s / %s are required", "\t\tif false {\n\t\t\treturn fmt.Errorf(\"config: %s / %s and %s / %s are required", [CF], "keys not required in production"),
 ("C04", C, "\tif c.GuardEnvironment() != \"development\" {", "\tif c.Environment != \"development\" {", [CF], "placeholder check ignores a missing APP_ENV"),
 ("C05", C, "\tDevProviderCredentialFingerprintKeyPlaceholder, CIProviderCredentialFingerprintKeyPlaceholder,\n", "\tDevProviderCredentialFingerprintKeyPlaceholder,\n", [CF], "CI fingerprint placeholder not refused"),
 ("C06", C, "\t\tif err := d.b.Check(d.v); err != nil {", "\t\tif err := d.b.Check(d.v); err != nil && false {", [CF], "session bounds not enforced"),
 ("C07", C, "\t\t*d.dst = time.Duration(n) * time.Second", "\t\t_ = n", [CF], "duration override ignored"),
 ("C08", C, "\tif err := staffmfa.ValidateIssuer(c.StaffMFAIssuer); err != nil {", "\tif err := staffmfa.ValidateIssuer(c.StaffMFAIssuer); err != nil && false {", [CF], "issuer not validated"),
 ("C09", C, "\t{\"JWT_PREVIOUS_SECRET\", func(c Config) (map[string][]byte, error) {\n\t\treturn singleKey(c.JWTPreviousKID, c.JWTPreviousSecret), nil", "\t{\"JWT_PREVIOUS_SECRET\", func(c Config) (map[string][]byte, error) {\n\t\treturn nil, nil", [CF], "JWT previous secret not in the registry check"),
 ("C10", C, "\t{\"PAYOUT_INSTRUMENT_FP_KEYS\", func(c Config) (map[string][]byte, error) {\n\t\treturn kidListKeys(\"PAYOUT_INSTRUMENT_FP_KEYS\", c.PayoutInstrumentFPKeys.Reveal(), actorproof.ParseKeySet)", "\t{\"PAYOUT_INSTRUMENT_FP_KEYS\", func(c Config) (map[string][]byte, error) {\n\t\treturn nil, nil", [CF], "PAYOUT_INSTRUMENT_FP_KEYS not in the registry check"),
]

def run(pkgs):
    p = subprocess.run(["go", "test", "-count=1"] + pkgs, cwd=root, capture_output=True, text=True, timeout=900)
    return p.returncode, p.stdout + p.stderr

only = set(sys.argv[3].split(",")) if len(sys.argv) > 3 else None
if only:
    M = [m for m in M if m[0] in only]
lines = []
rc, o = run([SM, CF])
lines.append(f"BASELINE | {'PASS' if rc == 0 else 'FAIL'}")
if rc != 0:
    lines.append(o[-2000:])
for mid, f, old, new, pkgs, desc in M:
    path = root / f
    src = path.read_text()
    n = src.count(old)
    if n != 1:
        lines.append(f"{mid} | ERROR | {desc} | pattern found {n} times")
        continue
    path.write_text(src.replace(old, new, 1))
    try:
        rc, o = run(pkgs)
    finally:
        path.write_text(src)
    if rc == 0:
        status, detail = "SURVIVED", ""
    elif "[build failed]" in o or "[setup failed]" in o:
        status, detail = "ERROR", "build failed: " + " ".join(l for l in o.splitlines() if ".go:" in l)[:300]
    else:
        status = "KILLED"
        fails = [l.strip() for l in o.splitlines() if l.strip().startswith("--- FAIL") or l.startswith("panic:")]
        detail = "; ".join(fails[:4])
    lines.append(f"{mid} | {status} | {desc}\n      {detail}")
    out.write_text("\n".join(lines) + "\n")
out.write_text("\n".join(lines) + "\n")
print("\n".join(lines))
