# Security confirmation — ADRs 0102–0104 revision 2 (2026-09-28)

Security checked the ADR bodies at `12d24ad`, not only their disposition tables. Read-only review.

| ADR | Verdict |
|---|---|
| 0102 revision 2 | **ACCEPT**. C-102-1..9 and the `raise_failed` Part 1 conditions are all met. Ledger-finance's two notes are consistent with them. |
| 0103 revision 2 | **ACCEPT**. C-103-1..6 are all met. |
| 0104 revision 2 | **ACCEPT**. C-104-1..6 are all met, and the resolver fails closed. |

**Workstreams unblocked:** I-core and G1 may start. B may start after A merges.

**Implementation notes (not conditions; security will check them in the diff reviews):**
- **N-1 (I-core): a caller must not be able to degrade a specific P1.**
  - `request_id` comes from the caller-controlled `X-Request-Id` header.
  - The alert constructor must **drop** a non-conforming `request_id` attribute (omit it and count it) and still raise the specific Kind.
  - Otherwise caller input could turn that P1 into a generic `raise_failed`.
  - Test: `X-Request-Id: "a b"` still persists the specific integrity Kind.
- **N-2 (G1): the display-name endpoint is new.** No staff update endpoint exists today, so G1 builds one. It must:
  - change `display_name` only, using a column-allowlisted UPDATE;
  - for another user, require `PermStaffManage` plus `canActOnTenant`, with tenant callers limited to their own tenant's staff;
  - for a self-rename, use only the verified token subject;
  - have a test for each refusal.
- **N-3 (G1): also refuse Unicode format characters in `display_name`,** in both the CHECK and Go. These are the bidi overrides U+202A–U+202E and U+2066–U+2069, and the zero-width characters U+200B–U+200F.

**Info:**
- **0103:** the `UNIQUE (id, tenant_id)` constraint on `casino_launch_sessions` already exists (0080:22), so B must not touch A's table.
- **0104:** the `WithoutTenant` name lookup cannot see tenant staff rows. Tenant staff in an approval chain therefore show `display_name: null`, which is acceptable because it fails closed.
- **SA-3** is still open and owned by 0099/0112. It must be closed before K1 or K2 code.
