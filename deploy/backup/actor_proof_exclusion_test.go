package backup_test

// Static guards for owner decision 5 (ADR 0122 section 10) and F1 (migration 0166): the actor-proof key table lives in the owner-only schema
// actorproof_private; a LOGICAL backup excludes that schema (and refuses a stream that carries any of it or any actor_proof_key* data), the backup role
// is refused if it can reach the schema, a restore recreates the schema EMPTY from reviewed DDL and is re-keyed through a scripted, reviewed procedure,
// and no tool in this directory reads or copies the secret column. Textual, like the other static tests; the behavioural proof is the drill
// (deploy/backup/drill.sh: phase 2 "backup_role_cannot_read_actor_proof_key", section 3 "actor-proof").

import (
	"regexp"
	"strings"
	"testing"
)

func TestBackupExcludesActorProofSecretAndVerifiesIt(t *testing.T) {
	b := read(t, "deploy/backup/backup.sh")
	for _, want := range []string{
		"EXCL=(--exclude-schema=actorproof_private)", // the exclusion itself (F1: the whole key schema; pg_dump neither locks nor reads it)
		"pg_restore --list",                          // the TOC of the very stream that is encrypted is read ...
		"TABLE DATA [^ ]+ actor_proof_key[^ ]* ",     // ... and a data entry for a key table (any schema) refuses the backup ...
		"'(^| )actorproof_private( |$)'",             // ... as does ANY entry of the key schema
		" FUNCTION public actor_proof_require(",      // the verifier must be in the dump (a platform database, not an unexpected dump)
		`"actor_proof_keys_data": "${APK_DATA}"`,     // the manifest records the derived fact
		"include_actor_proof_keys",                   // the drill-only negative control
		"can read the actor-proof signing key",       // the preflight refuses a backup role that can reach the key schema ...
		"migration 0166 is not applied",              // ... or a source where the key is still in public
		"has_schema_privilege(n.oid, 'USAGE')",       // privileges read by OID (a name lookup would itself be refused)
	} {
		if !strings.Contains(b, want) {
			t.Errorf("backup.sh lost %q", want)
		}
	}
	// the negative control is gated to a throwaway target like the other fault hooks, and cannot be set silently
	if !strings.Contains(b, `[ -z "$FAULT" ] || [ "$BK_TARGET_LOCAL" -eq 1 ]`) {
		t.Error("backup.sh: the BACKUP_TEST_FAULT gate for throwaway targets is gone")
	}
	// the exclusion must be unconditional apart from the gated fault: exactly one place may empty the exclusion list
	if n := strings.Count(b, "EXCL=()"); n != 1 {
		t.Errorf("backup.sh: expected exactly one EXCL=() (the gated fault), found %d", n)
	}
	if !regexp.MustCompile(`(?s)FAULT" != include_actor_proof_keys \] \|\| EXCL=\(\)`).MatchString(b) {
		t.Error("backup.sh: EXCL=() must be reachable only through the include_actor_proof_keys fault")
	}
	if !regexp.MustCompile(`pg_dump -Fc --snapshot="\$SNAP" "\$\{EXCL\[@\]\}"`).MatchString(b) {
		t.Error("backup.sh: pg_dump must be invoked with the exclusion list")
	}
	// the key-reach preflight can be skipped ONLY by the drill fault on a throwaway target
	if !strings.Contains(b, `if [ "${BACKUP_TEST_FAULT:-}" != include_actor_proof_keys ] || [ "$BK_TARGET_LOCAL" -ne 1 ]; then`) {
		t.Error("backup.sh: the actor-proof key preflight must be skippable only by the include_actor_proof_keys fault on a throwaway target")
	}
	// F1: the table-data-only exclusion of the R1 era must not come back as the mechanism (it needs table-level SELECT for pg_dump's LOCK TABLE)
	if strings.Contains(stripComments(b), "--exclude-table-data=public.actor_proof_keys") {
		t.Error("backup.sh: the key must be excluded with its schema (--exclude-schema=actorproof_private), not by table data")
	}
}

func TestRestoreNeverRestoresActorProofKeys(t *testing.T) {
	r := read(t, "deploy/backup/restore.sh")
	for _, want := range []string{"grep -v -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* '", "grep -v -E '(^| )actorproof_private( |$)'", "APK_LEGACY", "APK_FOREIGN", "legacy set",
		"rekey-actor-proof.sh", "AP002", `-f "${SCRIPT_DIR}/actorproof-private-schema.sql"`, `PGOPTIONS="-c role=${OWNER_ROLE}" psql -X -q -At -1`, "unexpected actor-proof key state",
		"WHEN EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 166) THEN 'private'"} {
		if !strings.Contains(r, want) {
			t.Errorf("restore.sh lost %q", want)
		}
	}
	// the filter must still be what feeds pg_restore -L
	if !strings.Contains(r, `-L "${WORK}/toc.filtered"`) {
		t.Error("restore.sh: pg_restore no longer reads the filtered TOC")
	}
}

func TestVerifyAssertsActorProofState(t *testing.T) {
	v := read(t, "deploy/backup/verify.sh")
	for _, want := range []string{"--expect-actor-proof", "actor_proof_keys_empty_after_logical_restore", "actor_proof_key_is_the_new_one", "'^TABLE|actor_proof_keys|'", "-actor-proof", "--retired-kid",
		"ap_owner_state", `PGUSER="$VERIFY_OWNER_PGUSER"`, "actor_proof_keys_retained_by_physical_recovery", "to_regclass('actorproof_private.actor_proof_keys')"} {
		if !strings.Contains(v, want) {
			t.Errorf("verify.sh lost %q", want)
		}
	}
	// physical recovery keeps the live key: its comparison must NOT drop the table
	if !strings.Contains(v, "if [ \"$ANCHOR_MODE\" = reset ]; then EXPECT_AP=unprovisioned; else EXPECT_AP=skip; fi") {
		t.Error("verify.sh: the default actor-proof expectation must be unprovisioned for a logical restore and skip for physical recovery")
	}
	rc := read(t, "deploy/backup/restorecheck/main.go")
	for _, want := range []string{"VerifyConfiguredInProduction", "actor_proof_verifier_fails_closed", `"AP002"`, "actor_proof_startup_gate_admits_new_kid"} {
		if !strings.Contains(rc, want) {
			t.Errorf("restorecheck lost %q", want)
		}
	}
	// restorecheck must not be a signer or a key reader
	for _, bad := range []string{".Sign(", ".Attach(", "ParseKeySet", "ACTOR_PROOF_KEYS", "FROM actor_proof_keys", "FROM public.actor_proof_keys", "actorproof_private"} {
		if strings.Contains(stripGoLineComments(rc), bad) {
			t.Errorf("restorecheck must neither sign nor read keys: found %q", bad)
		}
	}
}

func stripGoLineComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestNoToolReadsTheActorProofSecret: the baseline, the dump, the restore and the verification never SELECT or hash the secret column. The only code
// that may mention it is the re-key script (which writes it as a bound parameter), the role definition (which keeps it out of the backup role's
// reach of) and the drill (which plants and hunts a canary).
func TestNoToolReadsTheActorProofSecret(t *testing.T) {
	secretRef := regexp.MustCompile(`(?is)actor_proof_keys\.secret|\bsecret\b[^;\n]{0,80}\bFROM\b[^;\n]{0,40}actor_proof_keys|\b(SELECT|COPY|UPDATE)\b[^;]{0,200}actor_proof_keys[^;]{0,60}\bsecret\b`)
	for _, f := range []string{"deploy/backup/backup.sh", "deploy/backup/restore.sh", "deploy/backup/verify.sh", "deploy/backup/baseline.sql", "deploy/backup/post-restore-grants.sql",
		"deploy/backup/lib-common.sh", "deploy/backup/lib-sign.sh", "deploy/backup/lib-globals.sh", "deploy/backup/pitr-drill.sh", "deploy/backup/seed.sh",
		"deploy/backup/actorproof-private-schema.sql"} {
		kept := strings.Split(stripComments(read(t, f)), "\n")
		if m := secretRef.FindString(strings.Join(kept, "\n")); m != "" {
			t.Errorf("%s refers to actor_proof_keys.secret in code: %q", f, m)
		}
	}
	// the baseline's row text for the key table is the NON-SECRET columns only (no whole-row hash of the secret)
	bl := read(t, "deploy/backup/baseline.sql")
	if !strings.Contains(bl, "(SELECT kid, status, created_at, retired_at FROM public.actor_proof_keys)") {
		t.Error("baseline.sql: the (legacy) actor_proof_keys row text must cover the non-secret columns only")
	}
	// F1: the baseline runs as the backup role, which has no access to the key schema: it reads only its CATALOG (structure and privileges)
	if regexp.MustCompile(`(?i)FROM\s+actorproof_private\.`).MatchString(stripComments(bl)) {
		t.Error("baseline.sql must not read a relation of the schema actorproof_private")
	}
	for _, want := range []string{"FP|actorproof_private_md5|", "INV|actor_proof_key_reachable_by_non_owner|"} {
		if !strings.Contains(bl, want) {
			t.Errorf("baseline.sql lost %q", want)
		}
	}
	// the restore DDL creates the key table EMPTY: it writes no key
	ddl := stripComments(read(t, "deploy/backup/actorproof-private-schema.sql"))
	if regexp.MustCompile(`(?i)\b(INSERT|COPY|UPDATE|GRANT)\b`).MatchString(ddl) {
		t.Error("actorproof-private-schema.sql must create the schema and the EMPTY key table only (no INSERT/COPY/UPDATE/GRANT)")
	}
	for _, want := range []string{"CREATE SCHEMA actorproof_private;", "REVOKE ALL ON SCHEMA actorproof_private FROM PUBLIC;", "CREATE TABLE actorproof_private.actor_proof_keys (",
		"REVOKE ALL ON actorproof_private.actor_proof_keys FROM PUBLIC;", "ALTER TABLE actorproof_private.actor_proof_keys ENABLE ROW LEVEL SECURITY;"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("actorproof-private-schema.sql lost %q", want)
		}
	}
	// a planted offender is caught
	if !secretRef.MatchString("SELECT md5(secret) FROM public.actor_proof_keys") {
		t.Error("the secret-reference guard does not bite")
	}
}

// TestBackupRoleHasNoAccessToTheActorProofKeySchema (F1, migration 0166; replaces the R1-era pin that recorded the backup role's table-level SELECT on
// public.actor_proof_keys): the key lives in actorproof_private and the backup role is given NOTHING there. init-backup-role.sql re-asserts the denial on
// every run and grants nothing on that schema; the drill proves pg_dump with --exclude-schema needs no privilege on it.
func TestBackupRoleHasNoAccessToTheActorProofKeySchema(t *testing.T) {
	raw := read(t, "deploy/backup/init-backup-role.sql")
	code := stripComments(raw)
	for _, want := range []string{"REVOKE ALL ON SCHEMA actorproof_private FROM igaming_backup;", "REVOKE ALL ON ALL TABLES IN SCHEMA actorproof_private FROM igaming_backup;",
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA actorproof_private FROM igaming_backup;", "REVOKE ALL ON ALL FUNCTIONS IN SCHEMA actorproof_private FROM igaming_backup;"} {
		if !strings.Contains(code, want) {
			t.Errorf("init-backup-role.sql lost the key-schema denial %q", want)
		}
	}
	if regexp.MustCompile(`(?is)GRANT[^;]*(actorproof_private|actor_proof_key)`).MatchString(code) {
		t.Error("init-backup-role.sql must not grant the backup role anything on the actor-proof key schema or tables")
	}
	if !strings.Contains(raw, "--exclude-schema=actorproof_private") || !strings.Contains(raw, "residual R1") {
		t.Error("init-backup-role.sql lost the explanation of why the backup role needs no privilege on the key schema (and the R1 history)")
	}
}

func TestRekeyScriptProperties(t *testing.T) {
	r := read(t, "deploy/backup/rekey-actor-proof.sh")
	code := stripComments(r)
	for _, want := range []string{
		`\bind :ap_kid :ap_hex`,        // the secret is a bound parameter
		"decode($2, 'hex')",            //
		`\getenv ap_hex AP_SECRET_HEX`, // read ONCE from an open descriptor, handed to psql through the environment (never argv)
		"exec 9<",
		"--first-key",
		"LOCK TABLE actorproof_private.actor_proof_keys IN SHARE ROW EXCLUSIVE MODE",
		"WHERE NOT EXISTS (SELECT 1 FROM actorproof_private.actor_proof_keys)",
		"INSERT INTO actorproof_private.actor_proof_keys (kid, secret)",
		"log_transaction_sample_rate",
		"auto_explain.log_min_duration",
		"pgaudit.log_parameter",
		"must be mode 0600 or 0400",         //
		"must be on tmpfs",                  // a real target never writes the secret to disk
		"equals a retired kid",              // the source's kid is not reusable
		"already holds",                     // first-key provisioning only
		"log_statement",                     // statement logging checked before the bound INSERT
		"log_min_duration_statement",        //
		"log_parameter_max_length_on_error", //
		"does not own actor_proof_keys",     // owner role only
		"bk_connect_env",                    // the same target guard (drill ports / verify-full) as the other tools
	} {
		if !strings.Contains(code, want) {
			t.Errorf("rekey-actor-proof.sh lost %q", want)
		}
	}
	// the secret never enters argv or statement text: no psql -c with the file contents, no echo/printf of it, no variable interpolation into SQL
	for _, bad := range []string{`psql[^\n]*-c[^\n]*\$\(cat`, `echo[^\n]*\$\(cat "?\$SECRET_FILE`, `printf[^\n]*\$\(cat "?\$SECRET_FILE`, `-v ap_hex`, `:'ap_hex'`, `set -x`, `xtrace`} {
		if regexp.MustCompile(bad).MatchString(code) {
			t.Errorf("rekey-actor-proof.sh: forbidden construct %q", bad)
		}
	}
	// it must not copy a key out of a database or generate/print one
	for _, bad := range []string{`(?i)SELECT[^\n]*\bsecret\b[^\n]*FROM`, `openssl\s+rand`, `/dev/urandom`, `pg_dump`} {
		if regexp.MustCompile(bad).MatchString(strings.ReplaceAll(code, "octet_length(secret)", "")) {
			t.Errorf("rekey-actor-proof.sh: forbidden construct %q", bad)
		}
	}
}
