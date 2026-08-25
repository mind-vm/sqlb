package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
)

// A declared query is a domain read with a generated route (the read half of
// ADR-0043).
//
// Unlike [Action], there is no envelope to speak of: a query addresses no
// row, so there is nothing to fetch, no BeforeQuery obligation to inherit and
// no lock to take. What is generated is the route, the parameter binding and
// the response wrapper; do is handed the Executor it was mounted with and is
// on its own for confining what it reads — the position sqlb.Query in
// application code is already in (ADR-0030), and the one [CollectionAction]
// is already in for the same reason.
//
// QuerySpec.Reads is the other half: a table name a client cache can use to
// invalidate this query when a [rest.Event] for that table arrives. That is
// the whole of "opt-in reactive queries" here — a client refetches, the
// server does not push a diffed result — and it costs nothing for a query
// that declares no Reads.

// QuerySpec describes one query to the runtime.
//
// It restates what schema.Query declared, and codegen would write it from
// that declaration — the same arrangement ActionSpec has with schema.Action,
// and for the same reason: nothing on the request path imports the schema
// package.
type QuerySpec struct {
	// Name is the read, used in the operation ID: "overdue" gives
	// overdue-task.
	Name string

	// Path is the full route, resource path included: "/tasks/overdue".
	Path string

	// Field is the name of this query's field on the generated Queries
	// struct, so that a nil func can be reported as the thing the author has
	// to go and set.
	Field string

	// Summary and Description document the operation.
	Summary     string
	Description string

	// Reads names the tables this query reads. Nothing here enforces it — see
	// [schema.Query.Reads].
	Reads []string

	// HasParams reports whether the query declared any parameters. The input
	// type is generated either way, mirroring ActionSpec.HasBody.
	HasParams bool
}

func (s QuerySpec) describe() string {
	if len(s.Reads) == 0 {
		return s.Description
	}
	reach := fmt.Sprintf(
		"This operation reads: %s. That set is declared rather than enforced, and is what a "+
			"client cache uses to invalidate and refetch this query on a change-feed event — see the schema.",
		strings.Join(s.Reads, ", "))
	if s.Description == "" {
		return reach
	}
	return s.Description + "\n\n" + reach
}

func (s QuerySpec) validate(resource string) error {
	switch {
	case s.Name == "":
		return fmt.Errorf("rest: %s declares a query with no Name", resource)
	case s.Path == "":
		return fmt.Errorf("rest: %s query %q has no Path", resource, s.Name)
	}
	return nil
}

func (s QuerySpec) operationID(opts Options) string { return s.Name + "-" + opts.name() }

func missingQueryDo(resource string, spec QuerySpec) error {
	field := spec.Field
	if field == "" {
		field = spec.Name
	}
	return fmt.Errorf(
		"rest: %s declares the query %q, and Queries.%s is nil;\n"+
			"  pass it to Register, e.g. Register(api, db, Queries{%s: %s})\n"+
			"  or drop the query from the schema, which is the honest way to say the read does not exist",
		resource, spec.Name, field, field, lowerFirst(field))
}

// queryOutput carries a query's result, whatever shape do returned.
//
// It is not [itemOutput]: a query's result is not necessarily a row of a
// model with a projection and hidden columns to enforce, so it is rendered
// as-is.
type queryOutput[Out any] struct {
	Body Out
}

// Query registers a declared read at spec.Path.
//
// do receives the Executor Query was mounted with and the decoded
// parameters, and returns whatever the operation answers with. There is no
// fetch, no transaction and no obligation check: a query is exactly as
// confined as the statements do issues, which is why Reads is documentation
// and not enforcement.
func Query[In, Out any](api huma.API, db sqlb.Executor, opts Options, spec QuerySpec, do func(context.Context, sqlb.Executor, In) (Out, error)) error {
	// Not opts.validate(): that check exists for Resource and Action, both of
	// which mount CRUD operations and so require a non-empty Ops. A query
	// mounts no operation of Options' own — Options here is only where its
	// Name, Tag and Security come from — and requiring Ops on it would make
	// every hand-built Options for a query-only mount carry a bitmask that
	// means nothing to the route it names.
	if opts.Path == "" {
		return errors.New("rest: Options.Path is required")
	} else if !strings.HasPrefix(opts.Path, "/") {
		return fmt.Errorf("rest: Options.Path %q must start with a slash", opts.Path)
	}
	if err := spec.validate(opts.Path); err != nil {
		return err
	}
	if db == nil {
		return fmt.Errorf("rest: %s has no Executor", spec.Path)
	}
	if do == nil {
		return missingQueryDo(opts.Path, spec)
	}

	id := spec.operationID(opts)
	if err := refuseDuplicateID(api, opts.Path, spec.Name, id); err != nil {
		return err
	}

	op := huma.Operation{
		OperationID:                  id,
		Method:                       http.MethodGet,
		Path:                         spec.Path,
		Summary:                      spec.Summary,
		Description:                  spec.describe(),
		Tags:                         []string{opts.tag()},
		Security:                     opts.Security,
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(api.OpenAPI().Components.Schemas,
			http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}

	if spec.HasParams {
		// In is registered directly as Huma's input struct — its fields carry
		// the query/path/header tags Huma reflects on, exactly the shape
		// codegen would emit for a declared Params list. There is no wrapper:
		// Go does not allow embedding a type parameter, and In already has
		// the shape Huma wants.
		huma.Register(api, op, func(ctx context.Context, in *In) (*queryOutput[Out], error) {
			out, err := do(ctx, db, *in)
			if err != nil {
				return nil, asHumaError(ctx, err, opts.name())
			}
			return &queryOutput[Out]{Body: out}, nil
		})
		return nil
	}
	huma.Register(api, op, func(ctx context.Context, _ *struct{}) (*queryOutput[Out], error) {
		var in In
		out, err := do(ctx, db, in)
		if err != nil {
			return nil, asHumaError(ctx, err, opts.name())
		}
		return &queryOutput[Out]{Body: out}, nil
	})
	return nil
}
