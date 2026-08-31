package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// RequiredAsOf is what codegen emits for a parameter that is neither nullable
// nor defaulted: the same field with `required:"true"` on it.
//
// The tag is the whole of the difference, and it was absent for as long as
// declared queries existed. huma treats a query parameter as optional unless
// told otherwise, so a read that could not answer without a value was handed
// the zero one instead and had no way to tell that from a caller who meant
// midnight on the first of January year one. The contract said the parameter
// was required the whole time — restcompat records it as such, since it is
// neither nullable nor defaulted — so the server was more permissive than its
// own declaration rather than the declaration being wrong.
type RequiredAsOf struct {
	AsOf string `query:"as_of" required:"true" doc:"RFC3339 timestamp"`
}

func mountRequired(t *testing.T, db sqlb.Executor,
	do func(context.Context, sqlb.Executor, RequiredAsOf) ([]Post, error)) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	spec := overdueSpec()
	if err := rest.Query[RequiredAsOf, []Post](api, db, postOptions(), spec, do); err != nil {
		t.Fatalf("mounting the query: %v", err)
	}
	return api
}

// Omitting a required parameter is refused, and the func does not run.
//
// The second half is the point. A read that is handed a zero value it cannot
// distinguish from a deliberate one answers 200 with rows nobody asked for,
// which is the failure this asserts against — not a wrong status, a wrong
// answer.
func TestAnOmittedRequiredQueryParamIsRefusedBeforeDoRuns(t *testing.T) {
	db := newFakeDB(t)

	ran := false
	api := mountRequired(t, db.db, func(context.Context, sqlb.Executor, RequiredAsOf) ([]Post, error) {
		ran = true
		return nil, nil
	})

	resp := api.Get("/posts/overdue")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for an omitted required parameter: %s", resp.Code, resp.Body)
	}
	if ran {
		t.Error("the func ran with a zero parameter, which is the outcome the required tag exists to prevent")
	}
	// The refusal names the parameter, so a caller learns which one it left
	// out rather than that "the request" was wrong.
	if !strings.Contains(resp.Body.String(), "as_of") {
		t.Errorf("the refusal does not name the parameter: %s", resp.Body)
	}
}

// And supplying it still works, so the tag narrows nothing it should not.
func TestASuppliedRequiredQueryParamReachesDo(t *testing.T) {
	db := newFakeDB(t)

	var seen RequiredAsOf
	api := mountRequired(t, db.db, func(_ context.Context, _ sqlb.Executor, p RequiredAsOf) ([]Post, error) {
		seen = p
		return []Post{{ID: "p1", Title: "Hello"}}, nil
	})

	resp := api.Get("/posts/overdue?as_of=2026-01-01T00:00:00Z")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if seen.AsOf != "2026-01-01T00:00:00Z" {
		t.Errorf("the func saw as_of = %q", seen.AsOf)
	}
}
