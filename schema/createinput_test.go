package schema_test

import (
	"testing"

	"github.com/jryannel/sqlb/schema"
)

// The create body's non-column half (#309), and the four things it may not be.
//
// childrenWith is the issue's own case: a table storing a bcrypt digest,
// exposed with a create that takes the plaintext the digest is derived from.
func childrenWith(rest schema.REST) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		schema.Timestamp("created_at").Default(schema.Now()).ReadOnly(),
		schema.Varchar("pin_hash", 255).Hidden(),
	).Expose(rest)
	return r
}

func TestAValidCreateInputPasses(t *testing.T) {
	r := childrenWith(schema.REST{
		Ops: schema.OpCreate | schema.Reads,
		CreateInput: schema.Body(
			schema.Varchar("pin", 4).Comment("Four digits. Hashed on the way in."),
		),
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("a well-formed create input was refused: %v", err)
	}

	props := r.Get("children").Rest().CreateInput
	if len(props) != 1 || props[0].Desc().Name != "pin" {
		t.Fatalf("the declaration did not survive Expose: %+v", props)
	}
}

// A property is a value. Everything that places a column in a table is refused
// rather than ignored, because a declaration that reads as though it did
// something is worse than one that does not compile.
func TestCreateInputRefusesColumnCapabilities(t *testing.T) {
	for _, tc := range []struct {
		claim string
		spec  schema.FieldSpec
	}{
		{"Filterable", schema.Text("pin").Filterable()},
		{"PrimaryKey", schema.Text("pin").PrimaryKey()},
		{"Hidden", schema.Text("pin").Hidden()},
		{"WriteOnly", schema.Text("pin").WriteOnly()},
		{"Sortable", schema.Text("pin").Sortable()},
	} {
		t.Run(tc.claim, func(t *testing.T) {
			r := childrenWith(schema.REST{Ops: schema.OpCreate, CreateInput: schema.Body(tc.spec)})
			refusal(t, r, "CreateInput", tc.claim, "describes a column rather than a declared property")
		})
	}
}

// One JSON object cannot have two meanings for one key, and the generated body
// would carry both under one tag.
func TestCreateInputRefusesAColumnName(t *testing.T) {
	r := childrenWith(schema.REST{
		Ops:         schema.OpCreate,
		CreateInput: schema.Body(schema.Text("name")),
	})
	refusal(t, r, `property "name" is already a column`)
}

// The collision that only exists on the wire: under Camel the column pin_hash
// is spelled pinHash, so a property of that name is the same key. Comparing raw
// names alone would let it through, and the failure would be a Go struct with
// two identical json tags.
func TestCreateInputRefusesAWireNameCollision(t *testing.T) {
	r := schema.NewRegistry().WireCase(schema.Camel)
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Varchar("pin_hash", 255).Hidden(),
	).Expose(schema.REST{
		Ops:         schema.OpCreate,
		CreateInput: schema.Body(schema.Text("pinHash")),
	})
	refusal(t, r, `"pinHash" is how column "pin_hash" is spelled on the wire`)
}

// Without a create there is no body for the property to be part of, so nothing
// would be emitted and nothing would refuse the request that sent it.
func TestCreateInputNeedsACreate(t *testing.T) {
	r := childrenWith(schema.REST{
		Ops:         schema.Reads,
		CreateInput: schema.Body(schema.Text("pin")),
	})
	refusal(t, r, "does not expose OpCreate")
}

func TestCreateInputRefusesADuplicateProperty(t *testing.T) {
	r := childrenWith(schema.REST{
		Ops:         schema.OpCreate,
		CreateInput: schema.Body(schema.Text("pin"), schema.Text("pin")),
	})
	refusal(t, r, "declared twice")
}

// The manifest is what an agent reads, and a property that is not in it is one
// no reader can discover: it is absent from the column table by construction.
func TestCreateInputReachesTheManifest(t *testing.T) {
	r := childrenWith(schema.REST{
		Ops:         schema.OpCreate | schema.Reads,
		CreateInput: schema.Body(schema.Varchar("pin", 4)),
	})
	m := r.BuildManifest()
	for _, tm := range m.Tables {
		if tm.Name != "children" {
			continue
		}
		props := tm.REST.CreateInput
		if len(props) != 1 || props[0].Name != "pin" || props[0].Type != string(schema.TypeVarchar) {
			t.Fatalf("the manifest does not describe the create input: %+v", props)
		}
		return
	}
	t.Fatal("children is not in the manifest")
}
