package db

import "testing"

// ADR 0115 S1: a connection string can never supply app.environment (any spelling), and an options
// parameter naming it is refused before any connection.
func TestDropURLAppEnvironment(t *testing.T) {
	rp := map[string]string{"app.environment": "test", "APP.Environment": "development", "application_name": "x"}
	if err := dropURLAppEnvironment(rp); err != nil {
		t.Fatal(err)
	}
	if len(rp) != 1 || rp["application_name"] != "x" {
		t.Fatalf("every spelling of app.environment must be dropped: %v", rp)
	}
	for _, opt := range []string{"-c app.environment=test", "-c APP.ENVIRONMENT=development"} {
		if err := dropURLAppEnvironment(map[string]string{"options": opt}); err == nil {
			t.Fatalf("options %q must be refused", opt)
		}
	}
	if err := dropURLAppEnvironment(map[string]string{"options": "-c statement_timeout=5s"}); err != nil {
		t.Fatalf("an unrelated options value is untouched: %v", err)
	}
}
