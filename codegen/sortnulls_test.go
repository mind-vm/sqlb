package codegen_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The declaration has to survive codegen as a struct tag, because the tag is
// the only thing the runtime model reads it back from. Without this the fix
// stops at the schema package and every generated resource keeps serving the
// default placement (#88).
func TestDeclaredNullPlacementReachesTheGeneratedTag(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Timestamp("published_at").Nullable().Sortable(schema.NullsLast),
		schema.Timestamp("retracted_at").Nullable().Sortable(schema.NullsFirst),
		schema.Timestamp("created_at").Sortable(),
	).Expose(schema.REST{Ops: schema.OpList})

	models := generate(t, r)["models_gen.go"]

	for _, want := range []string{
		`sqlb:"type:timestamptz,sort:nullslast"`,
		`sqlb:"type:timestamptz,sort:nullsfirst"`,
		// The column that declares nothing keeps the bare token beside the
		// type, rather than acquiring a placement it never asked for.
		`db:"created_at" json:"created_at" sqlb:"type:timestamptz,sort"`,
	} {
		if !contains(models, want) {
			t.Errorf("models are missing %q:\n%s", want, models)
		}
	}
}
