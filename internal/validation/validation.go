// Package validation carries per-field problems from domain services to
// handlers, which answer 400 validation_failed with details.
package validation

import (
	"strings"
)

// Field is one problem: Name is the camelCase API field (or a JSON pointer),
// Message says what's wrong with it.
type Field struct {
	Name    string
	Message string
}

// Error collects field problems. Services build one with Add and return
// OrNil, so success stays a nil error.
type Error struct {
	Fields []Field
}

// Add records a problem for a field.
func (e *Error) Add(name, msg string) {
	e.Fields = append(e.Fields, Field{Name: name, Message: msg})
}

// OrNil returns nil when nothing was recorded, otherwise the Error itself.
func (e *Error) OrNil() error {
	if e == nil || len(e.Fields) == 0 {
		return nil
	}
	return e
}

func (e *Error) Error() string {
	msgs := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		msgs = append(msgs, f.Name+": "+f.Message)
	}
	return "validation failed: " + strings.Join(msgs, "; ")
}
