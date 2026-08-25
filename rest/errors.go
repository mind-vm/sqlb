package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// Problem is the body of every rejection this package produces.
//
// It is RFC 9457 shaped, like Huma's own, so a generated client sees one error
// type across the whole API. The one addition is `allowed` on each detail,
// which carries what the caller could have asked for instead — the substance of
// ADR-0011. Huma's own ErrorDetail has no room for it, and flattening the
// allow-list into the message would leave a client parsing prose to recover.
//
// A handler returning this value has it marshalled directly, because it
// satisfies huma.StatusError.
type Problem struct {
	// Type is the RFC 9457 problem type URI.
	Type string `json:"type,omitempty" doc:"A URI reference identifying the problem type"`
	// Title is the short, human-readable summary of the problem.
	Title string `json:"title,omitempty" doc:"Short, human-readable summary of the problem"`
	// Status is the HTTP status code.
	Status int `json:"status,omitempty" doc:"HTTP status code"`
	// Detail explains this specific occurrence.
	Detail string `json:"detail,omitempty" doc:"Explanation specific to this occurrence"`
	// Errors lists every problem found, not just the first, so a malformed
	// request takes one round trip to fix rather than one per mistake.
	Errors []*ProblemDetail `json:"errors,omitempty" doc:"Every problem found with the request"`
}

// ProblemDetail is one rejected parameter or field.
type ProblemDetail struct {
	// Message says what was wrong.
	Message string `json:"message" doc:"What was wrong"`
	// Location is a path-like pointer to the offending input, e.g.
	// `query.sort` or `body.title`.
	Location string `json:"location,omitempty" doc:"Where the problem is, e.g. 'query.sort'"`
	// Value is the rejected value, echoed back.
	Value any `json:"value,omitempty" doc:"The rejected value"`
	// Allowed lists what would have been accepted instead, where there is a
	// finite set. Hidden columns never appear here: the diagnostic must not
	// become an oracle for what a resource is concealing.
	Allowed []string `json:"allowed,omitempty" doc:"What would have been accepted instead"`
}

// Error satisfies the error interface.
func (e *Problem) Error() string {
	if len(e.Errors) == 0 {
		return e.Detail
	}
	return fmt.Sprintf("%s (%d problems)", e.Detail, len(e.Errors))
}

// GetStatus satisfies huma.StatusError, which is what makes Huma write this
// model rather than converting it to its own.
func (e *Problem) GetStatus() int { return e.Status }

// ContentType marks the body as an RFC 9457 problem document.
func (e *Problem) ContentType(string) string { return "application/problem+json" }

// newError builds a problem document with no per-field detail.
func newError(status int, detail string) *Problem {
	return &Problem{
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
}

// invalidQuery converts filter's parse failures into a problem document,
// preserving the allow-lists that make a rejection actionable.
//
// The status is 400 rather than Huma's usual 422 for validation, matching
// filter.Errors.StatusCode: these are malformed query parameters, and the
// resource has no way to represent them as a well-formed entity that failed
// semantic checks.
func invalidQuery(errs filter.Errors) *Problem {
	out := newError(http.StatusBadRequest, "one or more query parameters were rejected")
	for _, e := range errs {
		detail := &ProblemDetail{
			Message:  e.Reason,
			Location: "query." + e.Param,
			Allowed:  e.Allowed,
		}
		if e.Value != "" {
			detail.Value = e.Value
		}
		out.Errors = append(out.Errors, detail)
	}
	return out
}

// asHumaError maps an error from the engine onto a response.
//
// Everything this package can classify is mapped. Anything it cannot becomes a
// 500 whose body says only that, with the error itself logged rather than
// returned — because an unrecognised database error is a bug here or an outage
// there, and the engine annotates it with the statement that failed. That
// annotation is exactly what a log needs and exactly what a response must not
// carry: it names tables, columns and constraints to whoever provoked it, and
// provoking it is as easy as posting a duplicate value.
func asHumaError(ctx context.Context, err error, resource string) error {
	var constraint *sqlb.ConstraintError

	switch {
	case err == nil:
		return nil
	case errors.Is(err, sqlb.ErrNotFound):
		return newError(http.StatusNotFound, fmt.Sprintf("no %s matched", resource))
	case errors.As(err, &constraint):
		return constraintProblem(constraint, resource)
	case errors.Is(err, sqlb.ErrBadCursor):
		// A cursor is a value the client was handed, so a bad one is a bad
		// request. The commonest way to reach it is changing ?sort= while
		// keeping the cursor, and the engine's message says so, so it is
		// carried through as the detail rather than replaced.
		return &Problem{
			Title:  http.StatusText(http.StatusBadRequest),
			Status: http.StatusBadRequest,
			Detail: "the cursor cannot be used for this request",
			Errors: []*ProblemDetail{{
				Message:  strings.TrimPrefix(err.Error(), "sqlb: "),
				Location: "query.cursor",
			}},
		}
	}
	if errs, ok := filter.AsErrors(err); ok {
		return invalidQuery(errs)
	}
	// An error that already carries a status is an answer application code
	// chose — a hook returning huma.Error403Forbidden because the caller lacks
	// a role, say. It is not an unclassified failure, and replacing it with a
	// generic 500 would turn every deliberate refusal a hook makes into an
	// apparent outage.
	var status huma.StatusError
	if errors.As(err, &status) {
		return err
	}
	// The log line names the fix, because this is the exact moment someone is
	// confused: a hook that returned a plain error to refuse a request reaches
	// here, and a deliberate refusal answered as 500 reads as an outage rather
	// than as the boundary working. The comment above says the same thing to a
	// reader of this file; the reporter of #293 shipped the 500 and found it
	// only by asserting on a status code, which is the tell that the sentence
	// was in the wrong place.
	slog.ErrorContext(ctx, "rest: unclassified error answering as 500",
		"resource", resource, "err", err,
		"hint", "a hook that meant to refuse should return a huma.StatusError — "+
			"huma.Error403Forbidden(...) for a caller who may not, huma.Error422UnprocessableEntity(...) "+
			"for a body that is wrong — which is carried through instead of being read as a failure")
	return newError(http.StatusInternalServerError, "the request could not be completed")
}

// constraintProblem answers a refused write in the terms of the request that
// caused it.
//
// A unique or exclusion violation is 409: the request is well formed and would
// be valid against a different state of the database. The others are 422: the
// entity itself is wrong, and no amount of waiting makes a row referencing a
// product that does not exist into a row that does.
//
// The constraint's name is deliberately not in the body. It is available to Go
// callers on sqlb.ConstraintError, which is where branching on it belongs; put
// in a response it becomes a way to enumerate a schema's indexes by provoking
// them, and ADR-0006's whole position is that a rejection names what the API
// accepts rather than what the database contains.
func constraintProblem(e *sqlb.ConstraintError, resource string) *Problem {
	switch e.Kind {
	case sqlb.ConstraintUnique, sqlb.ConstraintExclusion:
		return newError(http.StatusConflict,
			fmt.Sprintf("this %s conflicts with one that already exists", resource))
	default:
		return newError(http.StatusUnprocessableEntity,
			fmt.Sprintf("this %s breaks a rule the database enforces", resource))
	}
}

// errorResponses documents the failures an operation can produce.
//
// They are set on the Operation rather than left to Huma's Errors field,
// because Huma would document its own error model there and this package
// answers with a different one.
func errorResponses(reg huma.Registry, codes ...int) map[string]*huma.Response {
	schema := reg.Schema(reflect.TypeFor[Problem](), true, "Problem")
	out := make(map[string]*huma.Response, len(codes))
	for _, code := range codes {
		out[fmt.Sprint(code)] = &huma.Response{
			Description: http.StatusText(code),
			Content: map[string]*huma.MediaType{
				"application/problem+json": {Schema: schema},
			},
		}
	}
	return out
}
