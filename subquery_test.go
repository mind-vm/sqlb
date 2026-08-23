package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jryannel/sqlb"
)

// errNested stands in for a build error the caller put on a subquery.
var errNested = errors.New("sqlb-test: this subquery is broken")

// A query is a value (ADR-0002), and these tests are about the consequence that
// was missing: a value can be nested inside another one. The interesting half is
// not the SQL — it is that nesting a query does not run it, so a model confined
// by a hook could be read through someone else's WHERE clause with the
// confinement silently absent. That is refused, and both directions are proved.

type subPost struct {
	ID       string `db:"id" sqlb:"pk"`
	AuthorID string `db:"author_id" sqlb:"filter"`
	OrgID    string `db:"org_id" sqlb:"filter"`
	Title    string `db:"title" sqlb:"filter"`
}

func (subPost) TableName() string { return "sub_posts" }

func subHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"},
		[][]any{{"u1", "a@b.c", "Ada", nil, "org1", "", time.Unix(0, 0).UTC()}})
	t.Cleanup(h.close)
	return h
}

func TestInQueryNestsASelect(t *testing.T) {
	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id")).Where(sqlb.F("title").Eq("go"))
	q := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub))

	got, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email", "users"."name", "users"."age", "users"."org_id", ` +
		`"users"."password_hash", "users"."created_at" FROM "users" ` +
		`WHERE "id" IN (SELECT "sub_posts"."author_id" FROM "sub_posts" WHERE "sub_posts"."title" = $1)`
	if got != want {
		t.Errorf("SQL:\n got %s\nwant %s", got, want)
	}
	if len(args) != 1 || args[0] != "go" {
		t.Errorf("args = %v, want [go]", args)
	}
}

// The nested SELECT shares the outer statement's bind numbering. If it did not,
// two values would collide on $1 and the statement would silently read the
// wrong one.
func TestNestingContinuesTheBindNumbering(t *testing.T) {
	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id")).Where(sqlb.F("title").Eq("go"))
	q := sqlb.Query[User]().
		Where(sqlb.F("org_id").Eq("acme")).
		Where(sqlb.F("id").InQuery(sub)).
		Where(sqlb.F("name").Eq("Ada"))

	got, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{`= $1`, `= $2)`, `= $3`} {
		if !strings.Contains(got, want) {
			t.Errorf("SQL is missing %s: %s", want, got)
		}
	}
	if len(args) != 3 || args[0] != "acme" || args[1] != "go" || args[2] != "Ada" {
		t.Fatalf("args = %v, want [acme go Ada]", args)
	}
}

// A subquery's bare column names resolve to its own table even when the outer
// statement has set a base of its own. Without the qualification the inner
// WHERE would name the outer table and the subquery would become correlated —
// valid SQL answering a different question.
func TestANestedSelectQualifiesToItsOwnTable(t *testing.T) {
	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	q := sqlb.Query[User]().
		Join("orgs", "o", sqlb.F("org_id").EqField(sqlb.F("id").Qualify("o"))).
		Where(sqlb.F("id").InQuery(sub))

	got, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `SELECT "sub_posts"."author_id" FROM "sub_posts"`) {
		t.Errorf("the nested select did not qualify to its own table: %s", got)
	}
}

func TestExistsAndNotExists(t *testing.T) {
	sub := sqlb.Query[subPost]().Where(sqlb.F("title").Eq("go"))

	got, _, err := sqlb.Query[User]().Where(sqlb.Exists(sub)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `WHERE EXISTS (SELECT`) {
		t.Errorf("EXISTS did not render: %s", got)
	}

	got, _, err = sqlb.Query[User]().Where(sqlb.NotExists(sub)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `NOT (EXISTS (SELECT`) {
		t.Errorf("NOT EXISTS did not render: %s", got)
	}
}

// IN compares one column. A subquery projecting a whole model against it is
// refused here rather than by Postgres, whose complaint is about record types.
func TestInQueryRefusesTheWrongProjectionWidth(t *testing.T) {
	sub := sqlb.Query[subPost]()
	_, _, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).SQL()
	if err == nil {
		t.Fatal("a four-column subquery was accepted by IN")
	}
	for _, want := range []string{"4 columns", "sub_posts", "Select"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	// The other direction: one column is accepted, so the check is about the
	// width and not about subqueries in general.
	if _, _, err := sqlb.Query[User]().
		Where(sqlb.F("id").InQuery(sub.Clone().Select(sqlb.F("author_id")))).SQL(); err != nil {
		t.Errorf("a one-column subquery was refused: %v", err)
	}
}

// A subquery that failed to build fails the statement it was nested in, rather
// than compiling to something that runs.
func TestABrokenNestedQueryFailsTheOuterStatement(t *testing.T) {
	sub := sqlb.Query[subPost]().Select(sqlb.F("nope")).Where(sqlb.F("nope").Eq(1))
	sub.Fail(errNested)
	_, _, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).SQL()
	if err == nil || !strings.Contains(err.Error(), errNested.Error()) {
		t.Fatalf("the outer statement did not carry the nested error: %v", err)
	}
}

// A query nested inside itself is a cycle a caller can write, because a query is
// a value. It has to report a mistake rather than exhaust the stack.
func TestAQueryNestedInsideItselfIsRefused(t *testing.T) {
	q := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	q.Where(sqlb.F("author_id").InQuery(q))
	_, _, err := q.SQL()
	if err == nil {
		t.Fatal("a self-nesting query compiled")
	}
	if !strings.Contains(err.Error(), "nested inside itself") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// --- the scope guard --------------------------------------------------------

// The case the guard exists for. subPost's reads are confined to an org by a
// hook; nesting an unresolved query over it would read every org's rows and put
// the result inside a WHERE clause, where nothing about the response shows it.
func TestNestingAConfinedModelUnresolvedIsRefused(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)

	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	_, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).All(context.Background(), db)
	if err == nil {
		t.Fatal("a confined model was nested without its scope")
	}
	for _, want := range []string{"subPost", "Resolved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// The other direction, and the one that proves the guard is about the scope
// rather than about nesting: resolve the inner query first and it is accepted,
// with the hook's predicate in the nested SELECT.
func TestAResolvedNestedQueryCarriesItsScope(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)
	ctx := context.Background()

	sub, err := sqlb.Query[subPost]().Select(sqlb.F("author_id")).Resolved(ctx, db)
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if _, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).All(ctx, db); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := h.lastSelect(t)
	if !strings.Contains(got, `"sub_posts"."org_id" =`) {
		t.Errorf("the nested select ran without its scope: %s", got)
	}
}

// A model with no hook needs no resolving, or the guard would tax every caller
// for a risk they do not have.
func TestNestingAnUnconfinedModelNeedsNoResolving(t *testing.T) {
	h := subHarness(t)
	db := h.handle(sqlb.NewRegistry())

	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	if _, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).All(context.Background(), db); err != nil {
		t.Fatalf("All: %v", err)
	}
}

// A write is where a missing scope is worst: the nested query chooses the rows
// that are changed or removed.
func TestAWriteRefusesAnUnresolvedNestedQuery(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)
	ctx := context.Background()
	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))

	if _, err := sqlb.DeleteRows[User]().
		Where(sqlb.F("id").InQuery(sub)).Exec(ctx, db); err == nil {
		t.Error("a delete nested a confined model without its scope")
	}
	if _, err := sqlb.UpdateRows[User]().Set("name", "x").
		Where(sqlb.F("id").InQuery(sub)).Exec(ctx, db); err == nil {
		t.Error("an update nested a confined model without its scope")
	}
}

// The guard reaches a subquery nested two levels down, not only one. A walk that
// stopped at the first level would let the same leak through a longer chain.
func TestTheGuardReachesANestedNestedQuery(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)

	deep := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	middle := sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("id").InQuery(deep))
	_, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(middle)).All(context.Background(), db)
	if err == nil {
		t.Fatal("a confined model two levels down was nested without its scope")
	}
	if !strings.Contains(err.Error(), "subPost") {
		t.Errorf("error does not name the model: %v", err)
	}
}

// #288: the refusal a scoping hook hits, and why it cannot be the same one.
//
// A rule confining User's reads needs to reach subPost — the row it is
// narrowing by does not carry the column — so it adds a predicate nesting a
// query over a model that is itself confined. The refusal is right: a nested
// SELECT does not run the hooks that confine the table it names, so the inner
// query would be unconfined by construction.
//
// What was wrong was the advice. Resolved needs an Executor and a BeforeQuery
// hook is handed the query and nothing else, so at the one place this is most
// likely to be hit the suggested fix could not be applied. The reporter reached
// the real answer — denormalise the column, which was the better schema anyway
// — by trial and error rather than from the message.
func TestARefusalInsideAHookNamesTheFixesAHookCanActuallyApply(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	sqlb.On[User](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
		q.Where(sqlb.F("id").InQuery(sqlb.Query[subPost]().Select(sqlb.F("author_id"))))
		return nil
	})
	db := h.handle(reg)

	_, err := sqlb.Query[User]().All(context.Background(), db)
	if err == nil {
		t.Fatal("a hook nested a confined model without its scope")
	}
	msg := err.Error()

	// It has to say where the predicate came from: the caller wrote no
	// subquery, and being told "this statement nests a query" sends them
	// looking through code that does not contain one.
	if !strings.Contains(msg, "BeforeQuery") {
		t.Errorf("the refusal does not say a hook added the predicate: %v", err)
	}
	// The two fixes a hook can actually apply, and the model each names.
	for _, want := range []string{"denormalise", "User", "subPost"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// And it must not advise the one call that is unavailable here. This is the
	// whole of the issue: the message read as actionable and was not.
	if strings.Contains(msg, "Resolved(ctx, db)") {
		t.Errorf("the refusal still advises a call a hook has no executor for: %v", err)
	}
}

// The caller-written case keeps the advice that works for it, because there
// Resolved is exactly right and nothing else is as short.
func TestARefusalOutsideAHookStillAdvisesResolved(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)

	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))
	_, err := sqlb.Query[User]().Where(sqlb.F("id").InQuery(sub)).All(context.Background(), db)
	if err == nil {
		t.Fatal("a confined model was nested without its scope")
	}
	if !strings.Contains(err.Error(), "Resolved(ctx, db)") {
		t.Errorf("the caller's own subquery lost the advice that fits it: %v", err)
	}
	if strings.Contains(err.Error(), "BeforeQuery hook added") {
		t.Errorf("a subquery the caller wrote was blamed on a hook: %v", err)
	}
}
