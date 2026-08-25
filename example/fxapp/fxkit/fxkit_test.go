package fxkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb"
)

// TestGraphIsValid checks the kit's own wiring without building any of it —
// including that a contribution made through each Provide* helper lands in a
// group the kit's constructors actually consume. fx.ValidateApp resolves
// every dependency and constructs nothing, so this needs no database.
func TestGraphIsValid(t *testing.T) {
	app := fx.Options(
		Module(),
		fx.Supply(DBConfig{DSN: "postgres://localhost/ignored"}),
		fx.Supply(HTTPConfig{Title: "t", Version: "0"}),
		ProvideHooks(func() HookSet { return HookSet{Module: "t"} }),
		ProvideMigrations(func() MigrationSet { return MigrationSet{Module: "t"} }),
		ProvideMiddleware(func() MiddlewareSet { return MiddlewareSet{Module: "t"} }),
		ProvideOperations(func() OperationSet { return OperationSet{Module: "t"} }),
	)
	if err := fx.ValidateApp(app); err != nil {
		t.Fatalf("the kit's module graph does not resolve: %v", err)
	}
}

// TestHandlesComposeOverAForeignPlatform is the studio-apps/core shape: the
// platform owns the pool and the migrations, the application supplies the
// Migrated fact, and the kit contributes only the sqlb layer.
func TestHandlesComposeOverAForeignPlatform(t *testing.T) {
	app := fx.Options(
		// What a platform dbbase provides. ValidateApp constructs nothing,
		// so a nil pool constructor stands in for the platform's.
		fx.Provide(func() *pgxpool.Pool { return nil }),
		fx.Supply(Migrated{}),
		Handles(),
		fx.Invoke(func(*sqlb.DB, Unscoped) {}),
	)
	if err := fx.ValidateApp(app); err != nil {
		t.Fatalf("Handles() does not compose over a platform-owned pool: %v", err)
	}
}

// TestGroupTagsMatchTheConstants pins the string literals in the kit's param
// structs to the exported constants. A struct tag cannot reference a constant,
// so this is the check that keeps a rename honest: change GroupHooks and this
// fails until the tag moves with it.
func TestGroupTagsMatchTheConstants(t *testing.T) {
	cases := []struct {
		params reflect.Type
		field  string
		group  string
	}{
		{reflect.TypeOf(scopedParams{}), "Sets", GroupHooks},
		{reflect.TypeOf(migrateParams{}), "Sets", GroupMigrations},
		{reflect.TypeOf(routerParams{}), "Sets", GroupMiddleware},
		{reflect.TypeOf(apiParams{}), "Sets", GroupOperations},
	}
	for _, c := range cases {
		f, ok := c.params.FieldByName(c.field)
		if !ok {
			t.Fatalf("%s has no field %s", c.params.Name(), c.field)
		}
		if got := f.Tag.Get("group"); got != c.group {
			t.Errorf("%s.%s consumes group %q; the constant says %q",
				c.params.Name(), c.field, got, c.group)
		}
	}
}

// The principal seam's own tests are the engine's now, in principal_test.go —
// it moved there because example/tasks wanted it without a container.

// TestScopedHandleReportsTheFailingModule builds the registry from hook sets
// the way the kit does, and asserts both refusals: a set with no Register
// function, and a set whose Register fails, each named in the error.
//
// The pool is parsed but never connected — nothing here queries.
func TestScopedHandleReportsTheFailingModule(t *testing.T) {
	poolCfg, err := pgxpool.ParseConfig("postgres://localhost:1/never")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	unscoped := newUnscoped(pool, Migrated{})

	_, err = newScoped(scopedParams{
		Unscoped: unscoped,
		Sets:     []HookSet{{Module: "billing"}},
	})
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("a nil Register was not refused by module name: %v", err)
	}

	_, err = newScoped(scopedParams{
		Unscoped: unscoped,
		Sets: []HookSet{{Module: "billing", Register: func(*sqlb.Registry) error {
			return context.Canceled
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("a failing Register was not reported by module name: %v", err)
	}

	// And the passing shape, so the refusals above cannot pass by refusing
	// everything.
	db, err := newScoped(scopedParams{
		Unscoped: unscoped,
		Sets:     []HookSet{{Module: "billing", Register: func(*sqlb.Registry) error { return nil }}},
	})
	if err != nil || db == nil {
		t.Fatalf("a valid hook set did not produce a handle: %v", err)
	}
}

// TestMiddlewareOrderIsExplicit serves one request through a router built
// from contributions arriving in the wrong order, and asserts the chain ran
// by Order, ties broken by Module — not by arrival.
func TestMiddlewareOrderIsExplicit(t *testing.T) {
	var ran []string
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = append(ran, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	router := newRouter(routerParams{Sets: []MiddlewareSet{
		{Module: "z-late", Order: 10, Wrap: mark("z-late")},
		{Module: "b-tie", Order: 1, Wrap: mark("b-tie")},
		{Module: "a-tie", Order: 1, Wrap: mark("a-tie")},
	}})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rec.Code)
	}

	want := []string{"a-tie", "b-tie", "z-late"}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("middleware ran %v; want %v", ran, want)
	}
}

// TestOperationFailureTakesTheBoot asserts the property the kit exists to
// preserve: an OperationSet whose Register fails becomes a constructor error
// naming the module, not a server that starts and serves the rest.
func TestOperationFailureTakesTheBoot(t *testing.T) {
	_, err := newAPI(apiParams{
		Cfg:    HTTPConfig{Title: "t", Version: "0"},
		Router: newRouter(routerParams{}),
		Sets: []OperationSet{{Module: "store", Register: func(huma.API) error {
			return context.Canceled
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("a failing Register was not reported by module name: %v", err)
	}
}
