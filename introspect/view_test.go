package introspect

import (
	"strings"
	"testing"
)

// TestBuildMapsAView exercises the read path relkind='v' feeds — a view row
// alongside an ordinary table, so the two catalogs (tables/columns and
// views/viewColumns) are proven not to leak into each other's build pass.
func TestBuildMapsAView(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "users"}},
		columns: []columnRow{
			{Table: "users", Name: "id", Type: "uuid", NotNull: true},
			{Table: "users", Name: "email", Type: "text", NotNull: true},
			{Table: "users", Name: "active", Type: "boolean", NotNull: true},
		},
		views: []viewRow{
			{Name: "active_users", Comment: "users with active = true", Query: "SELECT id, email FROM users WHERE active"},
		},
		viewColumns: []viewColumnRow{
			{View: "active_users", Name: "id", Type: "uuid"},
			{View: "active_users", Name: "email", Type: "text"},
		},
	}

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("nothing here is beyond the DSL, but:\n%s", rep)
	}

	if len(r.Tables()) != 1 {
		t.Fatalf("Tables() should not include the view, got %d", len(r.Tables()))
	}
	views := r.Views()
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Name() != "active_users" {
		t.Errorf("view name = %q", v.Name())
	}
	if v.ViewQuery() != "SELECT id, email FROM users WHERE active" {
		t.Errorf("view query = %q", v.ViewQuery())
	}
	if v.Comment() != "users with active = true" {
		t.Errorf("view comment = %q", v.Comment())
	}
	if len(v.Fields()) != 2 {
		t.Fatalf("want 2 columns, got %d", len(v.Fields()))
	}
	if v.Fields()[0].Desc().Name != "id" || v.Fields()[1].Desc().Name != "email" {
		t.Errorf("column order/names wrong: %+v", v.Fields())
	}
}

// TestBuildReportsAnUnrepresentableViewColumn confirms a view whose column
// type the DSL cannot declare is refused as a whole (not declared with a
// silently narrower column set) and named in the Report, the same
// all-or-nothing rule a table's own unrepresentable column follows.
func TestBuildReportsAnUnrepresentableViewColumn(t *testing.T) {
	cat := &catalog{
		views: []viewRow{
			{Name: "odd_view", Query: "SELECT id, weird FROM somewhere"},
		},
		viewColumns: []viewColumnRow{
			{View: "odd_view", Name: "id", Type: "uuid"},
			{View: "odd_view", Name: "weird", Type: "tsvector"},
		},
	}

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(r.Views()) != 0 {
		t.Fatalf("a view with an unrepresentable column should not be declared at all, got %d", len(r.Views()))
	}
	if !strings.Contains(rep.String(), "odd_view") {
		t.Errorf("Report should name odd_view:\n%s", rep)
	}
}
