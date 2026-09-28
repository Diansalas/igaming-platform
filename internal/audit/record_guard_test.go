package audit

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestRecord_RefusesBothTenantAndSubjectTenantSet is ADR 0104 §4's Go-side
// guard, in front of any SQL: "audit.Record refuses an entry with both
// TenantID and SubjectTenantID set." A nil tx proves this check runs
// before Record ever touches the transaction - if it didn't, this would
// panic on a nil pointer dereference instead of returning a clean error.
func TestRecord_RefusesBothTenantAndSubjectTenantSet(t *testing.T) {
	err := Record(context.Background(), nil, Entry{
		TenantID: uuid.New(), SubjectTenantID: uuid.New(),
		ActorType: ActorStaff, ActorID: uuid.New(), Action: "test.both_set", Outcome: OutcomeSuccess,
	})
	if err == nil {
		t.Fatal("expected an error when both TenantID and SubjectTenantID are set")
	}
}
