package notes

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// provideOperations contributes the one endpoint the generator does not write.
//
// The generated resources arrive from the store module — generation is per
// schema, not per table, so they are one contribution rather than one per
// module. This is the other half: a module that owns a feature adds its
// hand-written operations to the same group, and they land on the same API and
// in the same OpenAPI document.
func provideOperations(db *sqlb.DB) fxkit.OperationSet {
	return fxkit.OperationSet{
		Module:   "notes",
		Register: func(api huma.API) error { return registerInsights(api, db) },
	}
}

// StatusCount is one row of the breakdown.
type StatusCount struct {
	Status store.NoteStatus `db:"status" json:"status"`
	Count  int64            `db:"count" json:"count"`
}

type insightsOutput struct {
	Body struct {
		ByStatus []StatusCount `json:"by_status" doc:"One entry per status that has at least one note."`
	}
}

// registerInsights mounts GET /insights/notes.
//
// The path is under /insights rather than /notes/stats deliberately: /notes/{id}
// is a generated route, and an endpoint whose meaning depends on "stats" never
// being a valid id is a trap for whoever changes the key type later.
//
// What this endpoint is here to show is one line long — the query below has no
// space_id in it, and the SQL that reaches Postgres does. The BeforeQuery hook
// registered in hooks.go applies to an aggregate the same way it applies to
// GET /notes, because both go through sqlb.Query. A GROUP BY that leaked
// across tenants would be a very quiet leak: no rows are returned, only counts
// of them.
func registerInsights(api huma.API, db *sqlb.DB) error {
	huma.Register(api, huma.Operation{
		OperationID: "notes-by-status",
		Method:      http.MethodGet,
		Path:        "/insights/notes",
		Tags:        []string{"insights"},
		Summary:     "Count this space's notes by status",
		Description: "A grouped count. Scoped by the same hook that scopes GET /notes, " +
			"which is the point: aggregates are queries too.",
	}, func(ctx context.Context, _ *struct{}) (*insightsOutput, error) {
		rows, err := sqlb.Collect[StatusCount](ctx, db,
			sqlb.Query[store.Note]().
				Select(store.NoteCols.Status, sqlb.Count()).
				GroupBy(store.NoteCols.Status.Field()).
				OrderBy(store.NoteCols.Status.Asc()))
		if err != nil {
			return nil, asHTTP(err)
		}

		out := &insightsOutput{}
		out.Body.ByStatus = rows
		return out, nil
	})
	return nil
}

// asHTTP passes a refusal a hook already classified through unchanged, and
// turns anything else into a 500 whose body says nothing about the database.
//
// The first half is what keeps the fail-closed path honest: dir.Current
// returns a 401 when the context carries no space, and replacing that with a
// generic 500 would make every deliberate refusal look like an outage.
func asHTTP(err error) error {
	var status huma.StatusError
	if errors.As(err, &status) {
		return err
	}
	return huma.Error500InternalServerError("the request could not be completed",
		fmt.Errorf("notes: counting by status: %w", err))
}
