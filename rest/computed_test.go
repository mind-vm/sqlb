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

// Starred is what codegen emits for a table with a per-viewer computed column:
// an ordinary field with an ordinary tag, and the expression in a method.
type Starred struct {
	ID        string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	Title     string `db:"title" json:"title" sqlb:"filter,sort"`
	IsStarred bool   `db:"is_starred" json:"is_starred" sqlb:"filter,readonly"`
	// StarCount is the other kind of computed column: an aggregate that takes no
	// bind, so a write *could* evaluate it — and since #164 does so only when the
	// resource asked for it.
	StarCount int64 `db:"star_count" json:"star_count" sqlb:"readonly"`
}

func (Starred) TableName() string { return "starred" }

func (Starred) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{{
		Name:  "is_starred",
		Expr:  "EXISTS (SELECT 1 FROM stars s WHERE s.item_id = starred.id AND s.member_id = ?)",
		Needs: []string{"viewer"},
	}, {
		Name: "star_count",
		Expr: "(SELECT count(*) FROM stars s WHERE s.item_id = starred.id)",
	}}
}

type starredCreate struct {
	Title string `json:"title"`
}

func (c starredCreate) Row() (*Starred, error) { return &Starred{Title: c.Title}, nil }

type starredUpdate struct {
	Title *string `json:"title,omitempty"`
}

func (u starredUpdate) Changes() (map[string]any, error) {
	out := map[string]any{}
	if u.Title != nil {
		out["title"] = *u.Title
	}
	return out, nil
}

func mountStarred(t *testing.T, db sqlb.Executor) error {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	return rest.Resource[Starred, starredCreate, starredUpdate](api, db, rest.Options{
		Path:     "/starred",
		Name:     "starred",
		Ops:      rest.CRUD | rest.OpList,
		Computed: []string{"is_starred"},
	})
}

// starredAPI mounts the resource with a chosen computed set and a hook that
// supplies the viewer bind, returning the test API to drive it with.
func starredAPI(t *testing.T, db *fakeDB, computed ...string) humatest.TestAPI {
	t.Helper()
	reg := sqlb.NewRegistry()
	sqlb.On[Starred](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Starred]) error {
		q.Bind("viewer", "member-1")
		return nil
	})
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Starred, starredCreate, starredUpdate](api, sqlb.New(db.db).WithHooks(reg), rest.Options{
		Path:     "/starred",
		Name:     "starred",
		Ops:      rest.CRUD | rest.OpList,
		Computed: computed,
	})
	if err != nil {
		t.Fatalf("mounting starred with Computed %v: %v", computed, err)
	}
	return api
}

// A write cannot evaluate a Needs-computed column — it has no bind — so
// ADR-0041 leaves it out of RETURNING. The response used to serialise the
// scanned struct's zero value anyway, which is a definite `false` where the
// truth is unknown, and for the acknowledged-by-me flag this feature exists to
// serve that is exactly the bug declaring it was meant to delete (#163).
func TestWriteResponseOmitsAColumnItCannotCompute(t *testing.T) {
	db := newFakeDB(t, reply{
		cols: []string{"id", "title"},
		rows: [][]any{{"s1", "New"}},
	})
	api := starredAPI(t, db, "is_starred")

	resp := api.Patch("/starred/s1", map[string]any{"title": "New"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if stmt := db.lastStatement(); strings.Contains(stmt, "is_starred") {
		t.Errorf("the statement should not mention a column it cannot bind:\n%s", stmt)
	}
	// Absent, not false. A client reads an absent key as "not computed here",
	// which is what the ADR promised; a present `false` is indistinguishable
	// from a real answer.
	if body := resp.Body.String(); strings.Contains(body, "is_starred") {
		t.Errorf("the write response carries a column no statement computed: %s", body)
	}
}

// The write path takes computed columns the way the read path has since #92:
// only the ones the mount asked for. Proven both ways on the same resource,
// because the failure #164 reports is the one that is invisible from inside a
// single mount — the aggregate is correct, it is simply paid for by writes that
// discard it, and by writes into a database where its table does not exist.
func TestWriteReturningFollowsTheMountsComputedSet(t *testing.T) {
	const expr = `(SELECT count(*) FROM stars s WHERE s.item_id = starred.id)`

	t.Run("not asked for", func(t *testing.T) {
		db := newFakeDB(t, reply{cols: []string{"id", "title"}, rows: [][]any{{"s1", "Hello"}}})
		api := starredAPI(t, db)

		resp := api.Post("/starred", map[string]any{"title": "Hello"})
		if resp.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
		}
		if stmt := db.lastStatement(); strings.Contains(stmt, expr) {
			t.Errorf("a resource that named no computed column still evaluates one:\n%s", stmt)
		}
		if body := resp.Body.String(); strings.Contains(body, "star_count") {
			t.Errorf("the response carries a column the resource does not serve: %s", body)
		}
	})

	t.Run("asked for", func(t *testing.T) {
		db := newFakeDB(t, reply{
			cols: []string{"id", "title", "star_count"},
			rows: [][]any{{"s1", "Hello", int64(4)}},
		})
		api := starredAPI(t, db, "star_count")

		resp := api.Post("/starred", map[string]any{"title": "Hello"})
		if resp.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
		}
		if stmt := db.lastStatement(); !strings.Contains(stmt, expr) {
			t.Errorf("a resource that asked for the column should get it back:\n%s", stmt)
		}
		if body := resp.Body.String(); !strings.Contains(body, `"star_count":4`) {
			t.Errorf("the response should carry the value the write returned: %s", body)
		}
	})
}

// The failure this closes: a declared bind nobody supplies. Every list would
// fail at the database, one request at a time, for a reason nothing in the
// handler names — so the resource refuses to mount instead (ADR-0041, and
// ADR-0030's shape).
func TestResourceRefusesAnUnboundComputedColumn(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	err := mountStarred(t, db)
	if err == nil {
		t.Fatal("expected mounting to fail: nothing supplies the viewer bind")
	}
	for _, want := range []string{
		"BeforeQuery",
		`is_starred is computed from the "viewer" bind`,
		// The headline says what is actually missing rather than reaching for
		// the tenant vocabulary, which would send a reader hunting for a scope
		// predicate that was never declared.
		"nothing supplies the computed binds of",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}
}

// A registered BeforeQuery hook satisfies it, and — as everywhere else in this
// check — its contents are not inspected. The query itself is the second line
// of defence: an expression whose bind never arrives fails rather than
// rendering NULL and answering false forever.
func TestResourceAcceptsAComputedColumnWithAHook(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Starred](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Starred]) error {
		q.Bind("viewer", "member-1")
		return nil
	})
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	if err := mountStarred(t, db); err != nil {
		t.Fatalf("mounting a resource whose bind a hook supplies: %v", err)
	}
}

// The other half of the obligation, and the reason it moved: a resource that
// does not select the column is not asked for its bind.
//
// Before #92 this mount failed, because the model declares is_starred and the
// check read the model. The model is shared — the same rows are served by a
// screen that wants the column and by endpoints that do not — so an obligation
// every mount inherited was an obligation with no failure behind it for most of
// them.
func TestResourceWithoutTheComputedColumnNeedsNoBind(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Starred, starredCreate, starredUpdate](api, db, rest.Options{
		Path: "/starred",
		Name: "starred",
		Ops:  rest.CRUD | rest.OpList,
		// No Computed: the resource never renders is_starred.
	})
	if err != nil {
		t.Fatalf("a resource that does not select the computed column was refused: %v", err)
	}
}

// Naming a column the model does not compute is a mounting error, for the
// reason an unknown Expandable is: at request time it would parse cleanly and
// serve a resource quietly missing the value somebody declared it should carry.
func TestResourceRefusesAnUnknownComputedName(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	tests := []struct {
		name, column, want string
	}{
		{"unknown", "is_starre", "has no such column"},
		{"stored", "title", "stores that column rather than computing it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
			err := rest.Resource[Starred, starredCreate, starredUpdate](api, db, rest.Options{
				Path:     "/starred",
				Name:     "starred",
				Ops:      rest.OpList,
				Computed: []string{tc.column},
			})
			if err == nil {
				t.Fatalf("Computed %q was accepted", tc.column)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say what is wrong (%q missing): %v", tc.want, err)
			}
		})
	}
}
