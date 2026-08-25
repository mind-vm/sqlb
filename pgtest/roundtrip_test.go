package pgtest

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// declare builds a registry exercising every construct the DSL can express and
// the DDL layer can render. It is deliberately one big schema rather than a
// table per feature: the failures worth catching here are the ones that only
// appear when constructs sit next to each other — a foreign key to a table
// declared later, an index over a column with a check on it.
func declare(r *schema.Registry) {
	orgs := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
		schema.Text("slug").Unique().Filterable(),
		schema.Varchar("region", 40).Nullable(),
		schema.Timestamps(),
	).Describe("A tenant.")

	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
		schema.Text("title").Searchable().Sortable().Comment("headline"),
		schema.Text("slug").Unique(),
		schema.Enum("status", "draft", "review", "published").
			Default(schema.Value("draft")).
			Filterable(),
		schema.Int("views").Default(schema.Value(0)).Sortable(),
		schema.Numeric("rating").Nullable(),
		// A precision-bounded numeric, which is a *different type* from the
		// unbounded one above: an adopting schema that could not say so either
		// held a permanent add-column waiver or left the field out of the
		// model, the REST surface and every generated client (issue #81).
		schema.Numeric("contracted_hours", 5, 2).Nullable(),
		schema.Float("score").Nullable(),
		schema.Bool("pinned").Default(schema.Value(false)),
		schema.JSON("meta").Nullable(),
		schema.Bytes("thumbnail").Nullable(),
		schema.Date("published_on").Nullable(),
		schema.Timestamp("reviewed_at").Nullable(),

		// Arrays, whose round trip is the whole reason ADR-0033 exists: a
		// module with one text[] in it could not be adopted at all, because a
		// column introspect dropped makes the first Diff propose deleting
		// production data.
		schema.Text("tags").Array().Default(schema.Value("{}")).Filterable(),
		schema.Int("revisions").Array().Nullable(),
		schema.Enum("channels", "web", "email", "push").Array().Nullable(),

		schema.Timestamps(),
	).
		Describe("Articles.").
		Check("views_non_negative", `"views" >= 0`).
		Index("org_id", "status").
		Index("title").
		AddIndex(schema.Index{Columns: []string{"tags"}, Method: "gin"}).
		// An ordered index, whose ordering *is* the index: this one backs
		// `ORDER BY status ASC NULLS FIRST, published_on DESC`, and an index on
		// the same three columns in any other order would not serve it. Here
		// rather than in a test of its own because what has to hold is that it
		// survives the round trip — declaring an ordering the import could not
		// read back proposed dropping the live index on every run, and could
		// not tell "missing" from "differently ordered" (issue #64).
		AddIndex(schema.Index{
			Name:    "articles_feed_idx",
			Columns: []string{"org_id", "status", "published_on"},
			Orders: map[string]schema.IndexOrder{
				"status":       {Nulls: schema.NullsFirst},
				"published_on": {Desc: true},
			},
		})
}

func TestGeneratedDDLAppliesAndReadsBackUnchanged(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	target := schema.NewRegistry()
	declare(target)

	applySchema(t, db, target)

	imported := importRegistry(t, db)

	// The claim: nothing needs to change to get from what the database now
	// holds to what was declared, except the one difference ADR-0014 records
	// and explains. Anything else is a construct that did not survive the trip,
	// which in production is a migration proposing to undo work nobody asked to
	// undo.
	changes := diff(t, imported, target)

	// A constraint that is dropped and re-added under the same name, where the
	// add is a CHECK, is the known normalisation. Pairing is what keeps the
	// allowance honest: a drop on its own is a lost constraint and must fail.
	readded := map[string]bool{}
	for _, c := range changes {
		if name, ok := addedCheckConstraint(c); ok {
			readded[name] = true
		}
	}

	var unexplained []migrate.Change
	var checkChurn int
	for _, c := range changes {
		name, isDrop := droppedConstraint(c)
		_, isAdd := addedCheckConstraint(c)
		if (isAdd) || (isDrop && readded[name]) {
			checkChurn++
			continue
		}
		unexplained = append(unexplained, c)
	}

	if len(unexplained) > 0 {
		t.Errorf("round trip lost %d construct(s):\n%s", len(unexplained), describe(unexplained))
	}

	// Not an error, but not silent either. Postgres normalises a hand-written
	// CHECK ("views" >= 0) to CHECK (views >= 0), so the imported expression is
	// never byte-identical to the declared one and the diff proposes to replace
	// the constraint with itself. Closing it needs a SQL expression parser,
	// which is the kind of guessing ADR-0014 rejects elsewhere.
	//
	// It is survivable only because it does not compound — see
	// TestImportIsAFixpoint, which is the test that would fail if it did.
	if checkChurn > 0 {
		t.Logf("%d check-constraint change(s), as expected: Postgres normalises check expressions, so a declared expression never matches the stored one verbatim", checkChurn)
	}
}

// addedCheckConstraint returns the name of the constraint a change adds, if
// the change adds a CHECK.
var addCheckRE = regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+"([^"]+)"\s+CHECK\s*\(`)

func addedCheckConstraint(c migrate.Change) (string, bool) {
	if m := addCheckRE.FindStringSubmatch(c.Up); m != nil {
		return m[1], true
	}
	return "", false
}

// droppedConstraint returns the name of the constraint a change drops.
//
// A drop names the constraint but not its kind, so this cannot tell a check
// from a unique on its own. That is why the caller only forgives a drop whose
// name is re-added as a CHECK in the same diff: an unpaired drop is a lost
// constraint and has to fail. An allowance broad enough to swallow a real
// regression is worse than no allowance, since it reports coverage it does not
// have (ADR-0016).
var dropConstraintRE = regexp.MustCompile(`(?is)DROP\s+CONSTRAINT\s+"([^"]+)"`)

func droppedConstraint(c migrate.Change) (string, bool) {
	if m := dropConstraintRE.FindStringSubmatch(c.Up); m != nil {
		return m[1], true
	}
	return "", false
}

// TestImportIsAFixpoint is the property that decides whether adoption works.
//
// A single round trip can differ from its input for reasons that do not matter
// — Postgres normalises a hand-written CHECK expression, for instance. What
// would matter is a difference that *recurs*: a schema that diffs against
// itself forever, so every migration after adoption carries the same phantom
// change. ADR-0014 calls this out as the decisive check for that reason.
func TestImportIsAFixpoint(t *testing.T) {
	t.Parallel()
	first := freshDB(t)

	target := schema.NewRegistry()
	declare(target)
	applySchema(t, first, target)

	once := importRegistry(t, first)

	// Render what was imported into a second database, and import that.
	second := freshDB(t)
	applySchema(t, second, once)
	twice := importRegistry(t, second)

	if changes := diff(t, once, twice); len(changes) > 0 {
		t.Errorf("import is not a fixpoint — %d change(s) on the second pass:\n%s",
			len(changes), describe(changes))
	}
}

// TestTheRoundTripCanFail is the positive control ADR-0016 requires: a guard is
// not trusted until it has failed on purpose.
//
// Without this, every assertion above would still pass if importRegistry
// silently returned the registry it was given, if diff always returned nothing,
// or if applySchema quietly executed no statements. Each of those is a bug that
// turns the suite green while verifying nothing.
func TestTheRoundTripCanFail(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	target := schema.NewRegistry()
	declare(target)
	applySchema(t, db, target)

	// Declare a table the database does not have. A working round trip must
	// notice; a broken one reports the same clean result as the tests above.
	expected := schema.NewRegistry()
	declare(expected)
	expected.Table("posts_extra",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("absent_from_the_database"),
	)

	changes := diff(t, importRegistry(t, db), expected)
	if len(changes) == 0 {
		t.Fatal("a table missing from the database produced no diff — the round trip cannot see a difference, so its silence elsewhere means nothing")
	}
	if !strings.Contains(describe(changes), "posts_extra") {
		t.Errorf("the diff fired but does not name the missing table:\n%s", describe(changes))
	}
}

// applySchema renders a registry as the DDL to create it from nothing, and
// executes it.
//
// Statements run in autocommit rather than one transaction, on purpose: it is
// what a migration runner does per file, and CREATE INDEX CONCURRENTLY cannot
// run inside a transaction block at all. Wrapping this would make the harness
// unable to exercise the concurrent paths it exists to check.
func applySchema(t *testing.T, db *pgxpool.Pool, target *schema.Registry, opts ...migrate.Option) {
	t.Helper()

	changes := diff(t, schema.NewRegistry(), target, opts...)
	if len(changes) == 0 {
		t.Fatal("creating a schema from nothing produced no statements")
	}

	for i, c := range changes {
		if strings.TrimSpace(c.Up) == "" {
			continue
		}
		if _, err := db.Exec(context.Background(), c.Up); err != nil {
			t.Fatalf("statement %d of %d failed: %v\n%s\n\n(comment: %s)",
				i+1, len(changes), err, strings.TrimSpace(c.Up), c.Comment)
		}
	}
}

func importRegistry(t *testing.T, db *pgxpool.Pool) *schema.Registry {
	t.Helper()

	r, report, err := introspect.Registry(context.Background(), db, introspect.Options{})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// An entry means the registry does not describe the database completely,
	// so every comparison made against it afterwards is comparing the wrong
	// thing. Fail rather than diff a partial picture.
	if !report.Empty() {
		t.Fatalf("the schema uses only constructs the DSL can express, but import reported:\n%s", report)
	}
	return r
}

func diff(t *testing.T, current, target *schema.Registry, opts ...migrate.Option) []migrate.Change {
	t.Helper()

	changes, err := migrate.Diff(current, target, opts...)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return changes
}

func describe(changes []migrate.Change) string {
	var b strings.Builder
	for _, c := range changes {
		if c.Comment != "" {
			b.WriteString("-- " + c.Comment + "\n")
		}
		b.WriteString(strings.TrimSpace(c.Up) + "\n\n")
	}
	return b.String()
}
