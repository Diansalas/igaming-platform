# PRH-2 C — Orchestrator independent mutant re-kill (c2d1fc1)

Local run, not CI. Done on 2026-10-03 against a `git archive` of `prh2-c-dep-ref-validate` at
`c2d1fc1`, using the private DB `orch_crk` (since dropped). Each Go mutant was applied to
`internal/payments/drive.go`, then
`-tags integration -count=1 -p 1 -run 'TestDep|TestINVDEP1|TestA7_3|TestReceipt|TestRVLF_N' ./internal/payments/`
was run. The file was restored and `cmp`-verified afterwards. There were no build errors.

| Mutant | Change | Result | Example failing tests |
|---|---|---|---|
| BASE | none | pass (rc=0) | — |
| REDIR (security C-1) | `playerFacingRedirect` forwards for every state (`if false`) | **KILLED** (19 failures) | `TestDepSyncAmount_MismatchDisputesNoPosting/*` |
| ERR (code review F1) | error path skips reference validation (`verr != nil && err == nil`) | **KILLED** (3) | `TestDepRef_ErrorPathWithInvalidReference_ParksNothingPersisted/{oversize,control-character}` |
| BIND (LF F-C1) | T6 binds `""` instead of the validated reference | **KILLED** (2) | `TestDepRef_ErrorPathWithValidReference_AmbiguousNoRedirectReferenceBound`, `TestDepSyncAmount_MissingEcho_ReferenceBound_SweepPollsAndPostsOnce` |
