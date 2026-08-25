package pgtest

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// selfReferencingCategories is example/catalog/catalogschema's declaration: the
// table, then a second statement adding the reference back to it, which is the
// spelling a self-reference has now that AddField exists.
func selfReferencingCategories() *schema.Registry {
	r := schema.NewRegistry()
	c := r.Table("categories",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Filterable(),
	)
	c.AddField(schema.Ref("parent", c).Nullable().Filterable().Expandable())
	return r
}

// A declaration diffed against the database its own migration built proposes
// nothing.
//
// This is the loop `sqlb migrate` runs on every run after the first, and it is
// not what TestRoundTripIsAFixpoint covers: that compares a database to a
// database, introspect on both sides, so an index introspect invents lands on
// both and cancels. Only a *declaration* on one side can see it — which is why
// this file exists, and why the bug it pins survived a gate built to catch
// exactly this class (#259).
//
// The failure it was written against: introspect imports a self-referencing
// foreign key as an enforced ExternalRef, ExternalRef implied an index, and the
// table synthesised one from that implication — so the registry describing the
// database claimed an index the database did not have. The declaration asked
// for no such index, the diff resolved the disagreement as DROP INDEX on every
// run, and applying it failed with 42704 because nothing ever created it. The
// implication is gone; an index is [schema.Field.Indexed] or it is nothing.
func TestDeclaredSelfReferenceProposesNothingAgainstItsOwnDatabase(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	declared := selfReferencingCategories()
	applySchema(t, db, declared)

	current, _, err := introspect.Registry(context.Background(), sqlb.New(db), introspect.Options{})
	if err != nil {
		t.Fatalf("reading the database back: %v", err)
	}

	changes, err := migrate.Diff(current, declared)
	if err != nil {
		t.Fatalf("diffing the declaration against its own database: %v", err)
	}
	if len(changes) != 0 {
		var b strings.Builder
		for _, c := range changes {
			b.WriteString("\n  " + strings.TrimSpace(c.Up))
		}
		t.Errorf("a declaration diffed against the database its own migration built "+
			"proposed %d change(s):%s\n\nthe database has: %v",
			len(changes), b.String(), realIndexes(t, db, "categories"))
	}
}

// The same claim from the other side, and it fails differently: a registry that
// invents an index is wrong whether or not the diff happens to cancel it out. A
// change that made both sides invent the same index would leave the test above
// green while `sqlb migrate` still proposed dropping something imaginary.
func TestIntrospectClaimsNoIndexTheDatabaseDoesNotHave(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	applySchema(t, db, selfReferencingCategories())

	current, _, err := introspect.Registry(context.Background(), sqlb.New(db), introspect.Options{})
	if err != nil {
		t.Fatalf("reading the database back: %v", err)
	}

	real := realIndexes(t, db, "categories")
	for _, tbl := range current.Tables() {
		for _, idx := range tbl.Indexes() {
			if !hasName(real, idx.Name) {
				t.Errorf("introspect reports index %q on %s; the database has %v",
					idx.Name, tbl.Name(), real)
			}
		}
	}
}

// realIndexes is what Postgres says, which is the only authority either side of
// a diff is answerable to.
func realIndexes(t *testing.T, db *pgxpool.Pool, table string) []string {
	t.Helper()

	rows, err := db.Query(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE tablename = $1`, table)
	if err != nil {
		t.Fatalf("reading pg_indexes: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning pg_indexes: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading pg_indexes: %v", err)
	}
	sort.Strings(names)
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
