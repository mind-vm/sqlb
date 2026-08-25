package pgtest

import (
	"context"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/schema"
)

// A date column across an expansion, against a real Postgres.
//
// This is the one test that could have caught #84, and the reason it is here
// rather than in the engine's own suite is that the engine's suite compares the
// compiled statement against a string somebody wrote. The bug was not in the
// statement. `json_build_object('due_on', "__ex_project"."due_on")` is a
// perfectly good query — it just serialises a date as "2026-07-01", which the
// time.Time on the other end will not decode. Only a database produces that
// value, so only a database can fail on it.
//
// What is asserted is a round trip: write a date, expand a relation over it,
// and get the same day back. Both halves matter — the decode is what used to
// 500, and the day is what a ::timestamptz cast would have silently shifted.

// The json tags are load-bearing rather than decoration: an expanded row is
// built by Postgres as a JSON object keyed by *column* name, and scanExpansion
// unmarshals it into this struct. Without a tag, encoding/json matches
// case-insensitively on the field name — "DueOn" against "due_on" — and does
// not ignore the underscore, so the field would silently stay nil and this test
// would report the bug it exists to catch.
type DateProject struct {
	ID    int64      `db:"id" json:"id" sqlb:"type:bigint,pk,default"`
	Name  string     `db:"name" json:"name" sqlb:"type:text"`
	DueOn *time.Time `db:"due_on" json:"due_on" sqlb:"type:date"`
}

func (DateProject) TableName() string { return "dateprojects" }

type DateEntry struct {
	ID        int64        `db:"id" json:"id" sqlb:"type:bigint,pk,default"`
	ProjectID int64        `db:"project_id" json:"project_id" sqlb:"type:bigint,filter,expand"`
	Project   *DateProject `db:"-" json:"project,omitempty" sqlb:"expands=project_id"`
}

func (DateEntry) TableName() string { return "dateentries" }

func dateRegistry() *schema.Registry {
	r := schema.NewRegistry()
	projects := r.Table("dateprojects",
		schema.BigInt("id").PrimaryKey().Default(schema.Expr("nextval('dateprojects_id_seq')")),
		schema.Text("name"),
		schema.Date("due_on").Nullable(),
	)
	r.Table("dateentries",
		schema.BigInt("id").PrimaryKey().Default(schema.Expr("nextval('dateentries_id_seq')")),
		schema.Ref("project", projects).Filterable().Expandable(),
	)
	return r
}

func TestExpandingARelationWithADateColumnRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	mustExec(t, raw, `CREATE SEQUENCE dateprojects_id_seq`)
	mustExec(t, raw, `CREATE SEQUENCE dateentries_id_seq`)
	applySchema(t, raw, dateRegistry())
	db := sqlb.New(raw)

	due := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var projectID int64
	if err := raw.QueryRow(ctx,
		`INSERT INTO dateprojects (name, due_on) VALUES ('Atlas', $1) RETURNING id`, due,
	).Scan(&projectID); err != nil {
		t.Fatalf("inserting a project: %v", err)
	}
	if _, err := raw.Exec(ctx,
		`INSERT INTO dateentries (project_id) VALUES ($1)`, projectID,
	); err != nil {
		t.Fatalf("inserting an entry: %v", err)
	}

	// Before the fix this returned:
	//   sqlb: decoding expanded "project": parsing time "2026-07-01" as
	//   "2006-01-02T15:04:05Z07:00": cannot parse "" as "T"
	entries, err := sqlb.Query[DateEntry]().Expand("project").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding a relation whose target has a date column: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0].Project
	if got == nil {
		t.Fatal("the expansion produced no project")
	}
	if got.DueOn == nil {
		t.Fatal("the date came back null")
	}
	// The day, not just the decode. `::timestamptz` would have resolved through
	// the session's TimeZone and moved this to 2026-06-30 under any zone east
	// of UTC — a fix that passes a decode test and loses a day.
	if y, m, d := got.DueOn.UTC().Date(); y != 2026 || m != time.July || d != 1 {
		t.Errorf("due_on came back as %s, want 2026-07-01", got.DueOn.UTC().Format(time.RFC3339))
	}
}

// The same column read directly, so the two paths can be compared rather than
// each being checked against the test author's expectation. The point of the
// fix is that they agree; that is worth asserting rather than assuming.
func TestADateReadsTheSameDirectlyAndThroughAnExpansion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	mustExec(t, raw, `CREATE SEQUENCE dateprojects_id_seq`)
	mustExec(t, raw, `CREATE SEQUENCE dateentries_id_seq`)
	applySchema(t, raw, dateRegistry())
	db := sqlb.New(raw)

	due := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var projectID int64
	if err := raw.QueryRow(ctx,
		`INSERT INTO dateprojects (name, due_on) VALUES ('Atlas', $1) RETURNING id`, due,
	).Scan(&projectID); err != nil {
		t.Fatalf("inserting a project: %v", err)
	}
	if _, err := raw.Exec(ctx,
		`INSERT INTO dateentries (project_id) VALUES ($1)`, projectID,
	); err != nil {
		t.Fatalf("inserting an entry: %v", err)
	}

	direct, err := sqlb.Query[DateProject]().Where(sqlb.F("id").Eq(projectID)).One(ctx, db)
	if err != nil {
		t.Fatalf("reading the project directly: %v", err)
	}
	entries, err := sqlb.Query[DateEntry]().Expand("project").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding: %v", err)
	}
	if len(entries) != 1 || entries[0].Project == nil {
		t.Fatal("the expansion produced no project")
	}

	if direct.DueOn == nil || entries[0].Project.DueOn == nil {
		t.Fatal("one of the two paths returned a null date")
	}
	if !direct.DueOn.Equal(*entries[0].Project.DueOn) {
		t.Errorf("the same column reads as %s directly and %s through an expansion",
			direct.DueOn.UTC().Format(time.RFC3339),
			entries[0].Project.DueOn.UTC().Format(time.RFC3339))
	}
}
