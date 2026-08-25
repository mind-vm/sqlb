package sqlb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// The models the expansion tests use. `expand` on the foreign key and
// `expands=` on the field beside it are the two halves of a relation; see
// relation.go for why it is split that way.

type expList struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	Name   string `db:"name" json:"name"`
	Secret string `db:"secret" json:"-" sqlb:"hidden"`

	// The reverse direction. The column named is a column of the child, and
	// the field's cardinality is what says so. That expTask expands back to
	// expList makes this pair a cycle, which is deliberate: relation targets
	// resolve lazily so that a cycle cannot recurse at model build.
	Tasks *sqlb.Collection[expTask] `db:"-" json:"tasks,omitempty" sqlb:"expands=list_id"`
	Notes *sqlb.Collection[expNote] `db:"-" json:"notes,omitempty" sqlb:"expands=list_id,order=-created_at,limit=2"`
}

type expNote struct {
	ID      string `db:"id" json:"id" sqlb:"pk"`
	ListID  string `db:"list_id" json:"list_id" sqlb:"filter"`
	Body    string `db:"body" json:"body"`
	Author  string `db:"author" json:"-" sqlb:"hidden"`
	Created string `db:"created_at" json:"created_at" sqlb:"sort"`
}

func (expNote) TableName() string { return "notes" }

func (expList) TableName() string { return "lists" }

type expTask struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	ListID string `db:"list_id" json:"list_id" sqlb:"filter,expand"`
	Title  string `db:"title" json:"title"`

	List *expList `db:"-" json:"list,omitempty" sqlb:"expands=list_id"`
}

func (expTask) TableName() string { return "tasks" }

func TestExpandCompilesAJoinAndAJSONColumn(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().Expand("list").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	for _, want := range []string{
		`LEFT JOIN "lists" AS "__ex_list" ON "__ex_list"."id" = "tasks"."list_id"`,
		`AS "__expand_list"`,
		`json_build_object('id', "__ex_list"."id", 'name', "__ex_list"."name")`,
		// A left join that matched nothing must produce NULL, not an object of
		// nulls — the two say different things.
		`CASE WHEN "__ex_list"."id" IS NULL THEN NULL`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}

	// The relation field is db:"-", so it must not reach the projection.
	if strings.Contains(sql, `"list"`) && !strings.Contains(sql, `"__ex_list"`) {
		t.Errorf("the relation field was projected as a column:\n%s", sql)
	}
}

// TestExpandOmitsHiddenColumnsOfTheTarget is the security-relevant one.
// `Hidden` has to survive a join, or expanding a relation becomes a way to read
// a column the target refuses to serve directly.
func TestExpandOmitsHiddenColumnsOfTheTarget(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().Expand("list").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "secret") {
		t.Errorf("a hidden column of the expanded target reached the statement:\n%s", sql)
	}
}

// A standalone pair rather than adding a writeonly field to expList/expTask:
// those two are scanned directly by several other tests through harnesses
// with a fixed column list, and a new mapped column would have to be added to
// every one of them to keep the mock in sync. WriteOnly needs its own guard
// proven regardless (#195), so it gets its own small fixture instead.
type expWOTarget struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	Name   string `db:"name" json:"name"`
	Answer string `db:"answer" json:"-" sqlb:"writeonly"`
}

func (expWOTarget) TableName() string { return "wo_targets" }

type expWOSource struct {
	ID     string       `db:"id" json:"id" sqlb:"pk"`
	TID    string       `db:"t_id" json:"t_id" sqlb:"filter,expand"`
	Target *expWOTarget `db:"-" json:"target,omitempty" sqlb:"expands=t_id"`
}

func (expWOSource) TableName() string { return "wo_sources" }

// The WriteOnly analogue of TestExpandOmitsHiddenColumnsOfTheTarget: a column
// settable only through create/update must not leak through an expansion
// either, exactly as a Hidden one must not (#195).
func TestExpandOmitsWriteOnlyColumnsOfTheTarget(t *testing.T) {
	sql, _, err := sqlb.Query[expWOSource]().Expand("target").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "answer") {
		t.Errorf("a write-only column of the expanded target reached the statement:\n%s", sql)
	}
}

// The WriteOnly analogue of the ExpandOnly refusal on a Hidden column.
func TestExpandOnlyRefusesAWriteOnlyColumn(t *testing.T) {
	_, _, err := sqlb.Query[expWOSource]().ExpandOnly("target", "answer").SQL()
	if err == nil {
		t.Fatal("ExpandOnly accepted a write-only column")
	}
	if !strings.Contains(err.Error(), "never serves") {
		t.Errorf("error does not say %q: %v", "never serves", err)
	}
}

func TestExpandScansIntoTheRelationField(t *testing.T) {
	h := newHarness(t,
		[]string{"id", "list_id", "title", "__expand_list"},
		[][]any{
			{"t1", "l1", "Ship it", []byte(`{"id":"l1","name":"Backlog"}`)},
			{"t2", "l2", "Later", nil},
		})
	defer h.close()

	tasks, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d rows, want 2", len(tasks))
	}

	if tasks[0].List == nil {
		t.Fatal("the expanded relation was not scanned")
	}
	if tasks[0].List.Name != "Backlog" || tasks[0].List.ID != "l1" {
		t.Errorf("expanded list = %+v", tasks[0].List)
	}
	// The ordinary columns still scan.
	if tasks[0].Title != "Ship it" || tasks[0].ListID != "l1" {
		t.Errorf("row = %+v", tasks[0])
	}

	// A NULL expansion is a nil field, not a zero-valued struct: "there is no
	// related row" and "there is one and it is empty" must stay distinguishable.
	if tasks[1].List != nil {
		t.Errorf("a null expansion produced %+v, want nil", tasks[1].List)
	}
}

func TestExpandRejectsAnUnknownRelation(t *testing.T) {
	_, _, err := sqlb.Query[expTask]().Expand("owner").SQL()
	if err == nil {
		t.Fatal("expanding an unknown relation was accepted")
	}
	// ADR-0011: a rejection names what would have worked.
	if !strings.Contains(err.Error(), "list") {
		t.Errorf("the rejection does not name the expandable relations: %v", err)
	}
}

func TestExpandIsIdempotent(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().Expand("list").Expand("list").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, "LEFT JOIN"); n != 1 {
		t.Errorf("expanding twice produced %d joins, want 1:\n%s", n, sql)
	}
}

// TestExpandIsNotAppliedUnlessAsked guards the default. Expansion costs a join,
// and a query that did not ask for one must not pay for it.
func TestExpandIsNotAppliedUnlessAsked(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "LEFT JOIN") || strings.Contains(sql, "__expand_") {
		t.Errorf("an unexpanded query joined anyway:\n%s", sql)
	}
}

// TestRelationRequiresTheColumnToDeclareIt catches the half-written
// declaration: a field claiming to expand a column that does not opt in.
func TestRelationRequiresTheColumnToDeclareIt(t *testing.T) {
	type bad struct {
		ID     string `db:"id" sqlb:"pk"`
		ListID string `db:"list_id"` // no `expand`
		List   *expList
	}
	_ = bad{}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a relation naming a non-expandable column was accepted")
		}
		if !strings.Contains(toString(r), "expand") {
			t.Errorf("panic does not explain the problem: %v", r)
		}
	}()

	type badTagged struct {
		ID     string   `db:"id" sqlb:"pk"`
		ListID string   `db:"list_id"`
		List   *expList `db:"-" sqlb:"expands=list_id"`
	}
	_ = sqlb.ModelOf[badTagged]()
}

func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestExpandQualifiesEveryColumnOfTheBaseTable is the regression for the one
// mistake here that the fake driver could not see.
//
// Both tables have an `id` and a `name`. Unqualified, `SELECT "id" … LEFT JOIN
// "lists"` is not a query that returns the wrong row — it is not a query at
// all: Postgres rejects it with `column reference "id" is ambiguous`
// (SQLSTATE 42702). The in-memory driver accepts any string, so this held green
// for a whole feature before a real database saw it.
//
// Every column a caller can name — the projection, a filter, a sort — is a
// column of T, so T's table is what an unqualified name resolves to.
func TestExpandQualifiesEveryColumnOfTheBaseTable(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().
		Expand("list").
		Select(sqlb.F("id"), sqlb.F("title")).
		Where(sqlb.F("title").Eq("x")).
		OrderBy(sqlb.F("id").Asc()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	for _, want := range []string{
		`SELECT "tasks"."id", "tasks"."title"`,
		`WHERE "tasks"."title" = $1`,
		`ORDER BY "tasks"."id" ASC`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}
}

// The qualification is bought by the join, not paid for by every query. With
// nothing joined, a predicate keeps the bare column name it was written with,
// which is most of what anyone ever reads in a log. (The default projection is
// qualified either way — it always was, which is exactly why the ambiguity
// surfaced only once ?select replaced it.)
func TestASingleTableQueryLeavesPredicatesUnqualified(t *testing.T) {
	sql, _, err := sqlb.Query[expTask]().Where(sqlb.F("title").Eq("x")).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `WHERE "title" = $1`) {
		t.Errorf("a query with nothing joined should not qualify its predicates:\n%s", sql)
	}
}

// Reverse expansion — a list and its tasks. ADR-0022.

func TestExpandCollectionCompilesASubqueryRatherThanAJoin(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().Expand("tasks").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	// The whole point: a join would multiply the base rows, so the page's row
	// count would depend on how many children each row has.
	if strings.Contains(sql, "LEFT JOIN") {
		t.Errorf("a collection was joined rather than subqueried:\n%s", sql)
	}

	for _, want := range []string{
		`AS "__expand_tasks"`,
		`FROM "tasks" AS "__ex_tasks"`,
		// Correlated on the base table's primary key, named explicitly: inside
		// the subquery a bare "id" would resolve to the child.
		`WHERE "__ex_tasks"."list_id" = "lists"."id"`,
		// One row past the cap, so count(*) can answer has_more...
		`LIMIT 51`,
		// ...and the extra row is filtered back out before it is returned.
		`FILTER (WHERE "__rows_tasks"."n" <= 50)`,
		`'has_more', count(*) > 50`,
		// An empty collection is [], not null: "no children" and "not asked
		// for" must stay distinguishable.
		`coalesce(json_agg`,
		`'[]'::json`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}
}

// With no declared order the primary key is the order, because a LIMIT over an
// unordered child table does not merely reshuffle the result — it decides which
// children the caller never sees, differently on each run. ADR-0027's argument,
// applied under a cap.
func TestExpandCollectionOrdersByThePrimaryKeyByDefault(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().Expand("tasks").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "__ex_tasks"."id") AS "n"`) {
		t.Errorf("the window is not ordered by the child's primary key:\n%s", sql)
	}
	if !strings.Contains(sql, `ORDER BY "__ex_tasks"."id" LIMIT 51`) {
		t.Errorf("the capped read is not ordered by the child's primary key:\n%s", sql)
	}
}

// A declared order carries the primary key as a tiebreaker, so the order is
// total even when the declared column is not unique.
func TestExpandCollectionOrderIsMadeTotalByThePrimaryKey(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().Expand("notes").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		`ORDER BY "__ex_notes"."created_at" DESC, "__ex_notes"."id" DESC LIMIT 3`,
		`FILTER (WHERE "__rows_notes"."n" <= 2)`,
		`'has_more', count(*) > 2`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}
}

// The security-relevant one, in the direction ADR-0025 did not cover: Hidden has
// to survive the reverse expansion too, or a collection becomes a way to read a
// column the child's own endpoint refuses to serve.
func TestExpandCollectionOmitsHiddenColumnsOfTheChild(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().Expand("notes").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "author") {
		t.Errorf("a hidden column of the expanded child reached the statement:\n%s", sql)
	}
}

// Two collections compose by addition rather than by multiplication, which is
// the property the join shape loses.
func TestExpandTwoCollectionsAreIndependentSubqueries(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().Expand("tasks", "notes").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, "row_number() OVER"); n != 2 {
		t.Errorf("two collections produced %d subqueries, want 2:\n%s", n, sql)
	}
	if strings.Contains(sql, "GROUP BY") {
		t.Errorf("a collection expansion should need no aggregation over the base row:\n%s", sql)
	}
}

func TestExpandCollectionScansTheEnvelope(t *testing.T) {
	h := newHarness(t,
		[]string{"id", "name", "secret", "__expand_tasks"},
		[][]any{
			{"l1", "Backlog", "", []byte(`{"items":[{"id":"t1","list_id":"l1","title":"Ship it"}],"has_more":true}`)},
			{"l2", "Done", "", []byte(`{"items":[],"has_more":false}`)},
		})
	defer h.close()

	lists, err := sqlb.Query[expList]().Expand("tasks").All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(lists) != 2 {
		t.Fatalf("got %d rows, want 2", len(lists))
	}
	if lists[0].Tasks == nil {
		t.Fatal("the expanded collection was not scanned")
	}
	if got := lists[0].Tasks.Len(); got != 1 {
		t.Fatalf("got %d children, want 1", got)
	}
	if lists[0].Tasks.Items[0].Title != "Ship it" {
		t.Errorf("child = %+v", lists[0].Tasks.Items[0])
	}
	// The half a bare slice could not carry.
	if !lists[0].Tasks.HasMore {
		t.Error("a truncated collection did not report HasMore")
	}
	if lists[1].Tasks == nil || lists[1].Tasks.Len() != 0 || lists[1].Tasks.HasMore {
		t.Errorf("an empty collection scanned as %+v", lists[1].Tasks)
	}
}

// A cycle — tasks expand their list, lists collect their tasks — must not
// recurse at model build. It cannot, because a relation's target resolves on
// first expansion rather than when the model is built.
func TestExpandCycleResolvesLazily(t *testing.T) {
	if _, _, err := sqlb.Query[expList]().Expand("tasks").SQL(); err != nil {
		t.Fatalf("expanding forwards through a cycle: %v", err)
	}
	if _, _, err := sqlb.Query[expTask]().Expand("list").SQL(); err != nil {
		t.Fatalf("expanding backwards through a cycle: %v", err)
	}
}

// The reverse relation requires nothing of the child's column, because that
// column's capabilities describe the child's own endpoint. What it does require
// is that the column exists, and the rejection names what does. ADR-0011.
func TestExpandCollectionRejectsAColumnTheChildDoesNotHave(t *testing.T) {
	type badList struct {
		ID    string                    `db:"id" sqlb:"pk"`
		Tasks *sqlb.Collection[expTask] `db:"-" json:"tasks" sqlb:"expands=owner_id"`
	}
	_, _, err := sqlb.Query[badList]().Expand("tasks").SQL()
	if err == nil {
		t.Fatal("a collection on a column the child does not have was accepted")
	}
	if !strings.Contains(err.Error(), "list_id") {
		t.Errorf("the rejection does not name the child's columns: %v", err)
	}
}

// Order and limit are a collection's vocabulary. On a forward relation they
// describe nothing, so they are refused rather than ignored.
func TestForwardRelationRefusesCollectionOptions(t *testing.T) {
	type badTask struct {
		ID     string   `db:"id" sqlb:"pk"`
		ListID string   `db:"list_id" sqlb:"expand"`
		List   *expList `db:"-" json:"list" sqlb:"expands=list_id,limit=5"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("a forward relation with a limit was accepted")
		}
	}()
	_ = sqlb.ModelOf[badTask]()
}

// A collection is not paid for unless it is asked for.
func TestExpandCollectionIsNotAppliedUnlessAsked(t *testing.T) {
	sql, _, err := sqlb.Query[expList]().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "__expand_") || strings.Contains(sql, "row_number") {
		t.Errorf("an unexpanded query subqueried anyway:\n%s", sql)
	}
}

// The claim ADR-0030 used to record as a gap, now pinned as the fix.
//
// A BeforeQuery hook on the *target* reaches an expansion of it, requalified
// onto the join alias. The parent's own hooks run too, against the base table —
// so the statement carries two scopes naming two tables, which is the property
// that makes the requalification load-bearing rather than cosmetic.
//
// Written as an assertion about the compiled SQL rather than about rows,
// because the point is exactly what reaches the join.
func TestExpandRunsTheTargetsQueryHooks(t *testing.T) {
	reg := sqlb.NewRegistry()
	parent := sqlb.On[expTask](reg)
	target := sqlb.On[expList](reg)

	parent.BeforeQuery(func(_ context.Context, q *sqlb.Builder[expTask]) error {
		q.Where(sqlb.F("id").Neq("hidden-task"))
		return nil
	})
	target.BeforeQuery(func(_ context.Context, q *sqlb.Builder[expList]) error {
		q.Where(sqlb.F("name").Neq("hidden-list"))
		return nil
	})

	h := newHarness(t, []string{"id", "list_id", "title", "__expand_list"}, nil)
	defer h.close()

	if _, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmt := h.lastQuery()

	// The subject's hook is applied to the base table.
	if !contains(stmt, `"tasks"."id" <> $`) {
		t.Errorf("the queried model's own hook should reach the statement:\n%s", stmt)
	}
	// The target's is applied to the join alias, not to the base table. A bare
	// "name" would resolve to tasks and silently filter the wrong table, which
	// is the failure requalification exists to prevent.
	if !contains(stmt, `"__ex_list"."name" <> $`) {
		t.Errorf("the expansion target's hook should reach the join, qualified:\n%s", stmt)
	}
	// And it is in the ON clause rather than the WHERE, or a parent whose list
	// is out of scope would vanish from the page instead of arriving with a
	// null expansion.
	on := stmt[strings.Index(stmt, "LEFT JOIN"):]
	where := strings.Index(on, "WHERE")
	if where >= 0 {
		on = on[:where]
	}
	if !contains(on, `"__ex_list"."name" <> $`) {
		t.Errorf("the target's scope belongs in the join condition:\n%s", stmt)
	}
}

// The reverse direction is a correlated subquery rather than a join, so the
// scope lands in its WHERE — and that placement is what makes has_more count
// only the children the caller may actually fetch.
func TestExpandCollectionRunsTheTargetsQueryHooks(t *testing.T) {
	reg := sqlb.NewRegistry()
	target := sqlb.On[expTask](reg)

	target.BeforeQuery(func(_ context.Context, q *sqlb.Builder[expTask]) error {
		q.Where(sqlb.F("title").Neq("secret"))
		return nil
	})

	h := newHarness(t, []string{"id", "name", "__expand_tasks"}, nil)
	defer h.close()

	if _, err := sqlb.Query[expList]().Expand("tasks").All(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmt := h.lastQuery()
	if !contains(stmt, `"__ex_tasks"."title" <> $`) {
		t.Errorf("the collection's scope should reach its subquery, qualified:\n%s", stmt)
	}
}

// A hook that cannot be requalified fails the query rather than being dropped.
// Dropping it would be the original leak arriving by a different route, and
// silently — which is worse than the leak, because nothing would say so.
func TestExpandRefusesAnUnqualifiableHook(t *testing.T) {
	reg := sqlb.NewRegistry()
	target := sqlb.On[expList](reg)

	target.BeforeQuery(func(_ context.Context, q *sqlb.Builder[expList]) error {
		q.Where(sqlb.RawPred("name <> ?", "hidden"))
		return nil
	})

	h := newHarness(t, []string{"id", "list_id", "title", "__expand_list"}, nil)
	defer h.close()

	_, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), h.handle(reg))
	if err == nil {
		t.Fatal("an unrequalifiable scope predicate was accepted; it would have " +
			"filtered the parent table instead of the target")
	}
	for _, want := range []string{
		"raw SQL", "requalified", "composite foreign key",
		// The third way out, and the one the first two do not reach. A scope
		// inherited from a parent row is a subquery, F() has no spelling for
		// one, and a reader sent to "write it with F()" searches for an API
		// that is not there and then denormalises the scope column onto the
		// child — the one column whose duplication is a leak (#158).
		"subquery", "through the parent",
	} {
		if !contains(err.Error(), want) {
			t.Errorf("the error should explain the way out (%q missing), got: %v", want, err)
		}
	}
}

// expOwner is an expansion target that declares a computed column, and expDoc
// is a parent that expands to it. They are their own pair rather than a field on
// expList because a computed column changes the target's projection, and every
// assertion above is written against the projection expList has.
type expOwner struct {
	ID        string `db:"id" json:"id" sqlb:"pk"`
	Name      string `db:"name" json:"name"`
	Quota     int32  `db:"quota" json:"quota"`
	Used      int32  `db:"used" json:"used"`
	OverQuota bool   `db:"over_quota" json:"over_quota" sqlb:"filter"`
}

func (expOwner) TableName() string { return "owners" }

func (expOwner) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{{Name: "over_quota", Expr: "used > quota"}}
}

type expDoc struct {
	ID      string `db:"id" json:"id" sqlb:"pk"`
	OwnerID string `db:"owner_id" json:"owner_id" sqlb:"filter,expand"`

	Owner *expOwner `db:"-" json:"owner,omitempty" sqlb:"expands=owner_id"`
}

func (expDoc) TableName() string { return "docs" }

// A hook predicate naming a computed column of the expansion target is refused,
// because there is nothing to qualify onto the alias: a computed column has no
// storage, so `"__ex_owner"."over_quota"` is a column the database does not
// have. It used to compile and fail at request time with a bare Postgres 42703
// naming a column the schema plainly declares — and only when the hooked model
// was expanded, so every direct-read test passed (#76).
func TestExpandRefusesAHookOnAComputedColumnOfTheTarget(t *testing.T) {
	reg := sqlb.NewRegistry()
	target := sqlb.On[expOwner](reg)

	target.BeforeQuery(func(_ context.Context, q *sqlb.Builder[expOwner]) error {
		q.Where(sqlb.F("over_quota").Eq(false))
		return nil
	})

	h := newHarness(t, []string{"id", "owner_id", "__expand_owner"}, nil)
	defer h.close()

	_, err := sqlb.Query[expDoc]().Expand("owner").All(context.Background(), h.handle(reg))
	if err == nil {
		t.Fatal("a hook predicate on a computed column was carried across the join; " +
			"it compiles to a column that does not exist and fails as a bare 42703")
	}
	for _, want := range []string{"over_quota", "computed", "expOwner"} {
		if !contains(err.Error(), want) {
			t.Errorf("the refusal should name the hook's column, the reason and the model, got: %v", err)
		}
	}

	// The refusal is about the expansion, not about the column: reading the
	// hooked model directly still applies the predicate, computed and all.
	direct := newHarness(t, []string{"id", "name", "quota", "used", "over_quota"}, nil)
	defer direct.close()
	if _, err := sqlb.Query[expOwner]().All(context.Background(), direct.handle(reg)); err != nil {
		t.Fatalf("a direct read of the hooked model should still work: %v", err)
	}
	if !contains(direct.lastQuery(), "used > quota") {
		t.Errorf("the hook's predicate should reach a direct read:\n%s", direct.lastQuery())
	}
}

// A query with no expansion, or one whose target registered nothing, is
// unchanged — the resolution costs one map lookup and adds no predicate.
func TestExpandWithoutTargetHooksIsUnchanged(t *testing.T) {
	h := newHarness(t, []string{"id", "list_id", "title", "__expand_list"}, nil)
	defer h.close()

	if _, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), h.db); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmt := h.lastQuery()
	if contains(stmt, " AND ") {
		t.Errorf("an unhooked expansion should carry no extra condition:\n%s", stmt)
	}
}

// A release reaches an expansion target's hooks, not only the subject's.
//
// This is the property that makes a scope name span models rather than types
// (ADR-0054): "a shopper sees the published catalog" is one rule over several
// tables, and an admin reading a draft product expects the draft variants under
// it. If the release stopped at the subject, an admin's ?expand would carry the
// storefront's rule on the join and quietly drop rows the admin exists to see.
func TestAReleaseReachesTheExpansionTargetsHooks(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[expTask](reg).Scope("storefront").BeforeQuery(func(_ context.Context, q *sqlb.Builder[expTask]) error {
		q.Where(sqlb.F("id").Neq("hidden-task"))
		return nil
	})
	sqlb.On[expList](reg).Scope("storefront").BeforeQuery(func(_ context.Context, q *sqlb.Builder[expList]) error {
		q.Where(sqlb.F("name").Neq("hidden-list"))
		return nil
	})

	h := newHarness(t, []string{"id", "list_id", "title", "__expand_list"}, nil)
	defer h.close()

	// Both directions, because an assertion that the join carries no predicate
	// cannot tell a released rule from one that never ran.
	if _, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("All: %v", err)
	}
	if stmt := h.lastQuery(); !contains(stmt, `"__ex_list"."name" <> $`) {
		t.Fatalf("the target's scope is absent before any release, so this proves nothing:\n%s", stmt)
	}

	admin := h.handle(reg).WithoutScope("storefront")
	if _, err := sqlb.Query[expTask]().Expand("list").All(context.Background(), admin); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmt := h.lastQuery()
	if contains(stmt, `"__ex_list"."name" <> $`) {
		t.Errorf("the release did not reach the expansion target's hook:\n%s", stmt)
	}
	if contains(stmt, `"tasks"."id" <> $`) {
		t.Errorf("the release did not reach the subject's own hook:\n%s", stmt)
	}
}
