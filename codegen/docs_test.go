package codegen_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// docsFixture is one exposed table wired to exercise every source `sqlb docs`
// reads: a filterable/sortable/searchable column and an expandable reference
// (list facts), a required column and an optional one with a default (create
// facts), an immutable column (excluded from update facts), a table comment,
// one declared action with a body and a write/touch set, and one declared
// query with params and a cross-table read.
func docsFixture(path string, ops schema.Op) *schema.Registry {
	r := schema.NewRegistry()
	authors := r.Table("authors",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	).Expose(schema.REST{Path: "/authors", Ops: schema.OpRead})

	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable().Filterable().Searchable(),
		schema.Text("body").Nullable(),
		schema.Text("status").Default(schema.Value("draft")).Immutable(),
		schema.Ref("author", authors).Nullable().Expandable(),
	).Describe("Blog posts belonging to an org.").
		Expose(schema.REST{Path: path, Ops: ops, DefaultPageSize: 25, MaxPageSize: 100}).
		AddAction(schema.Action{
			Name:        "publish",
			Body:        schema.Body(schema.Text("note").Nullable()),
			Writes:      []string{"status"},
			Touches:     []string{"subscriptions"},
			Description: "Publishes a draft post and notifies subscribers.",
		}).
		AddQuery(schema.Query{
			Name:   "overdue",
			Params: schema.Body(schema.Timestamp("as_of")),
			Reads:  []*schema.TableDef{authors},
		})
	return r
}

func docsProject(reg *schema.Registry, featuresFile string, ops ...codegen.HandwrittenOp) codegen.Project {
	return codegen.Project{
		Options:        codegen.Options{Dir: ".", Package: "blog", Registry: reg},
		FeaturesFile:   featuresFile,
		HandwrittenOps: ops,
	}
}

// fillNote replaces a notes block's body in raw, for a test simulating an
// agent (or teammate) writing into the file between two runs of `sqlb docs`.
func fillNote(t *testing.T, raw, key, body string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)(<!-- sqlb:notes ` + regexp.QuoteMeta(key) + ` -->\n).*?(\n<!-- /sqlb:notes -->)`)
	if !re.MatchString(raw) {
		t.Fatalf("no notes block found for key %q in:\n%s", key, raw)
	}
	return re.ReplaceAllString(raw, "${1}"+body+"${2}")
}

func TestDocsWritesEndpointsFactsAndTheStructuredNotesTemplate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "FEATURES.md")
	code, out := run(t, docsProject(docsFixture("/posts", schema.CRUD|schema.OpList), file), "docs")
	if code != 0 {
		t.Fatalf("docs should succeed, got %d (%s)", code, out)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("FEATURES.md not written: %v", err)
	}
	got := string(raw)

	for _, want := range []string{
		// Endpoints and their schema-authored comments.
		"Blog posts belonging to an org.",
		"`GET /posts` — list",
		"`POST /posts` — create",
		"`GET /posts/{id}` — read",
		"`PATCH /posts/{id}` — update",
		"`DELETE /posts/{id}` — delete",
		"`POST /posts/{id}/publish` — action: publish",
		"`GET /posts/overdue` — query: overdue",
		"Publishes a draft post and notifies subscribers.",
		// Facts pre-populated above the notes block, so an agent's writing
		// time goes to what only it can supply.
		"Filterable: id, title",
		"Sortable: title",
		"Searchable: title",
		"Expandable: author",
		"Page size: default 25, max 100",
		"Required: title",
		"Optional: body, status, author",
		"Writable: title, body, author",
		"Body: note (text)",
		"Writes: status",
		"Touches: subscriptions",
		"Params: as_of (timestamptz)",
		"Reads: authors",
		// Keyed by resource and operation, not method and path.
		"<!-- sqlb:notes posts list -->",
		// The structured template: fixed sub-headings, not one open prompt.
		"**Source:**",
		"**Request:**",
		"**Response:**",
		"**Invariants:**",
		"**Errors:**",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FEATURES.md missing %q\n\n%s", want, got)
		}
	}
	// "Writable: title, body, author" (already asserted above) already proves
	// Immutable status is excluded: were it present the line would read
	// "title, body, status, author" and that substring match would fail.
}

func TestDocsPreservesNotesWrittenIntoTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "FEATURES.md")
	p := docsProject(docsFixture("/posts", schema.CRUD|schema.OpList), file)

	if code, out := run(t, p, "docs"); code != 0 {
		t.Fatalf("first run should succeed, got %d (%s)", code, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("FEATURES.md not written: %v", err)
	}
	filled := fillNote(t, string(raw), "posts read", "Fetches one post, expanding its author by default.")
	if err := os.WriteFile(file, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, out := run(t, p, "docs"); code != 0 {
		t.Fatalf("second run should succeed, got %d (%s)", code, out)
	}
	raw, err = os.ReadFile(file)
	if err != nil {
		t.Fatalf("FEATURES.md not written: %v", err)
	}
	if !strings.Contains(string(raw), "Fetches one post, expanding its author by default.") {
		t.Errorf("a rerun should keep the hand-written note, got:\n%s", raw)
	}
}

func TestDocsArchivesNotesForARemovedEndpoint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "FEATURES.md")
	full := docsProject(docsFixture("/posts", schema.CRUD|schema.OpList), file)

	if code, out := run(t, full, "docs"); code != 0 {
		t.Fatalf("first run should succeed, got %d (%s)", code, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	filled := fillNote(t, string(raw), "posts delete", "Soft-deletes; the row is kept for 30 days.")
	if err := os.WriteFile(file, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rerun against a schema that no longer exposes delete.
	narrowed := docsProject(docsFixture("/posts", schema.OpCreate|schema.OpRead|schema.OpUpdate|schema.OpList), file)
	code, out := run(t, narrowed, "docs")
	if code != 0 {
		t.Fatalf("second run should succeed, got %d (%s)", code, out)
	}
	if !strings.Contains(out, "1 note(s) archived") {
		t.Errorf("should report the archived note, got: %s", out)
	}

	raw, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "## Archived notes") {
		t.Errorf("removed endpoint should land under an Archived notes section, got:\n%s", got)
	}
	if !strings.Contains(got, "Soft-deletes; the row is kept for 30 days.") {
		t.Errorf("the archived note's content should be kept verbatim, got:\n%s", got)
	}
	if strings.Contains(got, "`DELETE /posts/{id}` — delete") {
		t.Errorf("the endpoint is no longer exposed and should not appear as a live section, got:\n%s", got)
	}
}

// TestDocsRenamingAResourcePathKeepsItsNotes is the fix for the review's
// point 4: a resource's path is not part of a note's key, so renaming it
// looks like nothing happened to the notes rather than a delete-and-create.
func TestDocsRenamingAResourcePathKeepsItsNotes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "FEATURES.md")
	before := docsProject(docsFixture("/posts", schema.OpList|schema.OpRead), file)

	if code, out := run(t, before, "docs"); code != 0 {
		t.Fatalf("first run should succeed, got %d (%s)", code, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	filled := fillNote(t, string(raw), "posts list", "Paginated, newest first, org-scoped.")
	if err := os.WriteFile(file, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same table, same ops, a different REST path — the kind of edit a
	// resource rename actually is.
	after := docsProject(docsFixture("/articles", schema.OpList|schema.OpRead), file)
	code, out := run(t, after, "docs")
	if code != 0 {
		t.Fatalf("second run should succeed, got %d (%s)", code, out)
	}
	if strings.Contains(out, "archived") {
		t.Errorf("a path rename should not archive anything, got: %s", out)
	}

	raw, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "Paginated, newest first, org-scoped.") {
		t.Errorf("the note should have carried over under its unchanged key, got:\n%s", got)
	}
	if strings.Contains(got, "## Archived notes") {
		t.Errorf("nothing should be archived on a path rename, got:\n%s", got)
	}
	if !strings.Contains(got, "`GET /articles`") {
		t.Errorf("the live endpoint should reflect the new path, got:\n%s", got)
	}
}

// TestDocsIncludesHandwrittenEndpoints is the fix for #211: a route mounted
// by hand alongside the generated resources gets a section too, with no
// schema declaration to fall back on for a description.
func TestDocsIncludesHandwrittenEndpoints(t *testing.T) {
	file := filepath.Join(t.TempDir(), "FEATURES.md")
	p := docsProject(docsFixture("/posts", schema.OpList|schema.OpRead), file,
		codegen.HandwrittenOp{
			Method:      "POST",
			Path:        "/auth/login",
			Summary:     "Log in",
			Description: "Exchanges credentials for a JWT.",
		})

	code, out := run(t, p, "docs")
	if code != 0 {
		t.Fatalf("docs should succeed, got %d (%s)", code, out)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"## Hand-written endpoints",
		"`POST /auth/login` — Log in",
		"Exchanges credentials for a JWT.",
		"<!-- sqlb:notes POST /auth/login -->",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FEATURES.md missing %q\n\n%s", want, got)
		}
	}
}
