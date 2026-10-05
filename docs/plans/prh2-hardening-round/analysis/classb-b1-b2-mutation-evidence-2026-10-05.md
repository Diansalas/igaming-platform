# Class-B B1 + B2 mutation-kill evidence (2026-10-05)

Method: per mutant the original file was copied aside (throwaway copy), the mutation applied in the worktree, targeted tests run
(-race -tags integration -count=1 -p 1, private scratch DB), then the original restored and verified byte-identical (cmp). Control run = unmutated tree, same tests.

Tests: TestCallProvider_PanicValueNeverReachesErr (internal/payments/gate_panic_redaction_test.go), TestReceiptT4Drain_* (internal/payments/receipt_t4_drain_integration_test.go).

```
control (unmutated) | rc=0 PASS | restore-cmp=True
    ok  	github.com/Diansalas/igaming-platform/internal/payments	4.184s
D-DRAIN-4: delete callback-path drain call | rc=1 KILLED | restore-cmp=True
    --- FAIL: TestReceiptT4Drain_PendingByMerchantRef_AppliesDeferredSuccess_ExactlyOnce (1.83s)
    --- FAIL: TestReceiptT4Drain_SuccessLandsDuringT4Callback_AppliedByTailDrain (1.90s)
    FAIL
    FAIL	github.com/Diansalas/igaming-platform/internal/payments	3.771s
    FAIL
D-DRAIN-RR: remove fresh re-read before drain | rc=1 KILLED | restore-cmp=True
    --- FAIL: TestReceiptT4Drain_PendingByMerchantRef_AppliesDeferredSuccess_ExactlyOnce (1.74s)
    --- FAIL: TestReceiptT4Drain_SuccessLandsDuringT4Callback_AppliedByTailDrain (1.58s)
    FAIL
    FAIL	github.com/Diansalas/igaming-platform/internal/payments	3.358s
    FAIL
SEC-8-a: %T back to %v (panic value leaks) | rc=1 KILLED | restore-cmp=True
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/string (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/error (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/url_error (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/struct (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/fmt_wrapped (0.00s)
    FAIL
    FAIL	github.com/Diansalas/igaming-platform/internal/payments	0.035s
SEC-8-b: Ambiguous -> NotSent on panic | rc=1 KILLED | restore-cmp=True
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/string (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/error (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/url_error (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/struct (0.00s)
    --- FAIL: TestCallProvider_PanicValueNeverReachesErr/fmt_wrapped (0.00s)
    FAIL
    FAIL	github.com/Diansalas/igaming-platform/internal/payments	0.038s
```
