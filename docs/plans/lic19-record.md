# lic19 record: signed, Person-distinct KYC enforcement policy writes (S4) and the K3 reserved-prefix catalogue

Branch `lic19-kyc-policy-proof` from integ-b `17f6fcc` (migration head 0184). Migration **0185** is new. Software capability only: no policy value is shipped and no regulatory approval is claimed.

## 1. What changed

| Item | Change | Label |
|---|---|---|
| Proof family | `kyc_enforcement_policy:{create,activate,withdraw,supersede}`, scope `platform` (NULL tenant) only, closed set, all step-up `critical`. Mirrored in `actorproof` (claim validation, `StepUpOperations`, platform-operation allow-list), in `actor_proof_require` (0185) and in the pins. `supersede` is the withdrawal of an ACTIVE policy naming its successor; the successor's own activation is an `activate` proof. | IMPLEMENTED |
| Verifier | `actor_proof_require` REPLACED in 0185 with the CURRENT body (0182; no later migration replaced it, verified) plus one marked block and one allow-list term. The down restores the 0182 body byte for byte. | IMPLEMENTED |
| Database enforcement | `kyc_enforcement_policies_enforce_lifecycle` REPLACED with the 0103 body plus one marked block (down restores 0103 byte for byte). A new AFTER INSERT trigger verifies the creation proof. A status change needs: an active `platform_admin` session, the acting administrator linked to a Person, the policy author linked to a Person, the two Persons different (KP003), a non-empty reason code in the single-use setting `app.kyc_policy_reason_code` (KP004), and a verified, consumed proof bound to (policy id, from, to, successor, reason code). A creation proof is bound to every caller-supplied column of the row. | IMPLEMENTED |
| Go signer | New leaf package `internal/kycpolicyproof` (a pinned signer owner, security review mandatory). `kyc` depends on it for the types and defines `kyc.PolicyProof`; the library write functions take a signer and fail closed (`ErrPolicyProofRequired`) without one. The four HTTP handlers attach `kycpolicyproof.Signer{}` (issuer from `actorproof.Default()`), so the route's critical step-up is also enforced by the signer backstop. | IMPLEMENTED |
| lic18 Go-level Person check | Removed: the database now owns it for create, activate, withdraw and supersede (both authors). The route maps KP002, KP003 and AP* to closed 403 sentences. | IMPLEMENTED |
| K3 catalogue | `aml_screening_requests.provider_reference` exempted in `TestK3_C9c_ReservedPrefixCatalogue` (section 4). | IMPLEMENTED |

## 2. Accounts without a Person link (fail closed) and the bootstrap check

Refused with KP002 for create, activate, withdraw and supersede, and also when the AUTHOR of the row has no Person (such a policy can never be activated; author a new one). No bypass was added. Checked for an existing documented bootstrap path:

- Migration 0139 SF014 already REFUSES creating a `platform_admin` without a Person, so a new platform administrator always has one.
- Legacy (pre-0139, not backfilled) unlinked platform administrators are remediated by the existing platform-staff person-link route (`POST /v1/admin/platform-staff/{id}/person-link`, ADR 0119 S5), which is the documented path. It is not a bootstrap exception for these writes: an unlinked account stays refused until linked.
- There is no seeded policy, and no first-policy bootstrap exists or is needed (the platform ships no policy; a jurisdiction with none is dormant, ADR 0096).

Residual: before this migration a platform administrator could change policies with only the account-level four-eyes. After it, an environment whose only platform administrators are unlinked legacy accounts cannot author policies until one is linked. That is intended.

## 3. The static byte pin

`TestMigration0182_Static` (withdrawalpolicy) pins the 0182 FILES (the 0182 up equals the 0177 body plus its block; the 0182 down restores 0177). Those files are unchanged, so that test is still true and was left as it is: re-pointing it at 0185 would stop it pinning 0182. What had to move is the notion of the CURRENT verifier: a new test `TestMigration0185_VerifierIsTheCurrentBodyPlusOneBlock` (internal/kycpolicyproof) pins that the 0185 verifier equals the 0182 body plus exactly the marked block and the allow-list term, that the 0185 down restores the 0182 body (compared to the extracted text AND to a pinned sha256), and that the latest migration defining `actor_proof_require` is 0185, so a future replacement must be re-based on 0185 (the pin fails otherwise). `TestMigration0185_LifecycleIsThe0103BodyPlusOneBlock` does the same for the lifecycle function. The integration test `TestMigration0185_UpDownUp_RestoresTheOlderBodiesByteForByte` compares the catalogue bodies after the down with the 0182 and 0103 file text.

## 4. K3 reserved-prefix catalogue decision

`TestK3_C9c_ReservedPrefixCatalogue` selects columns by NAME (`%provider_reference%`, `%provider_tx_id%`, `settlement_reference`) and requires the reserved-prefix CHECK or an exemption with a reason. It does not forbid the name for non-payment data: `kyc_verifications.provider_reference` and `aml_screening_results.provider_reference` (the same screening vendor reference, migration 0154) are already exempt on that reasoning. Renaming the column was considered and rejected: the data IS a vendor-owned reference, the catalogue does not forbid the name, a rename would split the queue column from `aml_screening_results.provider_reference` and touch the 0181 worker functions, and the reserved prefix protects payment and ledger keys which this column never is (it is stored for audit and read by neither the gate nor the officer list). So the catalogue got an exemption entry with that justification; the test logic is unchanged.

## 5. Pins changed (declared)

- `actorproof` `staticAllowed` gained `internal/kycpolicyproof` (signer owner) and `prooftestImporters` gained `internal/testsupport/kycpolfix` (fixture), each with a comment.
- `TestStepUpOperations_RegistryPinned`: +4 critical operations.
- Existing KYC policy tests (kyc, casino, sportsbook, payments, launchgov, licensingchange) used bare uuids as platform principals and unsigned writes; they now use real synthetic administrators with Persons (`internal/testsupport/kycpolfix`, `kycpolfix/seed`) and signed writes. Assertions were kept; the only semantic additions: the CHECK-on-activation test now activates with a VALID proof so the CHECK is what refuses, and the unwired-trigger test also asserts the refusal is the evaluator-wired rule.
- `kyc` library signatures: `CreateEnforcementPolicyParams.Proof`, `ActivateEnforcementPolicy(…, PolicyWrite)`, `WithdrawEnforcementPolicy(…, PolicyWrite)`, `SupersedeEnforcementPolicyParams.Proof/ReasonCode`, `AuditMeta.Proof`.

## 6. Tests added

Unit: `internal/actorproof/kyc_policy_scope_test.go` (scope matrix, closed set, step-up class), `internal/kycpolicyproof` (0185 static pins, operation mapping, hashes bind every field). Integration: `internal/kyc/policy_signed_four_eyes_integration_test.go` (no proof AP001, proof for other content AP004, replay AP005, no Person KP002 on creation and activation, same Person with a second account KP003, reason code missing KP004 / mismatched AP004, proof for another policy AP004, withdrawal proof cannot activate AP004, library fail-closed without a signer, tenant session still refused), `internal/kyc/migration_0185_integration_test.go` (up, down, up), and the HTTP test from lic18 (`PersonDistinctFourEyes_AndAuditAfterState`) now exercises the database rule.

## 7. Residuals

- The proof is bound to the transition and the reason code; the policy CONTENT of an activation is identified by the row id (content is immutable after insert).
- The `app.kyc_policy_reason_code` setting can be set by any holder of the runtime credential, but it is folded into the signed hash, so it cannot differ from what was signed.
- A holder of the runtime credential cannot forge a proof (the signing key is owner-only); a holder of the SIGNING key could, as for every ADR 0110 operation.
- Back Office pages for these routes remain NOT IMPLEMENTED.

## 8. What was run

All with `priv_test.sh -tags integration -count=1 -p 1` on a private scratch database rebuilt from the final migration set: `./internal/payments -run TestK3` ok (707 s, includes `TestK3_C9c_ReservedPrefixCatalogue`), `./internal/payments -run 'KYC|Cutover|AMLHold'` ok, `./internal/kyc`, `./internal/aml`, `./internal/actorproof`, `./internal/launchgov`, `./internal/db`, `./internal/licensingchange` ok (full packages), `./internal/casino` and `./internal/sportsbook` `-run 'KYC|Kyc'` ok, `./internal/alerting -run 'Kind|Migration0181'` ok, `./internal/httpserver -run 'KYCEnforcementPolicy|Screening|Residence|StepUp|Golden|Route|OpenAPI'` ok. Unit: `gofmt -l`, `go vet ./...`, `go vet -tags integration`, `go test ./...` clean. Full `./internal/payments`, `./internal/httpserver`, `./internal/withdrawal` were NOT run (left to the owner).

Mutation checks against the live scratch database, each killed: Person-distinct comparison removed (`SamePersonDifferentAccountIsRefused`), actor Person-null check removed (`AccountWithoutAPersonLinkIsRefused_FailClosed`), `actor_proof_require` call removed from the lifecycle block and the reason read replaced by a constant (`TransitionProofIsBoundToTheTransitionAndTheReason`), creation trigger disabled (`CreationNeedsAVerifiedProof`).
