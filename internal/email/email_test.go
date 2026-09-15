package email

import (
	"context"
	"testing"
)

func TestMockProvider_SendRecordsMessage(t *testing.T) {
	m := NewMockProvider()
	msg := Message{To: "player@example.com", Subject: "Verify", Body: "token-123"}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sent := m.Sent()
	if len(sent) != 1 || sent[0] != msg {
		t.Fatalf("expected the exact message to be recorded, got %+v", sent)
	}
}

func TestMockProvider_RejectsMessageWithNoRecipient(t *testing.T) {
	m := NewMockProvider()
	err := m.Send(context.Background(), Message{Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("expected an error for a message with no recipient")
	}
	if len(m.Sent()) != 0 {
		t.Fatal("expected no message recorded when Send fails")
	}
}
