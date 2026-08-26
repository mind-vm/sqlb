package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// itemInput addresses a single row.
//
// The path template is always `{id}` whatever the primary key column is called,
// because the URL names the resource's identity rather than its storage. The
// column name is what the predicate uses.
type itemInput struct {
	ID string `path:"id" doc:"Primary key of the row"`
}

// expandableInput is itemInput for a resource that declares a relation.
//
// It exists as a second type rather than as a field on itemInput because
// huma builds its known-parameter set from the input struct, not from
// Operation.Parameters — so a field here is what makes `?expand` a parameter
// the operation *has*, and its absence is what makes RejectUnknownQueryParameters
// refuse the parameter on a resource with nothing to expand. Declaring it
// unconditionally would document a parameter that always fails, and answer
// `?expand=org` with a 200 and no relation on every resource that forgot to
// reject it.
type expandableInput struct {
	ID     string   `path:"id" doc:"Primary key of the row"`
	Expand []string `query:"expand" doc:"Relations to embed."`
}

type itemOutput[T any] struct {
	Body row[T]
}

type createdOutput[T any] struct {
	Body row[T]
}

type createInput[C any] struct {
	Body C
}

type updateInput[U any] struct {
	ID   string `path:"id" doc:"Primary key of the row"`
	Body U
}

// key coerces a path segment into the primary key's Go type, so that a uuid
// binds as a uuid. Postgres will not compare a uuid column to text, so getting
// this wrong is an error from the driver rather than an empty result.
func (b *binding[T]) key(raw string) (any, error) {
	v, err := filter.Coerce(raw, b.model.PK.Type)
	if err != nil {
		return nil, &Problem{
			Title:  http.StatusText(http.StatusUnprocessableEntity),
			Status: http.StatusUnprocessableEntity,
			Detail: "the path does not name a valid " + b.opts.name(),
			Errors: []*ProblemDetail{{
				Message:  err.Error(),
				Location: "path.id",
				Value:    raw,
			}},
		}
	}
	return v, nil
}

// selection is the default projection, as Selectable items.
func (b *binding[T]) selection() []sqlb.Selectable {
	items := make([]sqlb.Selectable, len(b.selectable))
	for i, col := range b.selectable {
		items[i] = sqlb.F(col.Name)
	}
	return items
}

func registerRead[T any](api huma.API, db sqlb.Executor, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	op := huma.Operation{
		OperationID: "get-" + opts.name(),
		Method:      http.MethodGet,
		Path:        opts.itemPath(),
		Summary:     "Fetch one " + opts.name(),
		Description: opts.Description,
		Tags:        []string{opts.tag()},
		Security:    opts.Security,
		// Anything the operation does not declare is a mistake. Dropping it
		// silently would answer a question the client did not ask — the same
		// reason the list endpoint refuses an unknown parameter rather than
		// ignoring it.
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusBadRequest, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}

	// read is the handler proper, shared by both registrations below so the
	// expandable and plain forms cannot answer differently.
	read := func(ctx context.Context, id string, expand []string) (*itemOutput[T], error) {
		key, err := b.key(id)
		if err != nil {
			return nil, err
		}
		found, err := sqlb.Query[T]().
			Select(b.selection()...).
			Expand(expand...).
			Where(sqlb.F(b.model.PK.Name).Eq(key)).
			One(ctx, db)
		if err != nil {
			return nil, asHumaError(ctx, err, opts.name())
		}
		return &itemOutput[T]{Body: row[T]{
			value: found, cols: b.selectable, keys: b.jsonKey,
			expand: b.relationsFor(expand),
		}}, nil
	}

	// Two registrations, differing only in whether the input declares `expand`.
	// See expandableInput for why that cannot be one type with a conditional
	// parameter.
	if p := expandParam(b); p != nil {
		op.Parameters = []*huma.Param{p}
		huma.Register(api, op, func(ctx context.Context, in *expandableInput) (*itemOutput[T], error) {
			expand, err := b.expansions(ctx, in.Expand)
			if err != nil {
				return nil, err
			}
			return read(ctx, in.ID, expand)
		})
		return
	}
	huma.Register(api, op, func(ctx context.Context, in *itemInput) (*itemOutput[T], error) {
		return read(ctx, in.ID, nil)
	})
}

// expansions validates an item request's `?expand`.
//
// It goes through filter.Parse rather than checking the names here, so that the
// item endpoint refuses an unexpandable relation with exactly the document the
// list endpoint produces — same status, same message, same `allowed` list.
// ADR-0011 makes the rejection part of the contract, and a second hand-written
// copy of it is a second thing to drift.
func (b *binding[T]) expansions(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	q, err := filter.Parse(
		url.Values{"expand": {strings.Join(names, ",")}},
		filter.Options{
			Model:      b.model,
			Expandable: b.opts.Expandable,
			Computed:   b.opts.Computed,
			Columns:    b.opts.Columns,
		},
	)
	if err != nil {
		return nil, asHumaError(ctx, err, b.opts.name())
	}
	return q.Expand, nil
}

func registerCreate[T any, C CreateBody[T]](api huma.API, w writer, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	huma.Register(api, huma.Operation{
		OperationID:   "create-" + opts.name(),
		Method:        http.MethodPost,
		Path:          opts.Path,
		Summary:       "Create a " + opts.name(),
		Description:   createDescription(b),
		Tags:          []string{opts.tag()},
		Security:      opts.Security,
		DefaultStatus: statusCreated,
		// Create takes no query parameter at all, so every one of them is a
		// mistake — a typo'd `?exapnd=` here used to be answered with a 201 while
		// the same typo on the GET was answered with a 400.
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusBadRequest,
			http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *createInput[C]) (*createdOutput[T], error) {
		value, err := in.Body.Row()
		if err != nil {
			return nil, unprocessable(err, "body")
		}
		if value == nil {
			return nil, unprocessable(fmt.Errorf("the request body produced no %s", opts.name()), "body")
		}

		// Read-only columns are cleared rather than rejected: the database or a
		// BeforeCreate hook owns them, and the body type has no field for them
		// anyway.
		//
		// Cleared, specifically, and not omitted from the statement. Omitting
		// them was wrong in a way that only showed up with a hook: hooks run
		// inside Exec, after the omit set has been recorded, so a tenant id a
		// BeforeCreate hook had just filled in was dropped and the row arrived
		// with a NULL. Clearing keeps the same guarantee against the request —
		// whatever Row() built is discarded — while leaving the hook able to
		// supply the value.
		//
		// The rest of Insert's behaviour is intact: a defaulted column still
		// holding its zero value is omitted by Insert itself, so id, created_at
		// and anything else the database owns still comes from the database.
		b.clearReadOnly(value)

		// A body carrying properties that are not columns hands them over here,
		// because the hook that needs them is handed the row and the context and
		// nothing else. It goes in before the write rather than beside Row(),
		// which builds the row and has no business knowing about the ones that
		// are not on it (#309).
		if declared, ok := any(in.Body).(CreateInput); ok {
			ctx = sqlb.WithCreateInput(ctx, declared.Input())
		}
		created, err := write(ctx, w, func(ctx context.Context, db sqlb.Executor) (T, error) {
			// The mount's computed list covers both paths: the columns this
			// resource decided to pay for are the ones its RETURNING evaluates,
			// and a resource that named none sends an INSERT over stored columns
			// (#164). b.writeComputed is that list minus the ones a write cannot
			// bind, which is why this cannot fail on a Needs column.
			ins := sqlb.InsertRows(value).WithComputed(b.writeComputed...)
			// The body knows which columns the request carried; Insert cannot
			// tell a sent zero from an absent field. Handing the set over is
			// what makes {"active": false} on a Default(true) column write
			// false rather than answering 201 with true (#314).
			//
			// Explicit rather than Only: Only would restrict the statement to
			// what the request named, dropping the columns a BeforeCreate hook
			// fills in — the same failure clearReadOnly is written above to
			// avoid.
			if declared, ok := any(in.Body).(CreateExplicit); ok {
				if set := b.writableOnly(declared.Explicit()); len(set) > 0 {
					ins = ins.Explicit(set...)
				}
			}
			return ins.One(ctx, db)
		})
		if err != nil {
			return nil, asHumaError(ctx, err, opts.name())
		}
		return &createdOutput[T]{Body: row[T]{value: created, cols: b.writeSelectable, keys: b.jsonKey}}, nil
	})
}

func registerUpdate[T any, U UpdateBody](api huma.API, w writer, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	huma.Register(api, huma.Operation{
		OperationID: "update-" + opts.name(),
		Method:      http.MethodPatch,
		Path:        opts.itemPath(),
		Summary:     "Update a " + opts.name(),
		Description: "Only the fields the request carries are written. " + opts.Description,
		Tags:        []string{opts.tag()},
		Security:    opts.Security,
		// As on create: the operation declares no query parameter, so anything
		// in the query string is a mistake and is named rather than dropped.
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusBadRequest, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *updateInput[U]) (*itemOutput[T], error) {
		key, err := b.key(in.ID)
		if err != nil {
			return nil, err
		}
		names, changes, err := b.changeSet(in.Body)
		if err != nil {
			return nil, err
		}

		updated, err := write(ctx, w, func(ctx context.Context, db sqlb.Executor) (T, error) {
			stmt := sqlb.UpdateRows[T]().
				WithComputed(b.writeComputed...).
				Where(sqlb.F(b.model.PK.Name).Eq(key))
			for _, name := range names {
				stmt.Set(name, changes[name])
			}
			return stmt.One(ctx, db)
		})
		if err != nil {
			return nil, asHumaError(ctx, err, opts.name())
		}
		return &itemOutput[T]{Body: row[T]{value: updated, cols: b.writeSelectable, keys: b.jsonKey}}, nil
	})
}

func registerDelete[T any](api huma.API, w writer, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	huma.Register(api, huma.Operation{
		OperationID:                  "delete-" + opts.name(),
		Method:                       http.MethodDelete,
		Path:                         opts.itemPath(),
		Summary:                      "Delete a " + opts.name(),
		Tags:                         []string{opts.tag()},
		Security:                     opts.Security,
		DefaultStatus:                statusNoBody,
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *itemInput) (*struct{}, error) {
		key, err := b.key(in.ID)
		if err != nil {
			return nil, err
		}
		_, err = write(ctx, w, func(ctx context.Context, db sqlb.Executor) (int64, error) {
			n, err := sqlb.DeleteRows[T]().Where(sqlb.F(b.model.PK.Name).Eq(key)).Exec(ctx, db)
			if err != nil {
				return 0, err
			}
			// Reported from inside the unit of work, so that a delete matching
			// nothing rolls back rather than committing an empty transaction
			// whose AfterCommit callbacks would then announce a deletion that
			// did not happen.
			if n == 0 {
				return 0, errNoRowsAffected
			}
			return n, nil
		})
		switch {
		case errors.Is(err, errNoRowsAffected):
			return nil, newError(http.StatusNotFound, "no "+opts.name()+" matched")
		case err != nil:
			return nil, asHumaError(ctx, err, opts.name())
		}
		return nil, nil
	})
}

// changeSet resolves a PATCH body into the columns to write, sorted, and the
// values to write them with.
//
// Shared by the collection's PATCH and the singleton's, which differ only in
// how they address the row. Keeping it in one place is what stops the two
// answering differently to the same bad body — the same reason registerRead's
// two registrations share one closure.
//
// The names are sorted so that the same request compiles to the same SQL and a
// test can assert on the statement.
func (b *binding[T]) changeSet(body UpdateBody) ([]string, map[string]any, error) {
	changes, err := body.Changes()
	if err != nil {
		return nil, nil, unprocessable(err, "body")
	}
	if len(changes) == 0 {
		return nil, nil, &Problem{
			Title:  http.StatusText(http.StatusBadRequest),
			Status: http.StatusBadRequest,
			Detail: "the request body named no writable column",
			Errors: []*ProblemDetail{{
				Message:  "at least one field must be given",
				Location: "body",
				Allowed:  b.updatableNames(),
			}},
		}
	}
	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}
	sort.Strings(names)

	if problem := b.rejectUnwritable(names); problem != nil {
		return nil, nil, problem
	}
	return names, changes, nil
}

// rejectUnwritable reports the columns a PATCH may not set.
//
// A hidden column is reported as unknown rather than as unwritable, and never
// appears in the allow-list, so that the rejection cannot be used to enumerate
// what the resource is concealing. A column outside Options.Columns is treated
// the same way and for the same reason: from this resource it does not exist,
// and "column is read-only" would confirm that it does (#148).
func (b *binding[T]) rejectUnwritable(names []string) *Problem {
	var details []*ProblemDetail
	for _, name := range names {
		col := b.model.Column(name)
		switch {
		case col == nil || col.Hidden || !b.served[name]:
			details = append(details, &ProblemDetail{
				Message:  "unknown column",
				Location: "body." + name,
				Allowed:  b.updatableNames(),
			})
		case col.PrimaryKey || col.ReadOnly:
			details = append(details, &ProblemDetail{
				Message:  "column is read-only",
				Location: "body." + name,
				Allowed:  b.updatableNames(),
			})
		case col.Immutable:
			details = append(details, &ProblemDetail{
				Message:  "column cannot be changed after the row is created",
				Location: "body." + name,
				Allowed:  b.updatableNames(),
			})
		}
	}
	if details == nil {
		return nil
	}
	return &Problem{
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: "one or more fields cannot be written",
		Errors: details,
	}
}

// updatableNames lists the columns a PATCH may set.
func (b *binding[T]) updatableNames() []string {
	var out []string
	for _, col := range b.writable {
		if !col.Immutable {
			out = append(out, col.Name)
		}
	}
	return out
}

func unprocessable(err error, location string) *Problem {
	return &Problem{
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: "the request body was rejected",
		Errors: []*ProblemDetail{{Message: err.Error(), Location: location}},
	}
}

func createDescription[T any](b *binding[T]) string {
	desc := b.opts.Description
	if desc != "" {
		desc += "\n\n"
	}
	return desc + "Read-only columns are supplied by the database or by a hook and are " +
		"not accepted in the body. The stored row is returned, so generated identifiers " +
		"and defaults come back in the response."
}
