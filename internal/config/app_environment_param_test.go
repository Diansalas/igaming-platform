package config

import "testing"

// ADR 0115 S1: every connection carries app.environment = GuardEnvironment(); a Config without an
// explicit APP_ENV (including one not built by Load) says "production".
func TestDatabaseRuntimeParams_AppEnvironmentIsTheGuardedEnvironment(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{}, "production"},
		{Config{Environment: "development"}, "production"},
		{Config{Environment: "development", EnvironmentExplicit: true}, "development"},
		{Config{Environment: "staging", EnvironmentExplicit: true}, "staging"},
		{Config{Environment: "production", EnvironmentExplicit: true}, "production"},
	}
	for _, c := range cases {
		if got := c.cfg.DatabaseRuntimeParams()[RuntimeParamAppEnvironment]; got != c.want {
			t.Errorf("%+v: app.environment = %q, want %q", c.cfg, got, c.want)
		}
	}
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DatabaseRuntimeParams()[RuntimeParamAppEnvironment]; got != "production" {
		t.Fatalf("an unset APP_ENV must reach the session as production, got %q", got)
	}
}
