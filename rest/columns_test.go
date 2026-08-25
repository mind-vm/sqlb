package rest_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// Options.Columns is the answer to two surfaces over one table (#148).
//
// A storefront and an admin panel read the same products and differ in which
// columns each may see, and Hidden cannot say that: Hidden is a property of the
// model and there is one model per table. So the narrowing moved to the mount,
// where Computed had already put it for a different reason (#92).
//
// The Post model stands in for the shape. `public` serves three columns; the
// wide mount below serves everything, which is the half that has to keep
// working — a narrowing that narrowed the model rather than the resource would
// pass every test in this file except the last one.

// publicOptions is the narrow surface: id, title, status, and nothing else.
func publicOptions() rest.Options {
	o := postOptions()
	o.Path = "/public/posts"
	o.Name = "public-post"
	o.Columns = []string{"id", "title", "status"}
	return o
}

// The projection the database sees, the keys the response carries, and the
// parameters the document publishes all follow the resource rather than the
// model.
func TestANarrowedResourceReadsAndServesOnlyItsColumns(t *testing.T) {
	db := newFakeDB(t, reply{
		cols: []string{"id", "title", "status"},
		rows: [][]any{{"p1", "Hello", "draft"}},
	})
	api := mount(t, db.db, publicOptions())

	resp := api.Get("/public/posts")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	// Read, not merely hidden on the way out. A resource that selected the
	// column and dropped it during serialisation would still put the value on
	// the wire between Postgres and this process, and into every query log
	// along the way.
	stmt := db.lastStatement()
	for _, gone := range []string{`"body"`, `"excerpt"`, `"view_count"`, `"org_id"`, `"created_at"`} {
		if strings.Contains(stmt, gone) {
			t.Errorf("the narrowed resource selected %s:\n%s", gone, stmt)
		}
	}
	for _, want := range []string{`"id"`, `"title"`, `"status"`} {
		if !strings.Contains(stmt, want) {
			t.Errorf("the narrowed resource did not select %s:\n%s", want, stmt)
		}
	}

	body := resp.Body.String()
	for _, gone := range []string{"body", "excerpt", "view_count", "org_id", "created_at"} {
		if strings.Contains(body, `"`+gone+`"`) {
			t.Errorf("the response carries %q:\n%s", gone, body)
		}
	}
}

// A request naming an excluded column is refused as *unknown*, and the list of
// what would have been accepted does not name it either.
//
// Both halves matter. ADR-0011 makes the rejection part of the contract, and a
// narrowed resource that answered "column is not filterable" — or listed
// view_count among the allowed names — would confirm the existence of the
// column it was narrowed to conceal. Echoing back the name the client sent is
// not a disclosure: the client typed it.
func TestANarrowedResourceRefusesAnExcludedColumnAsUnknown(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, publicOptions())

	for _, q := range []string{
		"/public/posts?view_count=gte.3",
		"/public/posts?sort=-created_at",
		"/public/posts?select=id,body",
	} {
		resp := api.Get(q)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", q, resp.Code, resp.Body)
		}
		problem := decode(t, resp.Body.Bytes())
		errs, _ := problem["errors"].([]any)
		if len(errs) == 0 {
			t.Fatalf("%s: no errors in %v", q, problem)
		}
		for _, raw := range errs {
			detail, _ := raw.(map[string]any)
			if msg, _ := detail["message"].(string); !strings.Contains(msg, "unknown") {
				t.Errorf("%s: message = %q, want it to read as unknown", q, msg)
			}
			for _, name := range detail["allowed"].([]any) {
				switch name {
				case "view_count", "created_at", "body", "excerpt", "org_id":
					t.Errorf("%s: the allowed list names %q, which the resource does not serve: %v",
						q, name, detail["allowed"])
				}
			}
		}
	}
}

// The operation's parameters come from the same surface the parser enforces. A
// published parameter for a column every request naming it is about to be
// refused for is the failure both #92 and #148 are shaped like.
func TestANarrowedResourceDocumentsOnlyItsParameters(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, publicOptions())

	list := api.OpenAPI().Paths["/public/posts"].Get
	if list == nil {
		t.Fatal("the narrowed resource has no list operation")
	}
	names := map[string]bool{}
	enums := map[string]bool{}
	for _, p := range list.Parameters {
		names[p.Name] = true
		if p.Schema != nil && p.Schema.Items != nil {
			for _, v := range p.Schema.Items.Enum {
				enums[strings.TrimPrefix(fmt.Sprint(v), "-")] = true
			}
		}
	}
	for _, gone := range []string{"view_count", "created_at", "excerpt", "org_id", "body"} {
		if names[gone] {
			t.Errorf("the document publishes a %q filter the resource will refuse", gone)
		}
		if enums[gone] {
			t.Errorf("the document offers %q in a sort or select enum", gone)
		}
	}
	// And it does publish what it serves, or the assertions above would pass on
	// an operation with no parameters at all.
	if !names["title"] || !names["status"] {
		t.Errorf("the narrowed resource lost its own filters: %v", names)
	}
	if !enums["title"] {
		t.Errorf("the narrowed resource lost its sort and select enums: %v", enums)
	}
}

// The limitation, asserted rather than left to be discovered: the *response
// schema* is the model's Go type, registered once as a component and shared by
// every mount of it, so it still lists the columns this resource does not
// serve. The runtime response omits them — TestANarrowedResourceReadsAndServes
// OnlyItsColumns is the assertion that matters — but a client generated from
// the document will carry optional fields that are always absent.
//
// It is recorded here because the honest reading of it is a scope boundary,
// not a bug to be fixed inside Options: a per-resource response schema needs a
// per-resource Go type, which is the generated second resource this option
// deliberately did not build. See rest.Options.Columns.
func TestTheResponseSchemaStillDescribesTheModel(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, publicOptions())

	schema := api.OpenAPI().Components.Schemas.Map()["Post"]
	if schema == nil {
		t.Fatal("the model's schema is not registered under its own name")
	}
	if _, ok := schema.Properties["view_count"]; !ok {
		t.Skip("the response schema is now narrowed per resource; delete this test and the paragraph in Options.Columns that predicts it")
	}
}

// A PATCH naming an excluded column is refused, as unknown rather than as
// read-only, and the writable list does not name it.
func TestANarrowedResourceRefusesAWriteToAnExcludedColumn(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, publicOptions())

	resp := api.Patch("/public/posts/p1", map[string]any{"body": "rewritten"})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.Code, resp.Body)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "unknown column") {
		t.Errorf("the rejection should read as unknown, got %s", body)
	}
	for _, stmt := range db.statements() {
		if strings.Contains(stmt, "UPDATE") {
			t.Errorf("a refused patch still issued an update:\n%s", stmt)
		}
	}
}

// Two refusals at mount, both startup-only because neither has a request that
// could report it.
func TestColumnsIsCheckedAgainstTheModel(t *testing.T) {
	db := newFakeDB(t)

	// A typo would otherwise be a resource serving one column fewer than
	// somebody meant, silently and forever.
	opts := publicOptions()
	opts.Columns = []string{"id", "titel"}
	err := mountErr(t, db.db, opts)
	if err == nil {
		t.Fatal("a Columns entry naming no column was accepted")
	}
	for _, want := range []string{"titel", "title"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q: %v", want, err)
		}
	}

	// The key addresses a row, settles the ordering and is what a cursor is
	// built from, so a surface without it cannot page.
	opts = publicOptions()
	opts.Columns = []string{"title", "status"}
	err = mountErr(t, db.db, opts)
	if err == nil {
		t.Fatal("a surface with no primary key was accepted")
	}
	for _, want := range []string{"primary key", "cursor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should explain itself, mentioning %q: %v", want, err)
		}
	}
}

// The point of the feature, and the assertion the rest of this file cannot
// make on its own: both surfaces exist at once, over one model, and the
// privileged one is unchanged.
func TestTheWideResourceIsUnaffectedByTheNarrowOne(t *testing.T) {
	db := newFakeDB(t,
		reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}},
		reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}},
	)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	for _, opts := range []rest.Options{publicOptions(), postOptions()} {
		if err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, opts); err != nil {
			t.Fatalf("mounting %s: %v", opts.Path, err)
		}
	}

	// The admin surface filters on the column the public one does not have.
	resp := api.Get("/posts?view_count=gte.3")
	if resp.Code != http.StatusOK {
		t.Fatalf("the wide resource lost a filter: %d %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), `"view_count"`) {
		t.Errorf("the wide resource stopped serving view_count:\n%s", resp.Body)
	}
	// And the narrow one still refuses it, from the same process and the same
	// model.
	if code := api.Get("/public/posts?view_count=gte.3").Code; code != http.StatusBadRequest {
		t.Errorf("the narrow resource answered %d for a column it does not serve", code)
	}
}

// mountErr registers the resource and returns the mounting error rather than
// failing, for the refusals above.
func mountErr(t *testing.T, db sqlb.Executor, opts rest.Options) error {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	return rest.Resource[Post, PostCreate, PostUpdate](api, db, opts)
}
