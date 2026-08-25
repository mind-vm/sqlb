package restcompat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// The create body's non-column half is part of the contract (#309), for the
// reason a column is: a deployed client sends this body, and a property that
// arrives required fails every request that client already makes — with no DDL
// anywhere in the change, which is the premise of `sqlb impact`.

func withCreateInput(props ...schema.FieldSpec) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		schema.Varchar("pin_hash", 255).Hidden(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList, CreateInput: schema.Body(props...)})
	return r
}

func TestANewRequiredCreateInputIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(withCreateInput(), withCreateInput(schema.Varchar("pin", 4)))

	b := find(t, breaks, "required body property added")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Facet != restcompat.FacetCreate {
		t.Errorf("facet = %s, want create-body", b.Facet)
	}
	// Unqualified: a resource has one create body, so the property name alone
	// says where it is. An action's is "complete.note", because it does not.
	if b.Field != "pin" {
		t.Errorf("field = %q, want the bare property name", b.Field)
	}
}

func TestANewOptionalCreateInputIsAdditive(t *testing.T) {
	breaks := restcompat.Diff(withCreateInput(), withCreateInput(schema.Text("invite_code").Nullable()))

	if b := find(t, breaks, "optional body property added"); b.Level != restcompat.LevelAdditive {
		t.Errorf("level = %s, want additive", b.Level)
	}
}

// Tightening one is the same break arriving by a different route, and the
// diff has to see it: the property was already there, and a client that omits
// it stops working the day it stops being nullable.
func TestACreateInputBecomingRequiredIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(
		withCreateInput(schema.Varchar("pin", 4).Nullable()),
		withCreateInput(schema.Varchar("pin", 4)),
	)

	if b := find(t, breaks, "body property became required"); b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
}

// A schema that declares none records none, so a baseline taken before this
// existed reads as what it was rather than as a resource that lost something.
func TestNoCreateInputIsNoContract(t *testing.T) {
	snap := restcompat.Capture(withCreateInput())
	if got := snap.Resources[0].CreateInput; len(got) != 0 {
		t.Errorf("recorded %v for a resource that declares nothing", got)
	}
	if breaks := restcompat.Diff(withCreateInput(), withCreateInput()); len(breaks) != 0 {
		t.Errorf("a schema compared with itself reports %v", breaks)
	}
}

// The recorded shape is what a committed baseline holds, so it is asserted
// rather than left to the diff: a key that changed spelling would make every
// existing restcontract.json read as a resource that lost its inputs.
func TestCreateInputIsRecordedInTheSnapshot(t *testing.T) {
	snap := restcompat.Capture(withCreateInput(schema.Varchar("pin", 4), schema.Text("note").Nullable()))
	props := snap.Resources[0].CreateInput
	if len(props) != 2 {
		t.Fatalf("recorded %v", props)
	}
	if props[0].Name != "pin" || props[0].Type != string(schema.TypeVarchar) || props[0].Nullable {
		t.Errorf("first property = %+v", props[0])
	}
	if !props[1].Nullable {
		t.Errorf("second property = %+v, want it recorded nullable", props[1])
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"create_input"`) {
		t.Errorf("the snapshot does not carry the create input under its own key:\n%s", data)
	}
}
