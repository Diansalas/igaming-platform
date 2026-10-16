package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/stagingseed"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func baseArgs(extra ...string) []string {
	return append([]string{"-phase", "plan", "-tenant-slug", "acc-one", "-brand-slug", "acc-one-brand", "-country", "SV",
		"-authorization-reference", "TICKET-43", "-operator", "ops-alice", "-reason", "staging acceptance world", "-credentials-out", "/dev/shm/x.json"}, extra...)
}

func okEnv() map[string]string {
	return map[string]string{"APP_ENV": "staging", "DATABASE_URL": "postgres://igaming@db.example/igaming", "MOCK_GEOLOCATION_COUNTRY": "SV"}
}

func TestParseAcceptsAValidPlan(t *testing.T) {
	s, err := parse(baseArgs(), env(okEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if s.phase != "plan" || s.TenantSlug != "acc-one" || s.Country != "SV" || s.Operator != "ops-alice" {
		t.Fatalf("unexpected settings: %+v", s)
	}
}

func TestParseRefusals(t *testing.T) {
	prod := okEnv()
	prod["APP_ENV"] = "production"
	badCountry := okEnv()
	badCountry["MOCK_GEOLOCATION_COUNTRY"] = "DE"
	noURL := okEnv()
	delete(noURL, "DATABASE_URL")
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"production", baseArgs(), prod, "production"},
		{"unset app env", baseArgs(), map[string]string{"DATABASE_URL": "x"}, "APP_ENV"},
		{"no database url", baseArgs(), noURL, "DATABASE_URL"},
		{"country differs from the mock country", baseArgs(), badCountry, "MOCK_GEOLOCATION_COUNTRY"},
		{"no phase", []string{"-tenant-slug", "a", "-brand-slug", "b", "-country", "SV", "-authorization-reference", "t", "-operator", "o", "-reason", "r", "-credentials-out", "x"}, okEnv(), "phase"},
		{"apply without credentials-in", append(baseArgs()[2:], "-phase", "apply"), okEnv(), "credentials-in"},
		{"plan with credentials-in", baseArgs("-credentials-in", "x"), okEnv(), "credentials-in"},
		{"missing operator", []string{"-phase", "plan", "-tenant-slug", "a", "-brand-slug", "b", "-country", "SV", "-authorization-reference", "t", "-reason", "r", "-credentials-out", "x"}, okEnv(), "operator"},
		{"missing reason", []string{"-phase", "plan", "-tenant-slug", "a", "-brand-slug", "b", "-country", "SV", "-authorization-reference", "t", "-operator", "o", "-credentials-out", "x"}, okEnv(), "reason"},
		{"missing authorization reference", []string{"-phase", "plan", "-tenant-slug", "a", "-brand-slug", "b", "-country", "SV", "-operator", "o", "-reason", "r", "-credentials-out", "x"}, okEnv(), "authorization"},
		{"second tenant without brand", baseArgs("-second-tenant-slug", "acc-two"), okEnv(), "second"},
		{"second tenant equals the first", baseArgs("-second-tenant-slug", "acc-one", "-second-brand-slug", "x"), okEnv(), "differ"},
		{"bad actor id", baseArgs("-actor-id", "not-a-uuid"), okEnv(), "actor-id"},
		{"stray argument", append(baseArgs(), "extra"), okEnv(), "unexpected arguments"},
		{"no credentials-out", []string{"-phase", "plan", "-tenant-slug", "a", "-brand-slug", "b", "-country", "SV", "-authorization-reference", "t", "-operator", "o", "-reason", "r"}, okEnv(), "credentials-out"},
		{"lowercase country", []string{"-phase", "plan", "-tenant-slug", "a", "-brand-slug", "b", "-country", "sv", "-authorization-reference", "t", "-operator", "o", "-reason", "r", "-credentials-out", "x"}, map[string]string{"APP_ENV": "staging", "DATABASE_URL": "x"}, "country"},
	}
	for _, c := range cases {
		_, err := parse(c.args, env(c.env))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

func TestWriteCredsIs0600AndPrintsOnlyACount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	c := stagingseed.Creds{TenantSlug: "acc-one", BrandSlug: "b", Country: "SV", Actors: map[string]*stagingseed.ActorCred{
		"FINANCE1": {Email: "finance1@acc-one.acceptance.invalid", Password: "Sw1-SECRETPASSWORDVALUE0001", BootstrapToken: "TOKEN-SECRET-VALUE-0001"}}}
	var out bytes.Buffer
	if err := writeCreds(path, c, &out); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode %v, want 0600", st.Mode().Perm())
	}
	if strings.Contains(out.String(), "SECRETPASSWORD") || strings.Contains(out.String(), "TOKEN-SECRET") || strings.Contains(out.String(), "finance1@") {
		t.Fatalf("the credentials were printed: %q", out.String())
	}
	if !strings.Contains(out.String(), "1 account(s)") {
		t.Fatalf("expected a count, got %q", out.String())
	}
	// an existing, wider file is narrowed, not trusted
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCreds(path, c, &out); err != nil {
		t.Fatal(err)
	}
	if st, _ = os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("an existing file was not narrowed to 0600: %v", st.Mode().Perm())
	}
}

func TestReadSecretFileRefusesWideModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil {
		t.Fatal("a world-readable credentials file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err != nil {
		t.Fatalf("0600 refused: %v", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err != nil {
		t.Fatalf("0400 refused: %v", err)
	}
	if _, err := readSecretFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file was accepted")
	}
	if _, err := readSecretFile(dir); err == nil {
		t.Fatal("a directory was accepted")
	}
}
