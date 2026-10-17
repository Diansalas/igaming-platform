-- staging-runner mode seed-actor-proof: write the actorproof_private.actor_proof_keys verification row (the owner-only schema of migration
-- 0166, F1), following the reviewed procedure of
-- docs/runbooks/operational-runbooks.md section 15 (provisioning step 4). Run by modes/seed-actor-proof.sh as the table owner
-- (the RDS master), never as igaming_runtime.
--
--   * the key id arrives as a psql variable (-v ap_kid=...: a key id is not secret);
--   * the secret NEVER appears in a statement text, a -v option, a command line or a log: it is read from a 0600 file on tmpfs by
--     the client (backtick, exactly as the runbook shows) and sent as a BOUND PARAMETER ($2) of an extended-protocol query (\bind),
--     so neither the server's statement logging nor an error CONTEXT carries it;
--   * IDEMPOTENT without ever overwriting: an identical row is reported and left alone; a row for the same kid with a DIFFERENT
--     secret is a conflict that stops the run (rotation is a separate, reviewed procedure); only an absent kid is inserted;
--   * output is ACTOR|... lines with no key material (kid, status, octet length only).
\set ON_ERROR_STOP on

\set ap_hex `cat "$RN_AP_HEX_FILE"`

SELECT (to_regclass('actorproof_private.actor_proof_keys') IS NOT NULL) AS has_table \gset
\if :has_table
\else
\echo ACTOR|state|no_table
\quit
\endif

SELECT (count(*) FILTER (WHERE secret = decode($2, 'hex')) > 0) AS is_same,
       (count(*) FILTER (WHERE secret <> decode($2, 'hex')) > 0) AS is_other
  FROM actorproof_private.actor_proof_keys WHERE kid = $1 \bind :ap_kid :ap_hex \gset

\if :is_same
\echo ACTOR|state|already_present_identical
\elif :is_other
\echo ACTOR|state|conflict_different_secret_for_this_kid
\quit
\else
INSERT INTO actorproof_private.actor_proof_keys (kid, secret) VALUES ($1, decode($2, 'hex')) \bind :ap_kid :ap_hex \g
\echo ACTOR|state|inserted
\endif

-- verification without printing the secret (runbook: SELECT kid, status, octet_length(secret) FROM actorproof_private.actor_proof_keys)
SELECT 'ACTOR|row|' || kid || '|' || status || '|octets=' || octet_length(secret) FROM actorproof_private.actor_proof_keys ORDER BY kid;
