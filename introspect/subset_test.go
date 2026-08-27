package introspect

import (
	"strings"
	"testing"
)

// The database that used to be unreadable: one tsvector column, one index over
// it, and 68 other tables that could be imported perfectly well (issue #54).
func driftCatalog() *catalog {
	return &catalog{
		tables: []tableRow{{Name: "document_chunks"}, {Name: "projects"}, {Name: "goose_db_version"}},
		columns: []columnRow{
			{Table: "document_chunks", Name: "id", Type: "uuid", NotNull: true},
			{Table: "document_chunks", Name: "body", Type: "text", NotNull: true},
			{Table: "document_chunks", Name: "search_vector", Type: "tsvector"},
			{Table: "projects", Name: "id", Type: "uuid", NotNull: true},
			{Table: "projects", Name: "name", Type: "text", NotNull: true},
			{Table: "goose_db_version", Name: "id", Type: "bigint", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "document_chunks", Name: "document_chunks_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "projects", Name: "projects_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "goose_db_version", Name: "goose_db_version_pkey", Type: "p", Columns: []string{"id"}},
		},
		indexes: []indexRow{
			{
				Table: "document_chunks", Name: "idx_document_chunks_search_vector",
				Columns: []string{"search_vector"}, Method: "gin",
				Def: "CREATE INDEX idx_document_chunks_search_vector ON document_chunks USING gin (search_vector)",
			},
			{Table: "projects", Name: "idx_projects_name", Columns: []string{"name"}, Method: "btree"},
		},
	}
}

// One unmodelable column used to abort the whole import, because the index over
// it was kept and the registry then failed its own validation.
func TestSkippedColumnTakesItsIndexWithIt(t *testing.T) {
	r, rep, err := build(driftCatalog(), Options{})
	if err != nil {
		t.Fatalf("one unmodelable column should not abort the import: %v", err)
	}

	// Everything else is imported, which is the whole point: a drift gate over
	// the other tables is now buildable.
	if r.Get("projects") == nil {
		t.Fatal("the tables that can be imported were lost with the one that cannot")
	}
	if len(r.Get("projects").Indexes()) != 1 {
		t.Error("an unrelated table's index was dropped")
	}

	chunks := r.Get("document_chunks")
	if chunks == nil {
		t.Fatal("the table itself is importable; only its tsvector column is not")
	}
	if chunks.Field("search_vector") != nil {
		t.Error("the tsvector column should have been skipped")
	}
	if len(chunks.Indexes()) != 0 {
		t.Error("the index over the skipped column should have gone with it")
	}

	// And the report says both things, so the gap is visible rather than
	// silent.
	for _, want := range []string{"search_vector", "idx_document_chunks_search_vector", "not imported"} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("the report does not mention %q:\n%s", want, rep)
		}
	}
}

// A check constraining a skipped column goes with it too, for the same reason:
// the DDL it would produce names a column the registry does not have.
func TestSkippedColumnTakesItsCheckWithIt(t *testing.T) {
	cat := driftCatalog()
	cat.constraints = append(cat.constraints, constraintRow{
		Table: "document_chunks", Name: "search_vector_present", Type: "c",
		Expr: "search_vector IS NOT NULL", Def: "CHECK (search_vector IS NOT NULL)",
	})
	// A check over a column that *was* imported stays.
	cat.constraints = append(cat.constraints, constraintRow{
		Table: "document_chunks", Name: "body_not_empty", Type: "c",
		Expr: "length(body) > 0", Def: "CHECK (length(body) > 0)",
	})

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	checks := r.Get("document_chunks").Checks()
	if len(checks) != 1 || checks[0].Name != "body_not_empty" {
		t.Errorf("checks = %v, want only body_not_empty", checks)
	}
	if !strings.Contains(rep.String(), "search_vector_present") {
		t.Errorf("the dropped check should be reported:\n%s", rep)
	}
}

// A column whose name merely contains a skipped column's name is not a
// dependent — the match is on word boundaries.
func TestCheckOnASimilarlyNamedColumnIsKept(t *testing.T) {
	cat := driftCatalog()
	cat.columns = append(cat.columns, columnRow{
		Table: "document_chunks", Name: "search_vector_backup", Type: "text",
	})
	cat.constraints = append(cat.constraints, constraintRow{
		Table: "document_chunks", Name: "backup_present", Type: "c",
		Expr: "search_vector_backup IS NOT NULL", Def: "CHECK (search_vector_backup IS NOT NULL)",
	})

	r, _, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(r.Get("document_chunks").Checks()) != 1 {
		t.Error("a check over a differently-named column was dropped with the skipped one")
	}
}

// The other half of the gate: an incremental adoption declares a few tables and
// the database holds dozens, so the import has to be narrowable.
func TestOnlyImportsTheNamedTables(t *testing.T) {
	r, rep, err := build(driftCatalog(), Options{Only: []string{"projects"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(r.Tables()) != 1 || r.Get("projects") == nil {
		t.Errorf("tables = %d, want only projects", len(r.Tables()))
	}
	if rep.String() != "" && strings.Contains(rep.String(), "document_chunks") {
		t.Errorf("a table nobody asked for should not be reported:\n%s", rep)
	}
}

// A typo in Only would silently shrink what a gate covers, so it is reported.
func TestOnlyReportsANameThatIsNotThere(t *testing.T) {
	_, rep, err := build(driftCatalog(), Options{Only: []string{"projects", "projekts"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(rep.String(), "projekts") {
		t.Errorf("a name that matched nothing should be reported:\n%s", rep)
	}
}

// Exclude is the shape the migration-history table wants: present in every
// database, described by no declaration.
func TestExcludeDropsTheNamedTables(t *testing.T) {
	r, _, err := build(driftCatalog(), Options{Exclude: []string{"goose_db_version"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r.Get("goose_db_version") != nil {
		t.Error("an excluded table was imported")
	}
	if r.Get("projects") == nil {
		t.Error("Exclude removed more than it was given")
	}
}

// The two compose, and Exclude applies after Only.
func TestOnlyAndExcludeCompose(t *testing.T) {
	r, _, err := build(driftCatalog(), Options{
		Only:    []string{"projects", "goose_db_version"},
		Exclude: []string{"goose_db_version"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(r.Tables()) != 1 || r.Get("projects") == nil {
		t.Errorf("tables = %d, want only projects", len(r.Tables()))
	}
}

// Narrowing the import does not lose a foreign key into a table left out: it
// imports as an enforced external reference, which is exactly what a partial
// declaration would have written by hand (issue #55).
func TestForeignKeyIntoAnExcludedTableSurvivesAsEnforced(t *testing.T) {
	cat := driftCatalog()
	cat.tables = append(cat.tables, tableRow{Name: "organizations"})
	cat.columns = append(cat.columns,
		columnRow{Table: "organizations", Name: "id", Type: "uuid", NotNull: true},
		columnRow{Table: "projects", Name: "org_id", Type: "uuid", NotNull: true},
	)
	cat.constraints = append(cat.constraints,
		constraintRow{Table: "organizations", Name: "organizations_pkey", Type: "p", Columns: []string{"id"}},
		constraintRow{
			Table: "projects", Name: "projects_org_id_fkey", Type: "f",
			Columns: []string{"org_id"}, RefTable: "organizations", RefCols: []string{"id"},
			Def: "FOREIGN KEY (org_id) REFERENCES organizations(id)",
		},
	)

	r, _, err := build(cat, Options{Only: []string{"projects"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ref := r.Get("projects").Field("org_id").Desc().Ref
	if ref == nil || !ref.Enforced {
		t.Fatalf("the foreign key should survive as an enforced external reference, got %+v", ref)
	}
	if _, table, column, _ := ref.EnforcedTarget(); table != "organizations" || column != "id" {
		t.Errorf("target = %s.%s, want organizations.id", table, column)
	}
}
