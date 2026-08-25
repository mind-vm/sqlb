package sqlb_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// Invoice carries no tags at all: it stands in for a struct that came from
// another generator, or from a package the caller does not own.
type Invoice struct {
	ID           string
	CustomerID   string
	AmountDue    int64
	Paid         bool
	InternalMemo string
	CreatedAt    time.Time
}

func init() {
	sqlb.Describe[Invoice]().
		PrimaryKey("id").
		Defaulted("id").
		Timestamps("created_at").
		Filterable("customer_id", "paid", "amount_due").
		Sortable("amount_due").
		Hidden("internal_memo")
}

// TestUntaggedStructWorksForQueries is the baseline: the builder needs no
// metadata beyond the field names.
func TestUntaggedStructWorksForQueries(t *testing.T) {
	m := sqlb.ModelOf[Invoice]()
	if m.Table != "invoices" {
		t.Errorf("table = %q, want %q", m.Table, "invoices")
	}
	want := []string{"id", "customer_id", "amount_due", "paid", "internal_memo", "created_at"}
	got := m.ColumnNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", got, want)
	}
}

// TestDescribeSuppliesTheInsertDefaults covers the failure this fixes: without
// metadata an insert writes "" into the key column and a zero timestamp over
// the database default.
func TestDescribeSuppliesTheInsertDefaults(t *testing.T) {
	inv := &Invoice{CustomerID: "c1", AmountDue: 500}
	sql, args, err := sqlb.InsertRows(inv).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if strings.Contains(sql, `"id"`) && strings.Contains(sql, `INSERT INTO "invoices" ("id"`) {
		t.Errorf("a defaulted zero-valued key must be left to the database, got: %s", sql)
	}
	if strings.Contains(sql, `"created_at"`) && strings.Contains(sql, `, "created_at")`) {
		t.Errorf("a defaulted zero timestamp must be left to the database, got: %s", sql)
	}
	if len(args) != 4 {
		t.Errorf("bound %d args, want 4 (customer_id, amount_due, paid, internal_memo)", len(args))
	}
}

// TestDescribeEnablesTheRESTLayer is the other half: an undescribed model
// exposes nothing, and a described one exposes exactly what was named.
func TestDescribeEnablesTheRESTLayer(t *testing.T) {
	opts := filter.Options{Model: sqlb.ModelOf[Invoice]()}

	q, err := filter.Parse(url.Values{
		"paid":       {"eq.false"},
		"amount_due": {"gte.100"},
		"sort":       {"-amount_due"},
	}, opts)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Invoice](), q).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	for _, want := range []string{`"amount_due" >= $1`, `"paid" = $2`, `ORDER BY "amount_due" DESC`} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %s\ngot: %s", want, sql)
		}
	}
}

func TestDescribeLeavesUndeclaredColumnsClosed(t *testing.T) {
	opts := filter.Options{Model: sqlb.ModelOf[Invoice]()}

	// created_at was made sortable by Timestamps but never filterable.
	if _, err := filter.Parse(url.Values{"created_at": {"gte.2024-01-01"}}, opts); err == nil {
		t.Error("a column that was not made filterable must stay closed")
	}
	// internal_memo is hidden, so it is indistinguishable from absent.
	_, err := filter.Parse(url.Values{"internal_memo": {"eq.x"}}, opts)
	if err == nil {
		t.Fatal("a hidden column must not be filterable")
	}
	if strings.Contains(err.Error(), "not filterable") {
		t.Errorf("the rejection confirms the hidden column exists: %v", err)
	}
	for _, col := range sqlb.ModelOf[Invoice]().Selectable() {
		if col.Name == "internal_memo" {
			t.Error("a hidden column must not be in the REST projection")
		}
	}
}

func TestDescribePrimaryKeyIsUsable(t *testing.T) {
	m := sqlb.ModelOf[Invoice]()
	if m.PK == nil || m.PK.Name != "id" {
		t.Fatalf("primary key = %v, want id", m.PK)
	}
	if !m.PK.ReadOnly || !m.PK.Filterable {
		t.Error("a primary key should be read-only and filterable")
	}
}

// A mistyped column would otherwise leave a capability quietly off and surface
// as a confusing 400 much later, so it fails at startup instead.
func TestDescribeRejectsUnknownColumns(t *testing.T) {
	type Widget struct {
		ID   string
		Name string
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("naming a column that does not exist should panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "does_not_exist") {
			t.Errorf("panic should quote the bad name: %v", r)
		}
		if !strings.Contains(msg, "id, name") {
			t.Errorf("panic should list the real columns: %v", r)
		}
	}()
	sqlb.Describe[Widget]().Filterable("does_not_exist")
}

func TestDescribeColumnRename(t *testing.T) {
	type Legacy struct {
		ID       string
		FullName string
	}
	sqlb.Describe[Legacy]().Table("legacy_people").Column("FullName", "nom_complet")

	sql, _, err := sqlb.Query[Legacy]().Where(sqlb.F("nom_complet").Eq("Ada")).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	want := `SELECT "legacy_people"."id", "legacy_people"."nom_complet" FROM "legacy_people" WHERE "nom_complet" = $1`
	if sql != want {
		t.Errorf("SQL\n got: %s\nwant: %s", sql, want)
	}
}

func TestDescribeRejectsCollidingRename(t *testing.T) {
	type Clash struct {
		ID   string
		Name string
		Alt  string
	}
	defer func() {
		if recover() == nil {
			t.Error("renaming onto an occupied column should panic")
		}
	}()
	sqlb.Describe[Clash]().Column("Alt", "name")
}

// Tags and descriptions have to compose, so a partly tagged struct can be
// completed without editing it.
func TestDescribeMergesOntoTags(t *testing.T) {
	type Mixed struct {
		ID    string `db:"id" sqlb:"pk,default"`
		Title string `db:"title" sqlb:"filter"`
		Notes string `db:"notes"`
	}
	sqlb.Describe[Mixed]().Sortable("title").Filterable("notes")

	m := sqlb.ModelOf[Mixed]()
	title := m.Column("title")
	if !title.Filterable {
		t.Error("the tag's filter capability was lost")
	}
	if !title.Sortable {
		t.Error("the description's sort capability was not applied")
	}
	if !m.Column("notes").Filterable {
		t.Error("a described capability on an untagged column was not applied")
	}
	if m.PK == nil {
		t.Error("the tag's primary key was lost")
	}
}

// Relation is the no-codegen path for ?expand. Codegen is optional
// (ADR-0010), so a feature reachable only from generated tags would be a
// feature half the intended users cannot have.

// descList and descTask carry no sqlb tags at all — the relation is declared
// entirely at runtime, the way a caller layering over someone else's structs
// has to.
type descList struct {
	ID     string
	Name   string
	Secret string
}

func (descList) TableName() string { return "lists" }

type descTask struct {
	ID     string
	ListID string
	Title  string

	List *descList `db:"-"`
}

func (descTask) TableName() string { return "tasks" }

func init() {
	sqlb.Describe[descList]().PrimaryKey("id").Hidden("secret")
	sqlb.Describe[descTask]().PrimaryKey("id").Relation("List", "list_id")
}

func TestDescribeRelationCompilesTheSameJoinAsTheTags(t *testing.T) {
	sql, _, err := sqlb.Query[descTask]().Expand("list").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		`LEFT JOIN "lists" AS "__ex_list" ON "__ex_list"."id" = "tasks"."list_id"`,
		`json_build_object('id', "__ex_list"."id", 'name', "__ex_list"."name")`,
		`AS "__expand_list"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}
	// Hidden on the target was declared through Describe too, and has to hold
	// across the join for the same reason it does with tags: expanding must not
	// become a way to read a column the target refuses to serve.
	if strings.Contains(sql, "secret") {
		t.Errorf("a hidden column of the expanded target reached the statement:\n%s", sql)
	}
}

// Declaring the relation is what makes the key expandable. There is no second
// place to say it, so there is nothing to disagree with.
func TestDescribeRelationMakesTheKeyExpandable(t *testing.T) {
	col := sqlb.ModelOf[descTask]().Column("list_id")
	if col == nil || !col.Expandable {
		t.Fatalf("list_id should carry the expand capability: %+v", col)
	}
	if names := sqlb.ModelOf[descTask]().RelationNames(); len(names) != 1 || names[0] != "list" {
		t.Errorf("relations = %v, want [list]", names)
	}
}

// A described relation has to reach the request path, not just the builder:
// ?expand=list parsed from a URL and applied produces the same join. This is
// the whole point of the no-codegen path — the REST layer is what most callers
// want the relation for.
func TestDescribeRelationIsReachableFromAQueryString(t *testing.T) {
	q, err := filter.Parse(url.Values{"expand": {"list"}}, filter.Options{
		Model:      sqlb.ModelOf[descTask](),
		Expandable: []string{"list"},
	})
	if err != nil {
		t.Fatalf("parsing ?expand=list against a described relation: %v", err)
	}

	sql, _, err := filter.Apply(sqlb.Query[descTask](), q).SQL()
	if err != nil {
		t.Fatalf("applying the parsed query: %v", err)
	}
	if !strings.Contains(sql, `LEFT JOIN "lists" AS "__ex_list"`) {
		t.Errorf("?expand=list did not reach the join:\n%s", sql)
	}
	// Hidden survives the whole path, not only a hand-built query.
	if strings.Contains(sql, "secret") {
		t.Errorf("a hidden column of the target reached the request path:\n%s", sql)
	}
}

func TestDescribeRelationRejectsAFieldThatIsAColumn(t *testing.T) {
	type Bad struct {
		ID     string
		ListID string
		List   string // a column, not a relation
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a mapped column cannot also hold an expanded row")
		}
		if msg, _ := r.(string); !strings.Contains(msg, `mapped to column "list"`) {
			t.Errorf("panic should say the field is already a column: %v", r)
		}
	}()
	sqlb.Describe[Bad]().Relation("List", "list_id")
}

func TestDescribeRelationRejectsAnUnknownForeignKey(t *testing.T) {
	type Bad struct {
		ID   string
		List *descList `db:"-"`
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expanding on a column that does not exist should panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "list_id") {
			t.Errorf("panic should quote the missing column: %v", r)
		}
	}()
	sqlb.Describe[Bad]().Relation("List", "list_id")
}

func TestDescribeRelationRejectsANonStructField(t *testing.T) {
	type Bad struct {
		ID     string
		ListID string
		Extra  []byte `db:"-"`
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a relation field has to be a struct or a pointer to one")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "want a struct") {
			t.Errorf("panic should say what the field should have been: %v", r)
		}
	}()
	sqlb.Describe[Bad]().Relation("Extra", "list_id")
}

// A described model's wire index has to survive the copy a Description
// publishes, and a rename has to move it.
//
// Both were missed when WireCase landed: the copy predates byWire and rebuilt
// only byName, so every described model resolved filters against an empty
// index — and the failure was a filter answering "unknown parameter" while the
// allowed list beside it named that very parameter.
func TestDescribedModelResolvesByWire(t *testing.T) {
	type Invoice struct {
		ID        string `db:"id"`
		AmountDue int64  `db:"amount_due"`
		Legacy    string
	}
	m := sqlb.Describe[Invoice]().
		PrimaryKey("id").
		Filterable("amount_due").
		Column("Legacy", "renamed_col").
		Filterable("renamed_col").
		Model()

	if col := m.ColumnByWire("amount_due"); col == nil {
		t.Fatal("a described column is not reachable by its wire name, so no filter can name it")
	}
	// The rename moved both indexes, and the name it replaced is gone from
	// each — otherwise two spellings answer, which is what ADR-0036 forbids.
	if col := m.ColumnByWire("renamed_col"); col == nil || col.Name != "renamed_col" {
		t.Errorf("the rename did not move the wire index: %+v", col)
	}
	if m.ColumnByWire("legacy") != nil {
		t.Error("the pre-rename spelling still resolves")
	}
}
