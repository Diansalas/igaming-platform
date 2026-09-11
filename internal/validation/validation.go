// Package validation provides small, composable request-validation
// helpers so handlers validate input consistently instead of each
// inventing its own ad hoc checks. This is a Stage 1 foundation - it
// covers primitive checks; domain-specific validation (e.g. bonus
// eligibility rules) belongs to the owning domain specialist in later
// stages.
package validation

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Errors collects field-level validation failures so a handler can report
// all of them at once instead of one-at-a-time.
type Errors struct {
	Fields map[string]string
}

func New() *Errors {
	return &Errors{Fields: map[string]string{}}
}

func (e *Errors) Add(field, message string) {
	e.Fields[field] = message
}

func (e *Errors) HasErrors() bool {
	return len(e.Fields) > 0
}

func (e *Errors) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for field, msg := range e.Fields {
		parts = append(parts, fmt.Sprintf("%s: %s", field, msg))
	}
	return strings.Join(parts, "; ")
}

// RequireNonEmpty adds a field error if the value is empty/whitespace.
func (e *Errors) RequireNonEmpty(field, value string) {
	if strings.TrimSpace(value) == "" {
		e.Add(field, "must not be empty")
	}
}

// RequireUUID adds a field error if value is not a valid UUID.
func (e *Errors) RequireUUID(field, value string) {
	if _, err := uuid.Parse(value); err != nil {
		e.Add(field, "must be a valid UUID")
	}
}

// RequireOneOf adds a field error if value is not among allowed.
func (e *Errors) RequireOneOf(field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	e.Add(field, fmt.Sprintf("must be one of: %s", strings.Join(allowed, ", ")))
}
