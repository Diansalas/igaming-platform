> **SUPERSEDED by `psp-final-recommendation-2026-10-19.md` (official documentation now read).**

# PSP decision matrix (provisional) — Nuvei vs Adyen, 2026-10-18

**Provisional, not final.** The official documentation of both could not be read (network policy), so no cell is `VERIFIED`. Cells marked (S) come from web-search summaries of official pages; (H) from model knowledge. The classification is therefore **provisional** and must be re-issued after the A..Z verification.

| | Nuvei | Adyen (alternative) |
|---|---|---|
| Deposits | Hosted/Web SDK/redirect; session + payment; DMN result (S) | Sessions/Drop-in hosted; webhook result (H) |
| Payouts | `/payout` to a vaulted UPO created by hosted account capture; raw card data needs PCI (S) | Payout API with stored details / review step (H) |
| Destination model | Provider-vaulted token (G-10, significant platform design) (S) | Stored-payment-method token (same G-10 design question) (H) |
| Webhook security | Checksum in body, SHA-256 or MD5 per site, timestamp covered, our reference not covered (S) | HMAC-SHA256, our `merchantReference` covered (H), per-endpoint key (S), **no timestamp** (H) |
| Status query | Not established | Not established (likely absent by reference) (H) |
| Refunds | Full/partial, async pending DMN (S) | Full/partial (H) |
| Chargebacks | Dispute + pre-chargeback DMN types (S) | Dispute events/API (H) |
| Reconciliation | File reports/SFTP, no statement API found (S) | Reports API/SFTP, report-only user (H) |
| Sandbox | Test account issued on onboarding; test cards, Postman (S) | Self-service test account (H) |
| Credentials | merchantId, merchantSiteId, secret (+SFTP) (S) | API key, merchant account, HMAC key (+report user) (H) |
| Platform changes | G-01, G-03, G-07, G-08, G-09, G-10, N-1 rule, SHA-256 pin | G-02 (replay control), G-07 (if SFTP), G-08, G-10; N-1 solved by signed reference if confirmed |
| Security concerns | N-1 unsigned reference; MD5 option; two signing schemes | no signed timestamp; confirm-by-query unavailable |
| **Overall (provisional)** | **AMBER** | **AMBER** (UNRATED until read) |

## Classification rule (applied after verification)
- GREEN: every kill test passes with no platform change.
- AMBER: compatible, needs controlled provider-neutral platform/adapter work (reviewed).
- RED: fails a kill test without weakening controls. Kill tests: (1) no PAN on our servers; (2) DMN success positively bindable to our reference before any financial effect (signed reference, or status query returning reference+amount+currency); (3) replay protection achievable (signed timestamp or a security-approved equivalent); (4) payout destination bindable to player+brand+attempt, immutable, with echo; (5) read-only retrievable reconciliation source; (6) merchant eligibility (owner).

## Recommended first PSP
**None can be recommended yet.** Honest reading: both are AMBER on what is known; the choice turns on kill tests (2), (3) and (4), which need the documentation. Prepare both in parallel (documentation access for both hosts, test accounts requested for both); pick the first PSP by kill-test results, not by ease of integration.
