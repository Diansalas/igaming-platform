#!/usr/bin/env python3
"""PRH-I5 mutation-kill harness (ADR 0095 §16.3/§16.4).

Applies one anchored, exact-match source mutation at a time, runs the named
test(s), expects them to FAIL, and restores the original file content
(including for untracked files) before the next mutation. DB-level mutations
(dropping a trigger) run as SQL against TEST_DATABASE_URL and are undone by
re-creating the object.

Usage (from the repo root, integration environment exported):
    python3 docs/plans/payment-readiness/evidence/prh-i5-mutate.py
Requires TEST_DATABASE_URL migrated through 0102, TEST_ADMIN_DATABASE_URL for
the scratch-database tests, and psql on PATH for the DB-level mutations.
"""
import os
import subprocess
import sys

PS = "internal/reconciliation/payment_statement.go"
SCHED = "internal/reconciliation/scheduler.go"
MOCK = "internal/payments/mock_statement_source.go"
LIM = "internal/reconciliation/statement/payment_limits.go"
M104 = "migrations/0104_payment_statement_line_charset.up.sql"
M104D = "migrations/0104_payment_statement_line_charset.down.sql"
PKG = "./internal/reconciliation/"

# (id, description, file, old, new, test regex)
SOURCE_MUTATIONS = [
    ("PM1", "pay_duplicate: never detect a duplicate statement line", PS,
     "if i > 0 && lines[i-1].ref == l.ref && lines[i-1].kind == l.kind {",
     "if false && i > 0 && lines[i-1].ref == l.ref && lines[i-1].kind == l.kind {",
     "TestPaymentStatement_Kind_Duplicate$"),
    ("PM2", "pay_duplicate: drop the platform-side >1 succeeded attempt per intent check", PS,
     "HAVING count(*) > 1", "HAVING count(*) > 100",
     "TestPaymentStatement_Kind_DuplicatePlatformSuccess$"),
    ("PM3", "pay_missing_platform_record: an unresolvable line is silently skipped", PS,
     'm.r.add(MismatchKindPayMissingPlatformRecord, lk+" check=resolve", "platform: an attempt of this provider with this reference", m.label+l.render())',
     "_ = l",
     "TestPaymentStatement_Kind_MissingPlatformRecord$"),
    ("PM4", "pay_missing_provider_record: a succeeded attempt with no line is not flagged", PS,
     'case a.state == "succeeded" && m.inCoverage(a.sentAt):',
     'case a.state == "never":',
     "TestPaymentStatement_Kind_MissingProviderRecord$"),
    ("PM5", "pay_amount_mismatch: amounts never compared", PS,
     "} else if a.amount.Cmp(l.amount) != 0 {", "} else if false && a.amount.Cmp(l.amount) != 0 {",
     "TestPaymentStatement_Kind_AmountMismatch_MockNative$"),
    ("PM6", "pay_asset_mismatch: amount checked before asset (asset only when amounts agree)", PS,
     "\tif a.asset != l.asset {\n\t\tm.r.add(MismatchKindPayAssetMismatch",
     "\tif a.asset != l.asset && a.amount.Cmp(l.amount) == 0 {\n\t\tm.r.add(MismatchKindPayAssetMismatch",
     "TestPaymentStatement_Kind_AssetMismatch_CheckedBeforeAmount$"),
    ("PM7", "pay_reference_mismatch: merchant-reference resolution never compares references", PS,
     'if byMerchant && a.providerRef != "" && a.providerRef != l.ref {',
     'if false && byMerchant && a.providerRef != "" && a.providerRef != l.ref {',
     "TestPaymentStatement_Kind_ReferenceMismatch$"),
    ("PM8", "pay_status_mismatch: provider-succeeded vs platform-unposted not flagged (LF-C1(b))", PS,
     'case providerSucceeded && a.state != "succeeded":',
     'case providerSucceeded && a.state == "declined":',
     "TestPaymentStatement_Kind_StatusMismatch_DroppedSuccessCallback$|TestPaymentStatement_LFC1b_"),
    ("PM9", "pay_status_mismatch: provider declined vs platform succeeded not flagged", PS,
     'case l.status == statement.PaymentStatusDeclined && a.state == "succeeded":',
     'case false && l.status == statement.PaymentStatusDeclined && a.state == "succeeded":',
     "TestPaymentStatement_Kind_StatusMismatch_ProviderDeclinedPlatformSucceeded$"),
    ("PM10", "pay_unresolved: the horizon gate removed (in-flight flagged at once)", PS,
     "case !providerSucceeded && inFlight(a.state) && m.aged(a):",
     "case !providerSucceeded && inFlight(a.state):",
     "TestPaymentStatement_Kind_Unresolved_OnlyPastHorizon$|TestPaymentStatement_CleanMockRunOverMixedHistory$"),
    ("PM11", "pay_unresolved: deferred verified receipts never checked (LF95-C5)", PS,
     "AND disposition_at_receipt = 'deferred_unresolved' AND received_at < $3",
     "AND disposition_at_receipt = 'deferred_unresolved' AND received_at < $3 AND false",
     "TestPaymentStatement_UnresolvedDeferredReceipt$"),
    ("PM12", "coverage window ignored for unmatched attempts", PS,
     'case a.state == "succeeded" && m.inCoverage(a.sentAt):',
     'case a.state == "succeeded":',
     "TestPaymentStatement_CoverageWindow_RestartIsNotAFalseAlarm$"),
    ("PM13", "LF95-C13 (a): a succeeded payout without its posting is not flagged", PS,
     "if a.releaseTx == nil || !a.releaseIsCompletion {\n\t\t\t\tm.r.add(MismatchKindPayStatusMismatch",
     "if false {\n\t\t\t\tm.r.add(MismatchKindPayStatusMismatch",
     "TestPaymentStatement_LedgerJoin_SucceededPayoutWithoutPosting$"),
    ("PM14", "LF95-C13 (b): a posting with no succeeded attempt is not flagged", PS,
     "\t\tif n != 1 {\n\t\t\tm.r.add(", "\t\tif n > 1 {\n\t\t\tm.r.add(",
     "TestPaymentStatement_LedgerJoin_PostingWithNoAttempt$|TestPaymentStatement_SnapshotConsistency"),
    ("PM15", "LF95-C13: payout lines no longer match on the settlement reference", PS,
     "a = m.bySettlement[l.ref]", "a = nil",
     "TestPaymentStatement_Payout_SettlementReferenceOnlyIsAMatch$"),
    ("PM16", "INV-IO-14/S95-C1 (MX15 analogue): attempts of every provider are resolvable", PS,
     "WHERE a.tenant_id = $1 AND a.provider_id = $2\n", "WHERE a.tenant_id = $1 AND $2::text IS NOT NULL\n",
     "TestPaymentStatement_CrossProviderLineNeverMatches$"),
    ("PM17", "reversal: a tombstone no longer pairs with a provider reversal line", PS,
     "return // rollback of an unseen original: the tombstone is the platform's record",
     "_ = 0",
     "TestPaymentStatement_Reversal$"),
    ("PM18", "INV-IO-1: the stream calls Fetch under a held transaction", PS,
     "\tif txscope.Held(ctx) {\n\t\treturn statement.PaymentStatement{}, ErrPaymentFetchUnderTx",
     "\tif false && txscope.Held(ctx) {\n\t\treturn statement.PaymentStatement{}, ErrPaymentFetchUnderTx",
     "TestPaymentStatement_FetchRunsWithNoTransactionHeld$"),
    ("PM19", "INV-IO-1: the sweep fetches inside the ingest transaction", SCHED,
     "\t// Phase 1: fetch, no transaction held.\n\tstmt, err := FetchPaymentStatement(ctx, source, tenantID, periodStart, periodEnd, opts)\n\tif err != nil {\n\t\treturn fail(\"fetch\", err)\n\t}",
     "\tvar stmt statement.PaymentStatement\n\tif err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, _ pgx.Tx) error {\n\t\tvar err error\n\t\tstmt, err = FetchPaymentStatement(ctx, source, tenantID, periodStart, periodEnd, opts)\n\t\treturn err\n\t}); err != nil {\n\t\treturn fail(\"fetch\", err)\n\t}",
     "TestPaymentStatement_FetchRunsWithNoTransactionHeld$"),
    ("PM20", "REPEATABLE READ no longer required", PS,
     "\tif err := requireSnapshotIsolationFor(ctx, tx, StreamPaymentStatement); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\treturn runPaymentStatementUnchecked",
     "\treturn runPaymentStatementUnchecked",
     "TestPaymentStatement_RefusesOutsideRepeatableRead$"),
    ("PM21", "S95-C11: the per-import line cap is not enforced", PS,
     "if n := len(stmt.Lines); n > opts.maxLines() {", "if n := len(stmt.Lines); n < 0 {",
     "TestPaymentStatement_LineCapRefusesAndStoresNothing$"),
    # (A first attempt, "ON CONFLICT DO NOTHING" without a target, is an
    # EQUIVALENT mutant - it still absorbs the same unique violation - and
    # survived; recorded in prh-i5-mutation-kill.txt. This is the real one.)
    ("PM22", "ingest not idempotent: the re-fetch of identical content is not absorbed", PS,
     "\t\t ON CONFLICT (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest) DO NOTHING\n",
     "",
     "TestPaymentStatement_IngestIsIdempotent$"),
    ("PM23", "MX9: the stream writes the ledger (ledger.Post from the match)", PS,
     "\tif err := persistRun(ctx, tx, run, mismatches); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\treturn run, mismatches, info, nil",
     "\tif err := persistRun(ctx, tx, run, mismatches); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\tmxp, mxr := \"mx9\", uuid.NewString()\n\tif _, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: tenantID, TransactionType: ledger.TxTombstone, IdempotencyKey: \"mx9:\" + mxr, ProviderID: &mxp, ProviderTxID: &mxr, CorrelationID: uuid.New()}); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\treturn run, mismatches, info, nil",
     "TestPaymentStatement_NoFinancialEffect$"),
    ("PM24", "INV-IO-12: the stream re-drives attempts itself (writes payment_attempts)", PS,
     "\tif err := persistRun(ctx, tx, run, mismatches); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\treturn run, mismatches, info, nil",
     "\tif err := persistRun(ctx, tx, run, mismatches); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\tif _, err := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now(), updated_at = now() WHERE tenant_id = $1 AND next_action_at IS NOT NULL`, tenantID); err != nil {\n\t\treturn Run{}, nil, PaymentStatementInfo{}, err\n\t}\n\treturn run, mismatches, info, nil",
     "TestPaymentStatement_NoFinancialEffect$"),
    ("PM25", "MOCK source renders every tenant's records (tenant tag ignored)", MOCK,
     "if !a.tenantTagged || a.tenantID != cc.TenantID {", "if !a.tenantTagged {",
     "TestPaymentStatement_RLS_CrossTenant$"),
    # --- fix round (RV-PRH-I5 RM2-RM6, F1-F5; security C1/C2) ---
    ("PM29", "RM2: posted reversal vs provider reversal still pending/declined not flagged", PS,
     "if l.status == statement.PaymentStatusPending || l.status == statement.PaymentStatusDeclined {",
     "if false {",
     "TestPaymentStatement_Reversal_PostedButProviderPendingAndAsset$"),
    ("PM30", "RM3: two lines resolving one attempt are both matched (duplicate_match removed)", PS,
     "if first, seen := m.matchedBy[a.id]; seen {", "if first, seen := m.matchedBy[a.id]; seen && false {",
     "TestPaymentStatement_Payout_TwoLinesForOneAttemptIsDuplicate$"),
    ("PM31", "RM4: payout lookup via the separate SettlementReference field removed", PS,
     'if a == nil && l.settlement != "" {', 'if false && a == nil && l.settlement != "" {',
     "TestPaymentStatement_Payout_SettlementReferenceField$"),
    ("PM32", "RM5: reversal asset never compared", PS,
     "\tif rev.asset != l.asset {", "\tif false && rev.asset != l.asset {",
     "TestPaymentStatement_Reversal_PostedButProviderPendingAndAsset$"),
    ("PM33", "RM6: ledger join (b) ignores withdrawal_completed postings", PS,
     'if (t.txType != "deposit" && t.txType != "withdrawal_completed") || !m.inCoverage(t.postedAt) {',
     'if t.txType != "deposit" || !m.inCoverage(t.postedAt) {',
     "TestPaymentStatement_AttemptLinkedPostingsStayFullyChecked$"),
    ("PM34", "F1: legacy exclusion widened to every posting with no succeeded attempt", PS,
     "if n == 0 && t.legacyUnattempted {", "if n == 0 {",
     "TestPaymentStatement_LedgerJoin_PostingWithNoAttempt$|TestPaymentStatement_AttemptLinkedPostingsStayFullyChecked$"),
    ("PM35", "F1: legacy exclusion removed (every legacy posting a P1 again)", PS,
     "if n == 0 && t.legacyUnattempted {", "if false {",
     "TestPaymentStatement_LegacyUnattemptedPostingsAreCountedNotFlagged$"),
    ("PM36", "F1: a withdrawal with a payout attempt still counted as legacy", PS,
     "\t\t              AND NOT EXISTS (SELECT 1 FROM payment_attempts pa WHERE pa.tenant_id = wr.tenant_id AND pa.withdrawal_request_id = wr.id))",
     "\t\t              AND true)",
     "TestPaymentStatement_AttemptLinkedPostingsStayFullyChecked$"),
    ("PM37", "F2: ageing coverage-gated again", PS,
     "\t\tcase inFlight(a.state) && m.aged(a):", "\t\tcase inFlight(a.state) && m.aged(a) && m.inCoverage(a.sentAt):",
     "TestPaymentStatement_Unresolved_NotCoverageGated$"),
    ("PM38", "F3: unposted declined/pending reversal flagged again", PS,
     "if l.status == statement.PaymentStatusSucceeded || l.status == statement.PaymentStatusReversed {",
     "if true {",
     "TestPaymentStatement_Reversal_UnpostedDeclinedOrPendingIsNotAFinding$"),
    ("PM39", "F4: payout settlement_reference never compared with the ledger", PS,
     'if op == "payout" && l.settlement != "" && a.settlementRef != "" && l.settlement != a.settlementRef {',
     'if false {',
     "TestPaymentStatement_Payout_SettlementReferenceField$"),
    ("PM40", "F5: coverage end not bounded at fetch", PS,
     "stmt.CoverageEnd.After(limit) {", "stmt.CoverageEnd.After(limit.Add(24 * time.Hour)) {",
     "TestPaymentStatement_CoverageEndBoundedAtFetch$"),
    ("PM41", "C2: merchant_reference control-character rule removed", PS,
     'if err := providerref.ValidateOptional("merchant_reference", l.MerchantReference); err != nil {',
     'if err := error(nil); err != nil {',
     "TestPaymentStatement_ControlCharactersRefused$"),
    ("PM42", "C2: asset_code shape loosened to any 1..16 bytes", PS,
     "var paymentAssetCodeRE = regexp.MustCompile(`^[A-Z0-9]{1,16}$`)",
     "var paymentAssetCodeRE = regexp.MustCompile(`^(?s:.{1,16})$`)",
     "TestPaymentStatement_ControlCharactersRefused$"),
    ("PM43", "C1: a source's streaming-cap sentinel is not treated as over-cap", PS,
     "if errors.Is(err, statement.ErrPaymentStatementBodyTooLarge) || errors.Is(err, statement.ErrPaymentStatementTooManyLines) {",
     "if false {",
     "TestPaymentStatement_SourceStreamingCapRefuses$"),
    ("PM44", "C1: the MOCK ignores its configured streaming line cap", MOCK,
     "lines := statement.NewPaymentLineCollector(maxLines)", "lines := statement.NewPaymentLineCollector(0)",
     "TestPaymentStatement_SourceStreamingCapRefuses$"),
    ("PM45", "C1: the body limiter truncates silently instead of failing", LIM,
     "\tif b.read > b.max {", "\tif false && b.read > b.max {",
     "TestLimitPaymentStatementBody_|TestDecodePaymentStatementJSONLines_", "./internal/reconciliation/statement/"),
    ("PM46", "C1: the line collector never refuses", LIM,
     "\tif len(c.lines) >= c.max {", "\tif false && len(c.lines) >= c.max {",
     "TestPaymentLineCollector_|TestDecodePaymentStatementJSONLines_", "./internal/reconciliation/statement/"),
    # --- migration 0104 (security C2, database half); scratch-database tests ---
    ("PM47", "0104: merchant_reference CHECK drops the control-character rule", M104,
     "AND merchant_reference !~ '[\\x01-\\x1F\\x7F-\\x9F]')),", "AND true)),",
     "TestMigration0104_ChecksRefuseBadCharsetDirectly$"),
    ("PM48", "0104: asset_code CHECK loosened", M104,
     "        CHECK (asset_code ~ '^[A-Z0-9]{1,16}$');", "        CHECK (asset_code ~ '^.{1,16}$');",
     "TestMigration0104_ChecksRefuseBadCharsetDirectly$"),
    ("PM49", "0104: pre-flight never aborts", M104,
     "    IF total_merchant + total_asset > 0 THEN", "    IF false THEN",
     "TestMigration0104_PreflightCountsAndChangesNothing$"),
    ("PM50", "0104: pre-flight counts without scoping each tenant (blinded by FORCE RLS)", M104,
     "        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);\n", "",
     "TestMigration0104_PreflightCountsAndChangesNothing$"),
    ("PM51", "0104 down: the asset_code CHECK is left behind", M104D,
     "    DROP CONSTRAINT payment_statement_lines_merchant_reference_charset,\n    DROP CONSTRAINT payment_statement_lines_asset_code_shape;",
     "    DROP CONSTRAINT payment_statement_lines_merchant_reference_charset;",
     "TestMigration0104_UpDownUpRoundTrip$"),
]

MX9_IMPORT_OLD = '\t"github.com/Diansalas/igaming-platform/internal/providerref"\n'
MX9_IMPORT_NEW = '\t"github.com/Diansalas/igaming-platform/internal/ledger"\n\t"github.com/Diansalas/igaming-platform/internal/providerref"\n'

# (id, description, drop SQL, restore SQL, test regex)
DB_MUTATIONS = [
    ("PM26", "append-only: the lines UPDATE/DELETE trigger dropped",
     "DROP TRIGGER payment_statement_lines_immutable ON payment_statement_lines",
     "CREATE TRIGGER payment_statement_lines_immutable BEFORE UPDATE OR DELETE ON payment_statement_lines FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation()",
     "TestPaymentStatement_StoreIsAppendOnly$"),
    ("PM27", "line count not bound to stored rows (statement-level cap trigger dropped)",
     "DROP TRIGGER payment_statement_lines_count_cap ON payment_statement_lines",
     "CREATE TRIGGER payment_statement_lines_count_cap AFTER INSERT ON payment_statement_lines REFERENCING NEW TABLE AS new_lines FOR EACH STATEMENT EXECUTE FUNCTION payment_statement_lines_count_cap()",
     "TestPaymentStatement_StoreIsAppendOnly$"),
    ("PM28", "short import accepted (deferred exact-count trigger dropped)",
     "DROP TRIGGER payment_statement_imports_count_exact ON payment_statement_imports",
     "CREATE CONSTRAINT TRIGGER payment_statement_imports_count_exact AFTER INSERT ON payment_statement_imports DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payment_statement_imports_count_exact()",
     "TestPaymentStatement_OversizedFieldsRefused$"),
]


def builds(pkg):
    """Code review note: a mutant that does not compile must not count as
    killed. go vet compiles the package and its integration tests."""
    return subprocess.run(["go", "vet", "-tags=integration", pkg], capture_output=True, text=True).returncode == 0


def go_test(regex, pkg=PKG):
    r = subprocess.run(["go", "test", "-tags=integration", pkg, "-run", regex, "-count=1"],
                       capture_output=True, text=True)
    out = r.stdout + r.stderr
    detail = [ln.strip() for ln in out.splitlines() if "_test.go:" in ln][:2]
    return r.returncode, detail


def psql(sql):
    subprocess.run(["psql", os.environ["TEST_DATABASE_URL"], "-v", "ON_ERROR_STOP=1", "-q", "-c", sql], check=True)


def main():
    killed = survived = 0
    for mut in SOURCE_MUTATIONS:
        mid, desc, path, old, new, regex = mut[:6]
        pkg = mut[6] if len(mut) > 6 else PKG
        orig = open(path).read()
        if orig.count(old) != 1:
            print(f"{mid} ANCHOR-NOT-UNIQUE ({orig.count(old)}): {desc}")
            survived += 1
            continue
        mutated = orig.replace(old, new)
        if mid == "PM23":
            mutated = mutated.replace(MX9_IMPORT_OLD, MX9_IMPORT_NEW, 1)
        open(path, "w").write(mutated)
        try:
            if not builds(pkg):
                print(f"{mid} BUILD-FAIL (not counted as killed): {desc}")
                survived += 1
                continue
            rc, detail = go_test(regex, pkg)
        finally:
            open(path, "w").write(orig)
        status = "KILLED" if rc != 0 else "SURVIVED"
        killed += rc != 0
        survived += rc == 0
        print(f"{mid} {status}: {desc}\n    file: {path}\n    test: {regex}")
        for d in detail:
            print(f"    fail: {d}")
    for mid, desc, drop, restore, regex in DB_MUTATIONS:
        psql(drop)
        try:
            rc, detail = go_test(regex)
        finally:
            psql(restore)
        status = "KILLED" if rc != 0 else "SURVIVED"
        killed += rc != 0
        survived += rc == 0
        print(f"{mid} {status}: {desc}\n    sql: {drop}\n    test: {regex}")
        for d in detail:
            print(f"    fail: {d}")
    rc, _ = go_test("PaymentStatement|Migration0102|Migration0104")
    rc2, _ = go_test(".", "./internal/reconciliation/statement/")
    rc = rc or rc2
    print(f"\nbaseline after restore: {'PASS' if rc == 0 else 'FAIL'}")
    print(f"total: {killed} killed, {survived} survived")
    sys.exit(0 if survived == 0 and rc == 0 else 1)


if __name__ == "__main__":
    main()
