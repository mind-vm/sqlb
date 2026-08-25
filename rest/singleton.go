package rest

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
)

// The singleton shape.
//
// A table keyed by its own scope column has one row per caller, and neither op
// on offer fitted it: OpList answered a one-element envelope that every client
// unwraps forever, and OpRead put the caller's tenant id in the URL — a value
// the server already holds and the hook already enforces, so the segment is
// either redundant or a lie and a mismatch is a 404 meaning "you typed your own
// name wrong". Settings-per-tenant, profile-per-user and subscription-per-org
// are all this shape, and each one was a hand-written handler beside an
// otherwise fully declared module (#166).
//
// What makes the shape safe is the same thing that makes it possible: the row
// comes from the scope hook. There is no key in the path and no key predicate
// in the statement, so `SELECT … FROM billing_subscriptions` is what the handler
// builds and `WHERE org_id = $1` is what the hook appends. That is why
// [Resource] refuses OpSingleton on a model with no Scoped column — without the
// hook the read answers an arbitrary row and the write reaches every row — and
// why the obligation check treats this read as the strongest case it has.
//
// The statements below therefore look under-constrained on purpose. Reading one
// and concluding a predicate is missing is the expected reaction; the predicate
// is the hook's, and ADR-0030 is what guarantees there is one.

// singletonInput is a request that carries nothing: no key, no query.
type singletonInput struct{}

// singletonExpandInput is singletonInput for a resource that declares a
// relation. It is a second type for the reason expandableInput is — huma builds
// its known-parameter set from the input struct, so a field here is what makes
// ?expand a parameter the operation has.
type singletonExpandInput struct {
	Expand []string `query:"expand" doc:"Relations to embed."`
}

type singletonUpdateInput[U any] struct {
	Body U
}

// singletonDescription prefixes the resource's own description with the one
// fact a client cannot see from the path: there is no id because there is one
// row, and which row is not the client's to choose.
func singletonDescription(desc string) string {
	out := "The caller's own row. There is no {id}: the resource holds one row per caller " +
		"and which one is settled by the server, so a client that has authenticated has already " +
		"said everything the route needs. Answers 404 when the caller has no row yet."
	if desc != "" {
		out = desc + "\n\n" + out
	}
	return out
}

func registerSingleton[T any](api huma.API, db sqlb.Executor, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	op := huma.Operation{
		OperationID: "get-" + opts.name(),
		Method:      http.MethodGet,
		Path:        opts.Path,
		Summary:     "Fetch the caller's " + opts.name(),
		Description: singletonDescription(opts.Description),
		Tags:        []string{opts.tag()},
		Security:    opts.Security,
		// A singleton takes no filter, no sort, no page and no ?select: there is
		// one row and the caller does not choose it. Everything but ?expand is
		// therefore a mistake, and is named rather than dropped.
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError),
	}

	// read is shared by both registrations below, so the expandable and plain
	// forms cannot answer differently.
	read := func(ctx context.Context, expand []string) (*itemOutput[T], error) {
		// No Where. The scope hook supplies the whole predicate — see the note at
		// the top of this file — and One is what reports both ways it can be
		// wrong: ErrNotFound becomes the 404 a caller with no row should get, and
		// more than one row is an error rather than an arbitrary pick, because a
		// singleton answering two rows is a scoping bug and serving the first of
		// them is exactly the silent wrong answer this package refuses elsewhere.
		found, err := sqlb.Query[T]().
			Select(b.selection()...).
			Expand(expand...).
			One(ctx, db)
		if err != nil {
			return nil, asHumaError(ctx, err, opts.name())
		}
		return &itemOutput[T]{Body: row[T]{
			value: found, cols: b.selectable, keys: b.jsonKey,
			expand: b.relationsFor(expand),
		}}, nil
	}

	if p := expandParam(b); p != nil {
		op.Parameters = []*huma.Param{p}
		huma.Register(api, op, func(ctx context.Context, in *singletonExpandInput) (*itemOutput[T], error) {
			expand, err := b.expansions(ctx, in.Expand)
			if err != nil {
				return nil, err
			}
			return read(ctx, expand)
		})
		return
	}
	huma.Register(api, op, func(ctx context.Context, in *singletonInput) (*itemOutput[T], error) {
		return read(ctx, nil)
	})
}

func registerSingletonUpdate[T any, U UpdateBody](api huma.API, w writer, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	huma.Register(api, huma.Operation{
		OperationID:                  "update-" + opts.name(),
		Method:                       http.MethodPatch,
		Path:                         opts.Path,
		Summary:                      "Update the caller's " + opts.name(),
		Description:                  "Only the fields the request carries are written. " + singletonDescription(opts.Description),
		Tags:                         []string{opts.tag()},
		Security:                     opts.Security,
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusBadRequest, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *singletonUpdateInput[U]) (*itemOutput[T], error) {
		names, changes, err := b.changeSet(in.Body)
		if err != nil {
			return nil, err
		}
		updated, err := write(ctx, w, func(ctx context.Context, db sqlb.Executor) (T, error) {
			// Again no Where: BeforeUpdate carries the confinement, and the
			// obligation check refuses to mount this without one. One is what
			// turns "the caller has no row" into a 404 rather than a 200 over an
			// empty result — the failure mode #159 is about, arriving here as a
			// PATCH that would otherwise report success having written nothing.
			stmt := sqlb.UpdateRows[T]().WithComputed(b.writeComputed...)
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

func registerSingletonDelete[T any](api huma.API, w writer, b *binding[T]) {
	reg := api.OpenAPI().Components.Schemas
	opts := b.opts

	huma.Register(api, huma.Operation{
		OperationID:                  "delete-" + opts.name(),
		Method:                       http.MethodDelete,
		Path:                         opts.Path,
		Summary:                      "Delete the caller's " + opts.name(),
		Description:                  singletonDescription(opts.Description),
		Tags:                         []string{opts.tag()},
		Security:                     opts.Security,
		DefaultStatus:                statusNoBody,
		RejectUnknownQueryParameters: true,
		Responses: errorResponses(reg,
			http.StatusNotFound, http.StatusInternalServerError),
	}, func(ctx context.Context, in *singletonInput) (*struct{}, error) {
		_, err := write(ctx, w, func(ctx context.Context, db sqlb.Executor) (int64, error) {
			n, err := sqlb.DeleteRows[T]().Exec(ctx, db)
			if err != nil {
				return 0, err
			}
			// Reported from inside the unit of work, exactly as the collection's
			// delete is, so that a delete matching nothing rolls back rather than
			// committing an empty transaction whose AfterCommit callbacks would
			// announce a deletion that did not happen.
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
