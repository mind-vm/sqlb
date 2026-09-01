package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The refusals `skills/sqlb-authoring/SKILL.md` promises, executed.
//
// `skill-check` keeps that file's *vocabulary* level with the DSL: a method the
// engine declares and the skill does not name fails the gate, and a row naming
// something that does not exist fails it too. What no grep can check is the
// other half of what the skill says — the claims about which declarations are
// *refused*, which are prose restating `registry.go`'s report sites and are
// exactly what #293 calls "prose duplicating a check: the worst of both,
// because it is the copy that can be wrong."
//
// They were all correct when this was written, which is the point: the citation
// rot that prompted `skill-check` was invisible for months too. A claim about a
// refusal is executable, so it should be executed rather than believed.
//
// Adding a refusal claim to the skill means adding a case here. That is a rule
// nothing enforces, and it is a smaller gap than the one it replaces — every
// claim currently in the file is now pinned, where before none was.

// refusal asserts that a declaration the skill calls refused is refused, and
// that the message names the column rather than only the table.
func refusalClaim(t *testing.T, claim string, build func(*schema.Registry), wants ...string) {
	t.Helper()
	r := schema.NewRegistry()
	build(r)

	err := r.Validate()
	if err == nil {
		t.Fatalf("the skill says %q, and the validator accepted it", claim)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal for %q does not mention %q:\n  %v", claim, w, err)
		}
	}
}

func TestTheSkillsHiddenClaimsAreRefusals(t *testing.T) {
	// "refused on a non-`Hidden` column"
	refusalClaim(t, "LookupKey on a non-Hidden column", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.Text("tok").LookupKey())
	}, "t.tok", "Hidden")

	// "column is both Hidden and Filterable, which leaks its contents through
	// filter probing" — the refusal the skill's own first table row rests on.
	refusalClaim(t, "Hidden and Filterable on one column", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.Text("s").Hidden().Filterable())
	}, "t.s", "Filterable")
}

func TestTheSkillsPrimaryKeyClaimsAreRefusals(t *testing.T) {
	// "refused if also `Hidden`/`WriteOnly` — a response needs it to address
	// the row"
	refusalClaim(t, "a Hidden primary key", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey().Hidden(), schema.Text("n"))
	}, "t.id", "primary key")
	refusalClaim(t, "a WriteOnly primary key", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey().WriteOnly(), schema.Text("n"))
	}, "t.id", "primary key")
}

func TestTheSkillsScopedClaimsAreRefusals(t *testing.T) {
	// "`Nullable` is refused together with `Scoped`" — and the reason, which
	// the skill states and which the message should carry too: a row whose
	// tenant is NULL is outside every tenant's predicate.
	refusalClaim(t, "a Nullable Scoped column", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("org").Nullable().Scoped().ReadOnly())
	}, "t.org", "Nullable", "NULL")

	// "It must be `ReadOnly` — otherwise a create request names its own
	// tenant". The one the whole of #313 turns on.
	refusalClaim(t, "a Scoped column that is not ReadOnly", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.UUID("org").Scoped())
	}, "t.org", "ReadOnly")
}

func TestTheSkillsConstraintClaimsAreRefusals(t *testing.T) {
	// "refused without `Unique()` — there is otherwise no constraint to defer"
	refusalClaim(t, "Deferred without Unique", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.Text("n").Deferred())
	}, "t.n", "Unique")

	// "refused if the column is not a `Ref`"
	refusalClaim(t, "Expandable on a non-Ref column", func(r *schema.Registry) {
		r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.Text("n").Expandable())
	}, "t.n", "Ref")
}
