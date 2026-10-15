package backup_test

// A12 security-review fix round: behavioural unit tests of the guard, identifier validation and the signing library (they execute
// the REAL shell functions), plus textual ordering/shape pins for backup.sh, restore.sh and the hardening SQL.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// guard runs the REAL bk_guard_target from lib-common.sh and reports (exit status, combined output).
func guard(t *testing.T, host, port string, env ...string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := `. "$1"; bk_guard_target "$2" "$3"; echo "LOCAL=${BK_TARGET_LOCAL} HOST=${BK_CANON_HOST}"`
	cmd := exec.Command("bash", "-c", script, "x", filepath.Join(root(t), "deploy/backup/lib-common.sh"), host, port)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

// M5: every spelling of the shared 5432 cluster is refused by the guard, with no acknowledgement that can override it.
func TestGuardCanonicalisesAliasesOfTheSharedCluster(t *testing.T) {
	host, _ := os.Hostname()
	aliases := []string{"127.0.0.1", "localhost", "LOCALHOST", "localhost.", "LocalHost.", "127.0.0.2", "127.1", "127.0.0.01", "0.0.0.0", "::1", "[::1]", "0:0:0:0:0:0:0:1", "::ffff:127.0.0.1", "[::ffff:127.0.0.1]",
		"::", "a,127.0.0.1", "127.0.0.1,a", "127.0.0.1,", " 127.0.0.1", "127.0.0.1 ", "user@127.0.0.1", "127.0.0.1%lo", "/var/run/postgresql", "127.0.0.1/8"}
	if host != "" {
		aliases = append(aliases, host, strings.ToUpper(host), host+".")
	}
	for _, h := range aliases {
		for _, ack := range [][]string{nil, {"BACKUP_ALLOW_REMOTE_TARGET=" + h}} {
			code, out := guard(t, h, "5432", ack...)
			if code == 0 {
				t.Errorf("alias %q on 5432 was ADMITTED (env %v): %s", h, ack, out)
			}
		}
	}
	// local aliases are admitted only on a drill port, and are reported local
	for _, h := range []string{"127.0.0.1", "localhost", "LOCALHOST", "localhost.", "127.0.0.2", "127.1", "::1", "[::1]", "::ffff:127.0.0.1"} {
		code, out := guard(t, h, "5499")
		if code != 0 || !strings.Contains(out, "LOCAL=1") {
			t.Errorf("local alias %q on drill port 5499: code=%d out=%s", h, code, out)
		}
		if code, _ := guard(t, h, "5433"); code == 0 {
			t.Errorf("local alias %q on non-drill port 5433 admitted", h)
		}
	}
	// a remote host needs the exact acknowledgement; 5432 is legitimate there (RDS)
	if code, _ := guard(t, "db.example.invalid", "5432"); code == 0 {
		t.Error("remote host admitted without BACKUP_ALLOW_REMOTE_TARGET")
	}
	if code, _ := guard(t, "db.example.invalid", "5432", "BACKUP_ALLOW_REMOTE_TARGET=other.example.invalid"); code == 0 {
		t.Error("remote host admitted with a different acknowledgement")
	}
	if code, out := guard(t, "DB.Example.Invalid.", "5432", "BACKUP_ALLOW_REMOTE_TARGET=db.example.invalid"); code != 0 || !strings.Contains(out, "LOCAL=0") {
		t.Errorf("remote host with exact acknowledgement (case/trailing dot canonicalised) refused: code=%d %s", code, out)
	}
	if code, _ := guard(t, "", "5499"); code == 0 {
		t.Error("empty host admitted")
	}
	if code, _ := guard(t, "127.0.0.1", "54 99"); code == 0 {
		t.Error("non-numeric port admitted")
	}
}

func TestIdentifierValidation(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	run := func(v string) bool {
		cmd := exec.Command("bash", "-c", `. "$1"; bk_validate_ident X "$2"`, "x", filepath.Join(root(t), "deploy/backup/lib-common.sh"), v)
		return cmd.Run() == nil
	}
	for _, ok := range []string{"igaming_platform_dev", "igaming_backup", "a"} {
		if !run(ok) {
			t.Errorf("valid identifier %q refused", ok)
		}
	}
	for _, bad := range []string{"", "Igaming", "1abc", "a-b", "a;drop", "a b", "a\"b", "a'b", "-c", "a$b", strings.Repeat("a", 64)} {
		if run(bad) {
			t.Errorf("identifier %q accepted", bad)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// H3: the REAL signing library with real Ed25519 keys: a valid set verifies; unsigned, tampered, attacker-signed, in-directory key,
// missing/wrong pin are all refused.
func TestSigningLibrary(t *testing.T) {
	for _, tool := range []string{"bash", "openssl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	set := filepath.Join(dir, "set")
	for _, d := range []string{keys, set} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sh := func(script string, env ...string) (string, error) {
		cmd := exec.Command("bash", "-c", `set -e; . "$1/lib-common.sh"; . "$1/lib-sign.sh"; `+script, "x", filepath.Join(root(t), "deploy/backup"))
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mustSh := func(script string) string {
		out, err := sh(script)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return strings.TrimSpace(out)
	}
	mustSh(`openssl genpkey -algorithm ED25519 -out ` + keys + `/a.pem 2>/dev/null && openssl pkey -in ` + keys + `/a.pem -pubout -out ` + keys + `/a.pub && openssl genpkey -algorithm ED25519 -out ` + keys + `/b.pem 2>/dev/null && openssl pkey -in ` + keys + `/b.pem -pubout -out ` + keys + `/b.pub && chmod 600 ` + keys + `/*.pem`)
	pinA := mustSh(`bk_pubkey_fingerprint ` + keys + `/a.pub`)
	pinB := mustSh(`bk_pubkey_fingerprint ` + keys + `/b.pub`)
	if len(pinA) != 64 || pinA == pinB {
		t.Fatalf("bad fingerprints %q %q", pinA, pinB)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(set, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("db.dump.age", "ciphertext")
	write("manifest.json", "{}")
	reseal := func() { mustSh(`cd ` + set + ` && sha256sum -- db.dump.age manifest.json > SHA256SUMS`) }
	reseal()
	sign := func(key string) (string, error) {
		return sh(`bk_sign_set `+set, "BACKUP_SIGNING_KEY_FILE="+keys+"/"+key)
	}
	if out, err := sign("a.pem"); err != nil {
		t.Fatalf("signing failed: %v %s", err, out)
	}
	verify := func(pub, pin string) error {
		_, err := sh(`bk_verify_set_signature `+set, "RESTORE_VERIFY_PUBKEY_FILE="+pub, "RESTORE_VERIFY_PUBKEY_SHA256="+pin)
		return err
	}
	if err := verify(keys+"/a.pub", pinA); err != nil {
		t.Fatalf("a correctly signed set was refused: %v", err)
	}
	if err := verify(keys+"/b.pub", pinB); err == nil {
		t.Error("a set signed by key A verified against key B")
	}
	if err := verify(keys+"/b.pub", pinA); err == nil {
		t.Error("wrong public key with the right pin accepted")
	}
	if err := verify(keys+"/a.pub", pinB); err == nil {
		t.Error("right key with the wrong pin accepted")
	}
	if err := verify(keys+"/a.pub", ""); err == nil {
		t.Error("missing pin accepted")
	}
	if err := verify(keys+"/a.pub", strings.ToUpper(pinA)); err == nil {
		t.Error("non-lowercase pin accepted")
	}
	// the key must not be read from the backup directory
	if err := os.WriteFile(filepath.Join(set, "a.pub"), mustRead(t, keys+"/a.pub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(filepath.Join(set, "a.pub"), pinA); err == nil {
		t.Error("verification key inside the backup directory accepted")
	}
	_ = os.Remove(filepath.Join(set, "a.pub"))
	// tampering after signing: recomputed SHA256SUMS without re-signing
	write("manifest.json", "{\"x\":1}")
	reseal()
	if err := verify(keys+"/a.pub", pinA); err == nil {
		t.Error("re-computed SHA256SUMS without re-signing was accepted")
	}
	// an attacker re-seals with his own key
	if out, err := sign("b.pem"); err != nil {
		t.Fatalf("attacker signing: %v %s", err, out)
	}
	if err := verify(keys+"/a.pub", pinA); err == nil {
		t.Error("attacker-signed set accepted")
	}
	// unsigned
	_ = os.Remove(filepath.Join(set, "SHA256SUMS.sig"))
	if err := verify(keys+"/a.pub", pinA); err == nil {
		t.Error("unsigned set accepted")
	}
	// the signing key must be private
	if err := os.Chmod(keys+"/a.pem", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sign("a.pem"); err == nil {
		t.Error("a world-readable signing key was accepted")
	}
}

// Ordering properties of the scripts that a behavioural drill can only sample: pin them textually.
func TestSignatureAndEncryptionOrdering(t *testing.T) {
	r := stripComments(read(t, "deploy/backup/restore.sh"))
	sig := strings.Index(r, "bk_verify_set_signature \"$BACKUP_DIR\"")
	for _, later := range []string{`sha256sum --check`, `materialise db.dump`, `pg_restore --list`, `bk_sql "SELECT current_user"`, `CREATE DATABASE`, `age -d`, `gpg --batch`} {
		i := strings.Index(r, later)
		if i < 0 || sig < 0 || sig > i {
			t.Errorf("restore.sh must verify the signature (%d) before %q (%d)", sig, later, i)
		}
	}
	for _, want := range []string{"RESTORE_OWNER_ROLE_OVERRIDE", "--phase", "refusing to run the archive as", "TABLE DATA public database_environment", "RESTORE_EXPECT_SOURCE_ENVIRONMENT", `--role="$OWNER_ROLE"`, "pg_restore --single-transaction --exit-on-error --no-owner"} {
		if !strings.Contains(r, want) {
			t.Errorf("restore.sh lost %q", want)
		}
	}
	b := stripComments(read(t, "deploy/backup/backup.sh"))
	probe, dump := strings.Index(b, "probe.age"), strings.Index(b, `pg_dump -Fc --snapshot=`)
	if probe < 0 || dump < 0 || probe > dump {
		t.Error("backup.sh must probe-encrypt before it dumps")
	}
	if !strings.Contains(b, `pg_dump -Fc --snapshot="$SNAP" 2>`) || strings.Contains(b, "--file=-") || !strings.Contains(b, "set -o pipefail") {
		t.Error("backup.sh must stream pg_dump to the encryptor through a pipe with pipefail and no --file option (--file=- would create a plaintext file literally named minus)")
	}
	if regexp.MustCompile(`age\s[^\n]*&&\s*rm`).MatchString(b) || regexp.MustCompile(`gpg\s[^\n]*&&\s*rm`).MatchString(b) {
		t.Error("backup.sh must not encrypt-then-delete (the plaintext would exist on disk and survive a failure)")
	}
	if regexp.MustCompile(`pg_dump[^\n|]*(\s-f\s|--file=[^-])`).MatchString(b) {
		t.Error("backup.sh writes pg_dump output to a file instead of the encryptor pipe")
	}
	for _, want := range []string{"trap '' PIPE", "trap 'exit 130' INT", "trap 'exit 143' TERM", "trap cleanup EXIT", "bk_sweep_stale", "bk_mktemp_dir", "holder.dead", "BACKUP_KEEPALIVE_SECONDS", "PLAINTEXT", "REAL_ENC", "BACKUP_SIGNING_KEY_FILE", "bk_sign_set"} {
		if !strings.Contains(b, want) {
			t.Errorf("backup.sh lost %q", want)
		}
	}
	// no predictable temporary file names in the executable scripts
	for _, f := range []string{"backup.sh", "restore.sh", "verify.sh", "lib-common.sh", "lib-sign.sh"} {
		body := stripComments(read(t, "deploy/backup/"+f))
		if regexp.MustCompile(`[>"]\s*/tmp/[A-Za-z0-9_.-]+`).MatchString(body) {
			t.Errorf("%s uses a predictable temporary name (use mktemp)", f)
		}
	}
	if !strings.Contains(read(t, "deploy/backup/verify.sh"), "must not embed a password") {
		t.Error("verify.sh must refuse database URLs that embed a password")
	}
	// the unencrypted override is local-only
	if !strings.Contains(read(t, "deploy/backup/backup.sh"), `[ "$BK_TARGET_LOCAL" -eq 1 ]`) {
		t.Error("the unencrypted override must require a local throwaway target")
	}
}

func TestHardenSurfaceSQL(t *testing.T) {
	sql := stripComments(read(t, "deploy/backup/harden-backup-surface.sql"))
	for _, want := range []string{`pg\_advisory`, "pg_logical_emit_message", "pg_notify", "lo_create", "REVOKE ALL ON FUNCTION", "FROM PUBLIC", "igaming_runtime", "REVOKE CONNECT ON DATABASE postgres", "REVOKE CONNECT ON DATABASE template1"} {
		if !strings.Contains(sql, want) {
			t.Errorf("harden-backup-surface.sql lost %q", want)
		}
	}
	// it must never be applied by anything that targets a cluster the drill does not own
	for _, f := range files(t, "deploy/backup/*.sh") {
		body := stripComments(read(t, f))
		if regexp.MustCompile(`(-f|\\i)\s+"?[^\n]*harden-backup-surface\.sql`).MatchString(body) && base(f) != "lib-provision.sh" {
			t.Errorf("%s applies harden-backup-surface.sql; only lib-provision.sh (throwaway clusters) may", f)
		}
	}
}
