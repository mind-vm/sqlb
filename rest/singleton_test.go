package rest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// A tenant-keyed singleton is exercised through the Scoped fixture in
// scope_test.go: org_id carries the scope marker, which is the declaration the
// whole shape rests on.
//
// The hooks below are the ones ADR-0030 already requires. What they do here is
// what a real one does — append the caller's tenant — because the point of the
// shape is that the confinement is the *only* thing addressing the row (#166).

func singletonOptions() rest.Options {
	return rest.Options{Path: "/subscription", Name: "subscription", Ops: rest.OpSingleton}
}

// scopedTo builds the registry a singleton mount requires, with each hook
// narrowing to org.
func scopedTo(org string) *sqlb.Registry {
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).
		BeforeQuery(func(_ context.Context, b *sqlb.Builder[Scoped]) error {
			b.Where(sqlb.F("org_id").Eq(org))
			return nil
		}).
		BeforeCreate(func(_ context.Context, s *Scoped) error { s.OrgID = org; return nil }).
		BeforeUpdate(func(_ context.Context, u *sqlb.Update[Scoped]) error {
			u.Where(sqlb.F("org_id").Eq(org))
			return nil
		}).
		BeforeDelete(func(_ context.Context, d *sqlb.Delete[Scoped]) error {
			d.Where(sqlb.F("org_id").Eq(org))
			return nil
		})
	return reg
}

func mountSingleton(t *testing.T, f *fakeDB, opts rest.Options) (humatest.TestAPI, error) {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	db := sqlb.New(f.db).WithHooks(scopedTo("org-1"))
	return api, rest.Resource[Scoped, scopedCreate, scopedUpdate](api, db, opts)
}

func mustMountSingleton(t *testing.T, f *fakeDB, opts rest.Options) humatest.TestAPI {
	t.Helper()
	api, err := mountSingleton(t, f, opts)
	if err != nil {
		t.Fatalf("mounting the singleton: %v", err)
	}
	return api
}

func scopedCols() []string { return []string{"id", "org_id", "title", "deleted_at"} }

func scopedRow(id, org, title string) []any { return []any{id, org, title, nil} }

// The shape itself: GET on the collection path, a bare object, and no {id}
// anywhere — which is the whole ask. Before this the two available answers were
// a one-element envelope every client unwraps forever, and a route asking the
// client to send back its own tenant id.
func TestSingletonAnswersTheCallersRowAsABareObject(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols(), rows: [][]any{scopedRow("s1", "org-1", "Pro")}})
	api := mustMountSingleton(t, db, singletonOptions())

	resp := api.Get("/subscription")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	body := decode(t, resp.Body.Bytes())
	if body["title"] != "Pro" {
		t.Errorf("body = %s, want the caller's row itself rather than an envelope", resp.Body)
	}
	if _, wrapped := body["items"]; wrapped {
		t.Errorf("body = %s, want a bare object", resp.Body)
	}
}

// The statement carries no key predicate of its own. Everything narrowing it is
// the hook's, which is why Resource refuses the shape without a Scoped column —
// see TestSingletonRefusesAModelThatIsNotScoped.
func TestSingletonAddressesItsRowThroughTheScopeHookAlone(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols(), rows: [][]any{scopedRow("s1", "org-1", "Pro")}})
	api := mustMountSingleton(t, db, singletonOptions())

	if resp := api.Get("/subscription"); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	if !strings.Contains(stmt, `WHERE "org_id" = $1`) {
		t.Errorf("the scope hook did not confine the read:\n%s", stmt)
	}
	if strings.Contains(stmt, `"id" =`) {
		t.Errorf("the singleton addressed a row by key:\n%s", stmt)
	}
	if args := db.lastArgs(); len(args) != 1 || args[0] != "org-1" {
		t.Errorf("args = %v, want the caller's tenant and nothing else", db.lastArgs())
	}
}

// A caller with no row gets a 404, not a 200 over nothing. This is the one
// status the shape has to get right: "you have no subscription yet" is a real
// answer a client branches on.
func TestSingletonIs404WhenTheCallerHasNoRow(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols()})
	api := mustMountSingleton(t, db, singletonOptions())

	if resp := api.Get("/subscription"); resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
}

// Two rows is a scoping bug, and answering the first of them would be the
// silent wrong answer this package refuses everywhere else. One reports it.
func TestSingletonRefusesToPickBetweenTwoRows(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols(), rows: [][]any{
		scopedRow("s1", "org-1", "Pro"),
		scopedRow("s2", "org-1", "Team"),
	}})
	api := mustMountSingleton(t, db, singletonOptions())

	if resp := api.Get("/subscription"); resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: a singleton that matched two rows is not a 200: %s",
			resp.Code, resp.Body)
	}
}

// PATCH lands on the collection path too, and its statement is the same shape:
// a SET and the hook's predicate.
func TestSingletonUpdateWritesTheCallersRowWithNoKey(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols(), rows: [][]any{scopedRow("s1", "org-1", "Team")}})
	opts := singletonOptions()
	opts.Ops = rest.OpSingleton | rest.OpUpdate
	api := mustMountSingleton(t, db, opts)

	resp := api.Patch("/subscription", map[string]any{"title": "Team"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	if !strings.Contains(stmt, `UPDATE "scoped" SET "title" = $1 WHERE "org_id" = $2`) {
		t.Errorf("unexpected statement:\n%s", stmt)
	}
}

// A PATCH from a caller with no row is a 404 rather than a 200 having written
// nothing — the write-side twin of the read above, and the failure #159 is
// about arriving through a generated handler.
func TestSingletonUpdateIs404WhenTheCallerHasNoRow(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols()})
	opts := singletonOptions()
	opts.Ops = rest.OpSingleton | rest.OpUpdate
	api := mustMountSingleton(t, db, opts)

	resp := api.Patch("/subscription", map[string]any{"title": "Team"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
}

// DELETE is the same story, and matching nothing must not commit: the delete is
// reported from inside the unit of work so an AfterCommit callback cannot
// announce a deletion that did not happen.
func TestSingletonDeleteIs404WhenTheCallerHasNoRow(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols()})
	opts := singletonOptions()
	opts.Ops = rest.OpSingleton | rest.OpDelete
	api := mustMountSingleton(t, db, opts)

	resp := api.Delete("/subscription")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
	if stmts := db.statements(); stmts[len(stmts)-1] != "ROLLBACK" {
		t.Errorf("a delete that matched nothing committed: %v", stmts)
	}
}

// The singleton takes no filter, no sort, no page and no ?select: there is one
// row and the caller does not choose it. Everything but ?expand is named rather
// than dropped, as it is on every other operation here.
func TestSingletonRejectsCollectionParameters(t *testing.T) {
	db := newFakeDB(t, reply{cols: scopedCols(), rows: [][]any{scopedRow("s1", "org-1", "Pro")}})
	api := mustMountSingleton(t, db, singletonOptions())

	for _, q := range []string{"?title=eq.Pro", "?sort=title", "?page=2", "?select=title"} {
		if resp := api.Get("/subscription" + q); resp.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET /subscription%s = %d, want 422: %s", q, resp.Code, resp.Body)
		}
	}
}

// The refusal that carries the safety argument. Without a Scoped column the
// same statements are unconfined: the read answers an arbitrary row and the
// PATCH reaches every row in the table. Post has no scope marker, so this is
// the mount that must not happen.
func TestSingletonRefusesAModelThatIsNotScoped(t *testing.T) {
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Post, PostCreate, PostUpdate](api, newFakeDB(t).db, rest.Options{
		Path: "/settings", Name: "settings", Ops: rest.OpSingleton,
	})
	if err == nil {
		t.Fatal("expected the mount to fail: a singleton over an unconfined table serves an arbitrary row")
	}
	for _, want := range []string{"no Scoped column", "reaches every row"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}
}

// A singleton still owes its hooks, and the read is the strongest case there
// is: the hook is not narrowing the answer, it *is* the answer.
func TestSingletonRefusesAScopedModelWithNoHooks(t *testing.T) {
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	err := rest.Resource[Scoped, scopedCreate, scopedUpdate](api, db, singletonOptions())
	if err == nil {
		t.Fatal("expected the mount to fail: nothing confines a singleton whose schema says it is confined")
	}
	for _, want := range []string{"singleton read", "BeforeQuery", "org_id is Scoped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}
}

// The two combinations that cannot mean anything, each named for its own
// reason: OpList is the same route, and OpRead is the id-shaped question the
// shape exists to delete.
func TestSingletonRefusesTheCollectionOps(t *testing.T) {
	tests := []struct {
		name string
		ops  rest.Op
		want string
	}{
		{"list", rest.OpSingleton | rest.OpList, "the same route"},
		{"read", rest.OpSingleton | rest.OpRead, "drop OpRead"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := singletonOptions()
			opts.Ops = tc.ops
			_, err := mountSingleton(t, newFakeDB(t), opts)
			if err == nil {
				t.Fatalf("expected the mount to fail for %s", tc.ops)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

// A singleton needs no primary key: nothing addresses its row by one. This is
// what lets a table keyed only by its tenant column be a resource at all, which
// is the case the report was about.
func TestSingletonNeedsNoPrimaryKey(t *testing.T) {
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	reg := sqlb.NewRegistry()
	sqlb.On[TenantSettings](reg).BeforeQuery(func(context.Context, *sqlb.Builder[TenantSettings]) error { return nil })
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	err := rest.Resource[TenantSettings, rest.None[TenantSettings], rest.None[TenantSettings]](api, db, rest.Options{
		Path: "/settings", Name: "settings", Ops: rest.OpSingleton,
	})
	if err != nil {
		t.Fatalf("mounting a keyless singleton: %v", err)
	}
}

// TenantSettings is a settings table whose only key is the tenant it belongs
// to — the shape that had no resource before OpSingleton.
type TenantSettings struct {
	OrgID string `db:"org_id" json:"org_id" sqlb:"readonly,scope"`
	Theme string `db:"theme" json:"theme"`
}

func (TenantSettings) TableName() string { return "tenant_settings" }
