// Package email is Stage 4F's provider-neutral email-delivery boundary
// (directive §14) - the platform never hard-codes one email vendor
// (SES/SendGrid/Postmark/SMTP/...). Mirrors internal/payments.
// PaymentProvider and internal/casino.CasinoProvider's identical shape: a
// small interface, a mock implementation this stage ships, and a real
// vendor adapter slotting in later without touching any caller.
//
// This package NEVER logs a message's body (which, for the two message
// kinds this stage sends, always contains a raw verification/reset
// token) - only that a send was attempted/succeeded/failed, and to which
// (hashed at the CALLER, never here) recipient. See
// docs/decisions/0030-email-verification-and-password-reset.md §privacy.
package email

import (
	"context"
	"fmt"
	"sync"
)

// Message is a single outbound email. Body is plain text - Stage 4F does
// not implement HTML templating (a future, purely cosmetic addition, out
// of this stage's scope).
type Message struct {
	To      string
	Subject string
	Body    string
}

// Provider is the provider-neutral send boundary every caller uses.
// A real vendor adapter (SES, SendGrid, Postmark, SMTP, ...) implements
// this same interface later - no caller in this codebase changes.
type Provider interface {
	Send(ctx context.Context, msg Message) error
}

// MockProvider is the only implementation this stage ships - it never
// performs real network I/O, so the platform is fully usable in
// dev/test/CI without any production email credentials (directive §14's
// explicit requirement). It records every message it was asked to send,
// for tests to assert against - production code must never read Sent
// (that would be reading another goroutine/request's email content back,
// which a real provider could never do).
type MockProvider struct {
	mu   sync.Mutex
	sent []Message
}

func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

func (m *MockProvider) Send(ctx context.Context, msg Message) error {
	if msg.To == "" {
		return fmt.Errorf("email: message has no recipient")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

// Sent returns a snapshot of every message sent so far, oldest first -
// test-only inspection API.
func (m *MockProvider) Sent() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Message, len(m.sent))
	copy(out, m.sent)
	return out
}
