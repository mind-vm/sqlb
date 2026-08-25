package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// OverduePosts is what codegen would emit for a query's declared parameters.
type OverduePosts struct {
	AsOf string `query:"as_of" doc:"RFC3339 timestamp"`
}

func overdueSpec() rest.QuerySpec {
	return rest.QuerySpec{
		Name:      "overdue",
		Path:      "/posts/overdue",
		Field:     "OverduePosts",
		Summary:   "Posts overdue as of a time",
		Reads:     []string{"posts"},
		HasParams: true,
	}
}

func mountQuery(t *testing.T, db sqlb.Executor, spec rest.QuerySpec,
	do func(context.Context, sqlb.Executor, OverduePosts) ([]Post, error)) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Query[OverduePosts, []Post](api, db, postOptions(), spec, do); err != nil {
		t.Fatalf("mounting the query: %v", err)
	}
	return api
}

// The envelope binds the declared parameters, hands the func the Executor it
// was mounted with, and renders whatever the func returns — no fetch, no
// transaction, no lock, unlike an action.
func TestQueryBindsParamsAndRendersWhateverDoReturns(t *testing.T) {
	db := newFakeDB(t)

	var seenDB sqlb.Executor
	var seenParams OverduePosts
	api := mountQuery(t, db.db, overdueSpec(), func(_ context.Context, exec sqlb.Executor, p OverduePosts) ([]Post, error) {
		seenDB = exec
		seenParams = p
		return []Post{{ID: "p1", Title: "Hello"}}, nil
	})

	resp := api.Get("/posts/overdue?as_of=2026-01-01T00:00:00Z")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if seenDB != db.db {
		t.Fatalf("the func was handed a different Executor than Query was mounted with")
	}
	if seenParams.AsOf != "2026-01-01T00:00:00Z" {
		t.Errorf("the func was handed params %+v, want the decoded query string", seenParams)
	}

	var got []Post
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("response = %+v, want the rows the func returned", got)
	}

	// Nothing was ever queried: the envelope issues no fetch of its own.
	if len(db.statements()) != 0 {
		t.Errorf("the envelope issued statements %v, want none", db.statements())
	}
}

// A query with no declared parameters reads none, and the handler still
// receives the zero value.
func TestQueryWithNoParamsRunsWithTheZeroValue(t *testing.T) {
	db := newFakeDB(t)
	spec := overdueSpec()
	spec.HasParams = false

	var seenParams OverduePosts
	api := mountQuery(t, db.db, spec, func(_ context.Context, _ sqlb.Executor, p OverduePosts) ([]Post, error) {
		seenParams = p
		return nil, nil
	})

	resp := api.Get("/posts/overdue")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if seenParams != (OverduePosts{}) {
		t.Errorf("params = %+v, want the zero value", seenParams)
	}
}

// A nil func is refused at mount, the same as an action's nil Do — the
// compiler gets the first word, and this is the last one before a request.
func TestQueryRefusesAMissingDo(t *testing.T) {
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Query[OverduePosts, []Post](api, newFakeDB(t).db, postOptions(), overdueSpec(), nil)
	if err == nil {
		t.Fatal("want an error mounting a query with a nil func")
	}
}
