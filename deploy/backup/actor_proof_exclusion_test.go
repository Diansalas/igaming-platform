package backup_test

// Static guards for owner decision 5 (ADR 0122 section 10): a LOGICAL backup never carries actor_proof_keys.secret, a restore starts with no actor-proof key
// and is re-keyed through a scripted, reviewed procedure, and no tool in this directory reads or copies the secret column. Textual, like the other static
// tests; the behavioural proof is the drill (deploy/backup/drill.sh, section 3 "actor-proof").

import (
	"regexp"
	"strings"
	"testing"
)

func TestBackupExcludesActorProofSecretAndVerifiesIt(t *testing.T) {
	b := read(t, "deploy/backup/backup.sh")
	for _, want := range []string{
		"--exclude-table-data=public.actor_proof_keys", // the exclusion itself
		"pg_restore --list",                            // the TOC of the very stream that is encrypted is read ...
		" TABLE DATA public actor_proof_keys ",         // ... and a data entry for the table refuses the backup
		" TABLE public actor_proof_keys ",              // the structure must be present (a restore needs the empty table)
		`"actor_proof_keys_data": "${APK_DATA}"`,       // the manifest records the derived fact
		"include_actor_proof_keys",                     // the drill-only negative control
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
}

func TestRestoreNeverRestoresActorProofKeys(t *testing.T) {
	r := read(t, "deploy/backup/restore.sh")
	for _, want := range []string{"-e ' TABLE DATA public actor_proof_keys '", "APK_LEGACY", "legacy set", "rekey-actor-proof.sh", "AP002"} {
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
	for _, want := range []string{"--expect-actor-proof", "actor_proof_keys_empty_after_logical_restore", "actor_proof_key_is_the_new_one", "'^TABLE|actor_proof_keys|'", "-actor-proof", "--retired-kid"} {
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
	for _, bad := range []string{".Sign(", ".Attach(", "ParseKeySet", "ACTOR_PROOF_KEYS", "FROM actor_proof_keys", "FROM public.actor_proof_keys"} {
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
		"deploy/backup/lib-common.sh", "deploy/backup/lib-sign.sh", "deploy/backup/lib-globals.sh", "deploy/backup/pitr-drill.sh", "deploy/backup/seed.sh"} {
		var kept []string
		for _, l := range strings.Split(stripComments(read(t, f)), "\n") {
			kept = append(kept, l)
		}
		if m := secretRef.FindString(strings.Join(kept, "\n")); m != "" {
			t.Errorf("%s refers to actor_proof_keys.secret in code: %q", f, m)
		}
	}
	// the baseline's row text for the key table is the NON-SECRET columns only (no whole-row hash of the secret)
	bl := read(t, "deploy/backup/baseline.sql")
	if !strings.Contains(bl, "(SELECT kid, status, created_at, retired_at FROM public.actor_proof_keys)") {
		t.Error("baseline.sql: the actor_proof_keys row text must cover the non-secret columns only")
	}
	// a planted offender is caught
	if !secretRef.MatchString("SELECT md5(secret) FROM public.actor_proof_keys") {
		t.Error("the secret-reference guard does not bite")
	}
}

// TestBackupRoleKeepsTableSelectResidualIsRecorded pins a drilled finding: a column-level grant on actor_proof_keys (the secret withheld) makes pg_dump fail
// (`LOCK TABLE ... IN ACCESS SHARE MODE` needs table-level SELECT even when the data is excluded), so the backup role keeps the table-level SELECT and the residual (a
// stolen backup CREDENTIAL can read the secret) is recorded next to the grant. A later change that narrows the grant must re-prove the dump in the drill.
func TestBackupRoleKeepsTableSelectResidualIsRecorded(t *testing.T) {
	raw := read(t, "deploy/backup/init-backup-role.sql")
	if !strings.Contains(raw, "a stolen backup CREDENTIAL can still read the signing secret") || !strings.Contains(raw, "LOCK TABLE") {
		t.Error("init-backup-role.sql lost the recorded actor_proof_keys residual (pg_dump LOCK TABLE needs table-level SELECT)")
	}
	if regexp.MustCompile(`(?is)(GRANT|REVOKE)[^;]*actor_proof_keys`).MatchString(stripComments(raw)) {
		t.Error("init-backup-role.sql must not special-case actor_proof_keys grants without re-proving the dump (see the test comment)")
	}
}

func TestRekeyScriptProperties(t *testing.T) {
	r := read(t, "deploy/backup/rekey-actor-proof.sh")
	code := stripComments(r)
	for _, want := range []string{
		`\bind :ap_kid :ap_hex`,             // the secret is a bound parameter
		"decode($2, 'hex')",                 //
		`cat "$AP_SECRET_FILE"`,             // read by psql from the file, never via argv
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
