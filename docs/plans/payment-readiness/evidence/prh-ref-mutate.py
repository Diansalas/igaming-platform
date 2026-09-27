#!/usr/bin/env python3
"""PRH-REF (PROVIDER-REF-BOUND-1) mutation-kill harness (code review F6).

Applies one mutant at a time: an anchored string replacement that must match
EXACTLY once, otherwise it is reported as a SETUP ERROR. It then runs that
mutant's killing test command and always restores the file. A non-zero test
exit means KILLED.

Usage (from anywhere; the repository root is derived from this file's path):

    export DATABASE_URL=... TEST_DATABASE_URL=... TEST_RUNTIME_DATABASE_URL=... \\
           TEST_ADMIN_DATABASE_URL=... JWT_SIGNING_SECRET=...
    python3 docs/plans/payment-readiness/evidence/prh-ref-mutate.py            # all mutants
    python3 docs/plans/payment-readiness/evidence/prh-ref-mutate.py M22 M29    # a subset
    python3 docs/plans/payment-readiness/evidence/prh-ref-mutate.py --out results.txt

Use a private/local test database only. The migration tests create scratch
databases through TEST_ADMIN_DATABASE_URL. Start from a clean working tree:
if the process is killed mid-run, the version-control diff shows any mutant
that was left behind (every normal exit restores the file).

Exit status: 0 only when every selected mutant is KILLED.
"""
import datetime
import os
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", ".."))
REQUIRED_ENV = ["DATABASE_URL", "TEST_DATABASE_URL", "TEST_RUNTIME_DATABASE_URL", "TEST_ADMIN_DATABASE_URL", "JWT_SIGNING_SECRET"]

UNIT_REF = ["go", "test", "-count=1", "./internal/providerref/"]
CAS_UNIT = ["go", "test", "-count=1", "./internal/casino/", "-run", "ValidateCallbackReferences"]
CAS_INT = ["go", "test", "-count=1", "-tags=integration", "./internal/casino/", "-run", "ProviderRefBound"]
CAS_MIG = ["go", "test", "-count=1", "-tags=integration", "./internal/casino/", "-run", "Migration0099"]
PAY_INT = ["go", "test", "-count=1", "-tags=integration", "./internal/payments/", "-run", "ProviderRefBound"]
HTTP_INT = ["go", "test", "-count=1", "-tags=integration", "./internal/httpserver/", "-run",
            "OversizeProviderReference|OversizeOriginalReference|MapReceiveCallbackError"]
SB_UNIT = ["go", "test", "-count=1", "./internal/sportsbook/", "-run", "ValidateCatalogueReferences|ValidateSettlementEvent"]
SB_INT = ["go", "test", "-count=1", "-tags=integration", "./internal/sportsbook/", "-run", "SyncCatalogue_OverBound|SettlementValidation_FieldMatrix"]

MIG = "migrations/0099_provider_reference_bound.up.sql"
MIGD = "migrations/0099_provider_reference_bound.down.sql"
PR = "internal/providerref/providerref.go"

# The casino_callback_rejections.provider_tx_id CHECK, anchored by the
# constraint that follows it so the replacement matches exactly once.
REJ_TX = "CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1F\\x7F-\\x9F]'),\n    ADD CONSTRAINT casino_callback_rejections_original"


def rej_tx(check):
    return check + ",\n    ADD CONSTRAINT casino_callback_rejections_original"


MUTANTS = [
    ("M1 bound off-by-one (> becomes >=: exact max rejected)", PR, "if len(value) > MaxBytes {", "if len(value) >= MaxBytes {", UNIT_REF),
    ("M2 bound widened (MaxBytes+1 accepted)", PR, "if len(value) > MaxBytes {", "if len(value) > MaxBytes+1 {", UNIT_REF),
    ("M3 UTF-8 validity check removed", PR, "if !utf8.ValidString(value) {", "if false && !utf8.ValidString(value) {", UNIT_REF),
    ("M4 C1 control range dropped", PR, "return r <= 0x1F || (r >= 0x7F && r <= 0x9F)", "return r <= 0x1F || r == 0x7F", UNIT_REF),
    ("M5 required empty accepted", PR, "if value == \"\" {\n\t\treturn &Error{Field: field, Reason: ReasonEmpty", "if value == \"-\" {\n\t\treturn &Error{Field: field, Reason: ReasonEmpty", UNIT_REF),
    ("M6 value leaks into the error (Field carries value)", PR, "return &Error{Field: field, Reason: ReasonTooLong,", "return &Error{Field: field + value, Reason: ReasonTooLong,", UNIT_REF),
    ("M7 Fingerprint not a hash prefix", PR, "return hex.EncodeToString(sum[:])[:fingerprintHexLen]", "return hex.EncodeToString(sum[:])[1 : fingerprintHexLen+1]", UNIT_REF),
    ("M8 casino boundary call removed", "internal/casino/orchestrator.go", "if err := validateCallbackReferences(event); err != nil {", "if err := validateCallbackReferences(event); err != nil && false {", CAS_INT),
    ("M9 casino round_id not validated", "internal/casino/orchestrator.go", "providerref.Field{Name: \"round_id\", Value: event.RoundID},", "", CAS_UNIT),
    ("M10 casino original ref not validated", "internal/casino/orchestrator.go", "providerref.Field{Name: \"original_provider_tx_id\", Value: event.OriginalProviderTxID},", "", CAS_INT),
    ("M11 casino error not wrapped in domain sentinel", "internal/casino/orchestrator.go", "return fmt.Errorf(\"%w: %w\", ErrProviderReferenceInvalid, err)", "return fmt.Errorf(\"%w: %w\", ErrInvalidInput, err)", CAS_UNIT),
    ("M12 casino webhook 400 branch removed (falls to 500)", "internal/httpserver/casino_handlers.go", "if errors.Is(err, casino.ErrProviderReferenceInvalid) {\n\t\t\t// PROVIDER-REF-BOUND-1: a VERIFIED", "if errors.Is(err, casino.ErrProviderReferenceInvalid) && false {\n\t\t\t// PROVIDER-REF-BOUND-1: a VERIFIED", HTTP_INT),
    ("M13 rejection log line carries err text", "internal/httpserver/provider_reference_log.go", "attrs := []any{\"provider_id\", providerID,", "attrs := []any{\"error\", err.Error(), \"provider_id\", providerID,", HTTP_INT),
    ("M14 payments boundary call removed", "internal/payments/orchestrator.go", "); err != nil {\n\t\treturn ReceiveCallbackResult{}, fmt.Errorf(\"%w: %w\", ErrProviderReferenceInvalid, err)", "); err != nil && false {\n\t\treturn ReceiveCallbackResult{}, fmt.Errorf(\"%w: %w\", ErrProviderReferenceInvalid, err)", PAY_INT),
    ("M15 payments original reference not validated", "internal/payments/orchestrator.go", "providerref.Field{Name: \"original_provider_reference\", Value: event.OriginalProviderReference},", "", PAY_INT),
    ("M16 payments mapping case removed (500)", "internal/httpserver/payment_callback_errors.go", "if errors.Is(err, payments.ErrProviderReferenceInvalid) {\n\t\t// PROVIDER-REF-BOUND-1", "if errors.Is(err, payments.ErrProviderReferenceInvalid) && false {\n\t\t// PROVIDER-REF-BOUND-1", HTTP_INT),
    ("M17 payments webhook branch removed (no allow-listed log)", "internal/httpserver/deposit_handlers.go", "if errors.Is(err, payments.ErrProviderReferenceInvalid) {", "if errors.Is(err, payments.ErrProviderReferenceInvalid) && false {", HTTP_INT),
    ("M18 casino play rollback pre-validation removed", "internal/httpserver/casino_play_handlers.go", "if err := providerref.Validate(\"original_provider_tx_id\", req.OriginalProviderTxID); err != nil {", "if err := providerref.Validate(\"original_provider_tx_id\", req.OriginalProviderTxID); err != nil && false {", HTTP_INT),
    ("M19 sportsbook sync pre-validation removed", "internal/sportsbook/catalogue.go", "if err := validateCatalogueReferences(result); err != nil {", "if err := validateCatalogueReferences(result); err != nil && false {", SB_INT),
    ("M20 sportsbook selection level not validated", "internal/sportsbook/catalogue.go", "if err := check(\"selection.external_ref\", sel.ExternalRef); err != nil {", "if err := check(\"selection.external_ref\", sel.ExternalRef); err != nil && false {", SB_UNIT),
    ("M21 sportsbook settlement asset code not validated", "internal/sportsbook/settlement.go", "if err := providerref.Validate(\"asset_code\", ev.ClaimAssetCode); err != nil {", "if err := providerref.Validate(\"asset_code\", ev.ClaimAssetCode); err != nil && false {", SB_UNIT),
    ("M22 migration CHECK widened to 256 (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 256 AND provider_tx_id !~ '[\\x01-\\x1F\\x7F-\\x9F]')"), CAS_MIG),
    ("M23 migration CHECK control-char clause dropped (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255)"), CAS_MIG),
    ("M24 migration ledger CHECK removed", MIG, "    ADD CONSTRAINT ledger_transactions_provider_tx_id_ref_bound\n        CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1F\\x7F-\\x9F]');", "    ADD CONSTRAINT ledger_transactions_provider_tx_id_ref_bound\n        CHECK (true);", CAS_MIG),
    ("M25 pre-flight RLS-blind (no per-tenant app.tenant_id)", MIG, "PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);", "PERFORM set_config('app.tenant_id', '', true);", CAS_MIG),
    ("M26 pre-flight predicate widened (300 bytes not counted)", MIG, "(octet_length(%1$I) NOT BETWEEN 1 AND 255", "(octet_length(%1$I) NOT BETWEEN 1 AND 300", CAS_MIG),
    ("M27 pre-flight never raises", MIG, "IF grand_total > 0 THEN", "IF grand_total > 1000000 THEN", CAS_MIG),
    ("M28 down leaves a constraint behind", MIGD, "ALTER TABLE sb_sports DROP CONSTRAINT IF EXISTS sb_sports_external_ref_ref_bound;", "", CAS_MIG),
    # Code review F3: the SQL range edges and the minimum length. The Go rule
    # and the SQL rule are independent literals; these prove their parity.
    ("M29 SQL C1 range widened to U+00A0 (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1F\\x7F-\\xA0]')"), CAS_MIG),
    ("M30 SQL DEL dropped (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1F\\x80-\\x9F]')"), CAS_MIG),
    ("M31 SQL C1 upper edge narrowed to U+009E (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1F\\x7F-\\x9E]')"), CAS_MIG),
    ("M32 SQL C0 upper edge narrowed to U+001E (rejections.provider_tx_id)", MIG, REJ_TX, rej_tx("CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\\x01-\\x1E\\x7F-\\x9F]')"), CAS_MIG),
    ("M33 SQL minimum length dropped (ledger provider_tx_id)", MIG, "    ADD CONSTRAINT ledger_transactions_provider_tx_id_ref_bound\n        CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255", "    ADD CONSTRAINT ledger_transactions_provider_tx_id_ref_bound\n        CHECK (octet_length(provider_tx_id) BETWEEN 0 AND 255", CAS_MIG),
]


def run(cmd, env):
    p = subprocess.run(cmd, cwd=ROOT, env=env, capture_output=True, text=True, timeout=900)
    return p.returncode, (p.stdout + p.stderr)


def main():
    env = dict(os.environ)
    missing = [k for k in REQUIRED_ENV if not env.get(k)]
    if missing:
        sys.exit("prh-ref-mutate: set " + ", ".join(missing) + " (a private/local test database) first")
    args = sys.argv[1:]
    out_path = None
    if "--out" in args:
        i = args.index("--out")
        out_path = args[i + 1]
        args = args[:i] + args[i + 2:]
    only = set(args)
    selected = [m for m in MUTANTS if not only or m[0].split()[0] in only]

    out, killed = [], 0
    for name, rel, old, new, cmd in selected:
        path = os.path.join(ROOT, rel)
        with open(path) as f:
            src = f.read()
        if src.count(old) != 1:
            out.append(f"{name}: SETUP ERROR (anchor found {src.count(old)} times in {rel})")
            print(out[-1], flush=True)
            continue
        with open(path, "w") as f:
            f.write(src.replace(old, new, 1))
        try:
            rc, log = run(cmd, env)
        finally:
            with open(path, "w") as f:
                f.write(src)
        if rc != 0:
            killed += 1
        status = "KILLED" if rc != 0 else "SURVIVED"
        first_fail = next((l.strip() for l in log.splitlines() if "--- FAIL" in l or "cannot" in l or "error" in l.lower()), "")
        out.append(f"{name}: {status} [{rel}] by `{' '.join(cmd)}`" + (f"\n    first failure: {first_fail[:200]}" if first_fail else ""))
        print(out[-1], flush=True)

    summary = f"\n{killed}/{len(selected)} killed ({datetime.datetime.now(datetime.timezone.utc).isoformat()})"
    print(summary)
    if out_path:
        with open(out_path, "w") as f:
            f.write("\n".join(out) + summary + "\n")
    sys.exit(0 if killed == len(selected) else 1)


if __name__ == "__main__":
    main()
