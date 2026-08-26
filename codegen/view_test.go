package codegen_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// viewFixture is fixture() plus one view over blog_entries, exposed
// read-only — the shape Part 3 of the Drizzle-comparison plan promised: a
// view declared in the schema DSL gets a model and a read-only REST
// resource with no new codegen machinery of its own.
func viewFixture() *schema.Registry {
	r := fixture()
	r.View("published_titles",
		`SELECT id, title FROM blog_entries WHERE status = 'published'`,
		schema.UUID("id").PrimaryKey(),
		schema.Text("title").Filterable().Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})
	return r
}

func TestGeneratedModelsIncludesAView(t *testing.T) {
	models := generate(t, viewFixture())["models_gen.go"]

	for _, want := range []string{
		"type PublishedTitle struct {",
		`func (PublishedTitle) TableName() string { return "published_titles" }`,
		`Title string ` + "`" + `db:"title" json:"title" sqlb:"type:text,filter,sort"`,
	} {
		if !contains(models, want) {
			t.Errorf("models missing %q:\n%s", want, models)
		}
	}
}

func TestGeneratedRESTExposesAViewReadOnly(t *testing.T) {
	rest := generate(t, viewFixture())["rest_gen.go"]

	if !contains(rest, "PublishedTitle") {
		t.Fatalf("rest_gen.go does not mention the view's model at all:\n%s", rest)
	}
	for _, want := range []string{
		"rest.None[PublishedTitle]", // no create body — the view exposes no OpCreate
	} {
		if !contains(rest, want) {
			t.Errorf("rest missing %q:\n%s", want, rest)
		}
	}
}
