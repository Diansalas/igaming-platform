// Package txscope marks a context as "a pooled database transaction is
// held by this call chain" (ADR 0094 §4.1, INV-POOL).
//
// Every internal/db scope function (db.Pool.With*) passes Mark(ctx) to its
// callback. Code that must never run while a pooled connection is held -
// a secret-store fetch, or any wait for one - checks Held(ctx) and fails
// closed. This is defence in depth: the primary control is the API shape
// (no store-reaching method takes a pgx.Tx). A caller that deliberately
// drops the context inside a callback (context.Background()) escapes it.
//
// There is deliberately no way to unmark a context, and the key type is
// unexported (security review of ADR 0094, condition C6).
package txscope

import "context"

type heldKey struct{}

// Mark returns ctx marked as holding a pooled database transaction.
func Mark(ctx context.Context) context.Context {
	if Held(ctx) {
		return ctx
	}
	return context.WithValue(ctx, heldKey{}, true)
}

// Held reports whether ctx (or any parent) was marked by Mark.
func Held(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(heldKey{}).(bool)
	return v
}
