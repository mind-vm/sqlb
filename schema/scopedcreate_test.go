package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// "Which of my creates let the request choose the tenant it writes into?" (#313).
//
// The question is worth being able to answer from the schema and, until this
// rule, could not be: a Scoped column must be ReadOnly, so the guarantee used
// to be structural — the create body could not carry the tenant, and a
// BeforeCreate hook with only the context to read had only the verified
// principal to stamp it from. CreateInput reopened that without meaning to. A
// declared property may not take a column's name, but nothing stops one called
// "for_company" from being what the hook writes onto company_id.

// scopedCreate builds a tenant-scoped table with whatever REST exposure the
// case under test wants.
func scopedCreate(t *testing.T, rest schema.REST) schema.Diagnostics {
	t.Helper()
	r := schema.NewRegistry()
	co := r.Table("companies",
		schema.UUIDv7("id").PrimaryKey().Scoped(),
		schema.Text("name"),
	).Expose(schema.REST{Ops: schema.OpRead})

	r.Table("messages",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("company", co).ReadOnly().Scoped().Filterable(),
		schema.Text("body"),
	).Expose(rest)

	if err := r.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}
	return r.Lint()
}

func find(ds schema.Diagnostics, rule, table string) *schema.Diagnostic {
	for i := range ds {
		if ds[i].Rule == rule && ds[i].Table == table {
			return &ds[i]
		}
	}
	return nil
}

func TestAScopedCreateWithDeclaredInputIsNamed(t *testing.T) {
	ds := scopedCreate(t, schema.REST{
		Ops: schema.OpCreate | schema.OpList,
		CreateInput: schema.Body(
			schema.UUID("for_company").Comment("Which of the caller's companies this is written into."),
		),
	})

	d := find(ds, "scoped-create-takes-input", "messages")
	if d == nil {
		t.Fatalf("the create is not named, so the set is not enumerable:\n%s", ds)
	}
	if d.Severity != schema.SeverityInfo {
		t.Errorf("severity = %s, want info — the rule asks a question rather than reporting a fault", d.Severity)
	}
	// The diagnostic has to name the tenant column, not merely the table: the
	// reader's next action is to open the hook and look for a write to it.
	if d.Column != "company_id" {
		t.Errorf("column = %q, want the Scoped column", d.Column)
	}
	for _, want := range []string{`"for_company"`, "BeforeCreate", "company_id"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message does not name %q:\n%s", want, d.Message)
		}
	}
	if !strings.Contains(d.Fix, "re-resolved state") {
		t.Errorf("the fix does not say where the check belongs:\n%s", d.Fix)
	}
}

// The common case, and the one that must stay quiet: a scoped create whose
// tenant can only come from the principal, because there is no declared input
// for it to come from instead.
func TestAScopedCreateWithNoDeclaredInputIsSilent(t *testing.T) {
	ds := scopedCreate(t, schema.REST{Ops: schema.OpCreate | schema.OpList})
	if d := find(ds, "scoped-create-takes-input", "messages"); d != nil {
		t.Errorf("a create with no declared input was named:\n%s", d)
	}
}

// A table with declared input and no tenant has nothing to route anywhere, so
// the rule has nothing to ask about. This is the ordinary CreateInput case —
// the signup taking a password — and the rule firing on it would make it noise
// on every schema that uses the feature at all.
func TestAnUnscopedCreateWithDeclaredInputIsSilent(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("accounts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Varchar("password_hash", 255).Hidden(),
		schema.Text("email").Unique(),
	).Expose(schema.REST{
		Ops:         schema.OpCreate,
		CreateInput: schema.Body(schema.Text("password")),
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}
	if d := find(r.Lint(), "scoped-create-takes-input", "accounts"); d != nil {
		t.Errorf("an unscoped create was named:\n%s", d)
	}
}

// Deliberately over-inclusive, and this pins that it is a decision rather than
// an accident. The schema sees the property and the Scoped column and cannot
// see the hook that may or may not connect them, so a scoped create taking a
// password is named too. Under-inclusive would be the worse error: the whole
// value of the rule is that the set it prints is complete.
func TestTheRuleNamesEveryCandidateRatherThanGuessing(t *testing.T) {
	ds := scopedCreate(t, schema.REST{
		Ops:         schema.OpCreate,
		CreateInput: schema.Body(schema.Text("invite_token")),
	})
	if find(ds, "scoped-create-takes-input", "messages") == nil {
		t.Errorf("a property that plainly is not a tenant was skipped, so the set is a guess:\n%s", ds)
	}
}
