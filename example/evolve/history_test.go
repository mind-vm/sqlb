package evolve_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"

	// Imported for its side effects: this is the current declaration, and the
	// other side of every comparison below.
	_ "github.com/mind-vm/sqlb/example/evolve/evolveschema"
)

// A schema edit changes two things, and they drift independently: the code sqlb
// generates, and the API that code serves. `mise run generate-check` gates the
// first. This is about the second, and about the case where the two disagree —
// revision 4 of the history, where a rename produced a clean migration, a clean
// regeneration, and a broken client.
//
// The comparison needs a *previous* contract to compare against, and this
// example deliberately keeps no previous schema package: evolveschema is the
// current state and the migrations are the history. So the old side is built
// here, as a fixture, which is the same thing `sqlb impact` does with a
// committed restcontract.json — "backward compatible relative to what?" has to
// have a recorded answer, and a fixture is one.

// beforeTheRenames is the schema as of revision 3 — after the safe additions
// and the widened subject, before email became email_address and agents became
// support_agents, and while tickets still carried legacy_ref.
//
// extraTicketFields is how the additive case below gets a fourth revision
// without a fourth copy of all of this.
func beforeTheRenames(extraTicketFields ...schema.FieldSpec) *schema.Registry {
	r := schema.NewRegistry()

	customers := r.Table("customers",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique().Searchable(),
		schema.Text("name").Searchable().Sortable(),
		schema.Timestamps(),
	).
		Describe("Whoever a ticket is on behalf of.").
		Expose(schema.REST{
			Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
			DefaultPageSize: 25,
			MaxPageSize:     100,
		})

	fields := []schema.FieldSpec{
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("customer", customers).OnDelete(schema.Cascade).Filterable().Expandable(),
		schema.Text("subject").Searchable().Sortable(),
		schema.Text("body").Searchable(),
		schema.Enum("status", "open", "pending", "closed").
			Default(schema.Value("open")).Filterable().Sortable(),
		schema.Enum("priority", "low", "normal", "high", "urgent").
			Default(schema.Value("normal")).Filterable().Sortable(),
		schema.Text("legacy_ref").Nullable(),
		schema.Timestamps(),
	}
	fields = append(fields, extraTicketFields...)

	r.Table("tickets", fields...).
		Index("customer_id", "status").
		Describe("One request from one customer.").
		Expose(schema.REST{
			Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
			DefaultPageSize: 20,
			MaxPageSize:     100,
		})

	r.Table("agents",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique(),
		schema.Text("name").Searchable().Sortable(),
		schema.Bool("active").Default(schema.Value(true)).Filterable(),
		schema.Timestamps(),
	).
		Describe("Someone who answers tickets.").
		Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	return r
}

// The claim the document makes about revisions 4 and 5: a rename and a drop are
// clean migrations and broken clients, and nothing about the DDL says so.
func TestTheRenamesAndTheDropBreakClients(t *testing.T) {
	breaks := restcompat.Breaking(restcompat.Diff(beforeTheRenames(), schema.DefaultRegistry()))
	if len(breaks) == 0 {
		t.Fatal("renaming an exposed column, renaming a resource and dropping a response field broke nothing, which cannot be right")
	}

	report := summarise(breaks)
	for _, want := range []struct{ what, needle string }{
		// Revision 4: the column a client filters and reads by name.
		{"the renamed column", "email"},
		// Revision 4: the collection path itself moved.
		{"the renamed table", "agents"},
		// Revision 5: a field that was in every response is gone.
		{"the dropped column", "legacy_ref"},
	} {
		if !strings.Contains(report, want.needle) {
			t.Errorf("%s is not reported as a break:\n%s", want.what, report)
		}
	}
}

// The other direction, so the test above is not passing because this diff
// reports everything as a break: adding a nullable column is additive, and a
// client that ignores it is unaffected.
//
// This is the shape of every revision the document calls safe, and it is why
// they get one paragraph while the renames get a section each.
func TestAddingAColumnBreaksNothing(t *testing.T) {
	before := beforeTheRenames()
	after := beforeTheRenames(schema.Text("resolution_note").Nullable())

	all := restcompat.Diff(before, after)
	if len(all) == 0 {
		t.Fatal("adding a column changed nothing in the contract, so this comparison is not looking at it")
	}
	if breaks := restcompat.Breaking(all); len(breaks) != 0 {
		t.Errorf("adding a nullable column is not a breaking change:\n%s", summarise(breaks))
	}
}

func summarise(breaks []restcompat.Break) string {
	var b strings.Builder
	for _, br := range breaks {
		b.WriteString("  " + br.String() + "\n")
	}
	return b.String()
}
