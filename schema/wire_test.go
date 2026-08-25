package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The transformation and its inverse, over the names a real schema has.
func TestWireCaseRoundTrip(t *testing.T) {
	cases := []struct{ column, wire string }{
		{"created_at", "createdAt"},
		{"id", "id"},
		{"org_id", "orgId"},
		{"pos_x", "posX"},
		{"contracted_hours_per_week", "contractedHoursPerWeek"},
		// A leading underscore is structural, not a separator: dropping it
		// would collide _internal and internal onto one wire name.
		{"_internal", "_internal"},
	}
	for _, c := range cases {
		if got := schema.Camel.WireName(c.column); got != c.wire {
			t.Errorf("WireName(%q) = %q, want %q", c.column, got, c.wire)
		}
		if got := schema.Camel.ColumnName(c.wire); got != c.column {
			t.Errorf("ColumnName(%q) = %q, want %q", c.wire, got, c.column)
		}
	}
}

// Verbatim is the identity function in both directions, which is what makes it
// a safe default rather than a special case every caller has to remember.
func TestVerbatimIsIdentity(t *testing.T) {
	for _, n := range []string{"created_at", "posX", "_x", "pos_x_2"} {
		if got := schema.Verbatim.WireName(n); got != n {
			t.Errorf("WireName(%q) = %q", n, got)
		}
		if got := schema.Verbatim.ColumnName(n); got != n {
			t.Errorf("ColumnName(%q) = %q", n, got)
		}
	}
}

// The failure the amendment names: a digit boundary does not survive, so the
// schema is refused at build time rather than shipping a client that asks for a
// column no table has.
func TestWireCaseRefusesNamesItCannotRecover(t *testing.T) {
	r := schema.NewRegistry().WireCase(schema.Camel)
	r.Table("events",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Int("pos_x_2"),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("pos_x_2 does not round trip and must be refused")
	}
	for _, want := range []string{"pos_x_2", "posX2", "pos_x2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q, so it cannot be acted on:\n%v", want, err)
		}
	}
	// And it says what to do about it.
	if !strings.Contains(err.Error(), "Verbatim") {
		t.Errorf("the error does not offer the way out:\n%v", err)
	}
}

// Two columns landing on one wire name is the other half of the same failure.
func TestWireCaseRefusesACollision(t *testing.T) {
	r := schema.NewRegistry().WireCase(schema.Camel)
	r.Table("t",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("posX"),
		schema.Text("pos_x"),
	)
	if err := r.Validate(); err == nil {
		t.Fatal("two columns spelled the same on the wire must be refused")
	}
}

// A schema whose names all survive validates, which is the case that has to
// keep working for the feature to be usable at all.
func TestWireCaseAcceptsOrdinaryNames(t *testing.T) {
	r := schema.NewRegistry().WireCase(schema.Camel)
	r.Table("members",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("display_name"),
		schema.Timestamp("created_at"),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("ordinary snake_case must survive: %v", err)
	}
	if r.Wire() != schema.Camel {
		t.Errorf("Wire() = %q", r.Wire())
	}
}

// Verbatim runs no check at all, so a name that camel could not recover is
// simply a column name — which is what every schema written before this is.
func TestVerbatimAcceptsAnything(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("events", schema.UUIDv7("id").PrimaryKey(), schema.Int("pos_x_2"))
	if err := r.Validate(); err != nil {
		t.Fatalf("Verbatim must not police column names: %v", err)
	}
}

// The manifest's REST section is a description of the wire, so its capability
// lists and its worked examples carry wire names (#143). This is where the bug
// was: the skill emitter was a faithful render of a manifest that was itself
// wrong, so anything else reading BuildManifest for a contract was wrong the
// same way.
func TestManifestRESTSectionIsTheWire(t *testing.T) {
	m := camelManifest(t)

	table := m.Tables[0]
	if got := table.REST.Filterable; len(got) != 2 || got[0] != "id" || got[1] != "orgId" {
		t.Errorf("filterable = %v, want the wire spellings [id orgId]", got)
	}
	if got := table.REST.Sortable; len(got) != 1 || got[0] != "createdAt" {
		t.Errorf("sortable = %v, want [createdAt]", got)
	}
	// The examples are copy-pasteable requests, which is what makes a column
	// name in one a request that 400s rather than a cosmetic slip.
	for _, ex := range table.REST.Examples {
		if strings.Contains(ex, "org_id") || strings.Contains(ex, "created_at") {
			t.Errorf("example request carries a database spelling: %q", ex)
		}
	}
	joined := strings.Join(table.REST.Examples, "\n")
	for _, want := range []string{"sort=-createdAt", "orgId.eq.B"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no example carries %q: %v", want, table.REST.Examples)
		}
	}
}

// Both spellings, per column, so a consumer needing the other one has it without
// reimplementing the transformation. Absent where they coincide, which keeps a
// Verbatim schema's manifest byte-identical to the one it emitted before.
func TestManifestCarriesBothSpellings(t *testing.T) {
	byName := map[string]schema.ColumnManifest{}
	for _, c := range camelManifest(t).Tables[0].Columns {
		byName[c.Name] = c
	}
	if got := byName["org_id"].Wire; got != "orgId" {
		t.Errorf("org_id.wire = %q, want orgId", got)
	}
	if got := byName["id"].Wire; got != "" {
		t.Errorf("id.wire = %q, want it absent — the two spellings are the same", got)
	}

	verbatim := schema.NewRegistry()
	verbatim.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("org_id").Filterable())
	vm := verbatim.BuildManifest()
	if vm.WireCase != "" {
		t.Errorf("wireCase = %q on a schema that declared none", vm.WireCase)
	}
	for _, c := range vm.Tables[0].Columns {
		if c.Wire != "" {
			t.Errorf("%s.wire = %q, but Verbatim spells a column its own name", c.Name, c.Wire)
		}
	}
}

// The case itself is on the document, because a renderer writing prose about the
// mapping cannot otherwise tell "there is no mapping" from "here is the one".
func TestManifestNamesItsWireCase(t *testing.T) {
	if got := camelManifest(t).WireCase; got != string(schema.Camel) {
		t.Errorf("wireCase = %q, want %q", got, schema.Camel)
	}
}

func camelManifest(t *testing.T) *schema.Manifest {
	t.Helper()
	r := schema.NewRegistry().WireCase(schema.Camel)
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.BigInt("org_id").Filterable(),
		schema.Timestamp("created_at").Sortable(),
	).Expose(schema.REST{Path: "/posts", Ops: schema.Reads})
	if err := r.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	return r.BuildManifest()
}
