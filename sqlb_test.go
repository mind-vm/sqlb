package sqlb_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/internal/pgfake"
	"github.com/mind-vm/sqlb/schema"
)

// The model under test mirrors what codegen emits from a schema declaration:
// db tags for column names, sqlb tags for capabilities.
type User struct {
	ID        string    `db:"id" sqlb:"pk,default"`
	Email     string    `db:"email" sqlb:"filter,search"`
	Name      string    `db:"name" sqlb:"filter,search,sort"`
	Age       *int32    `db:"age" sqlb:"filter,sort"`
	OrgID     string    `db:"org_id" sqlb:"filter"`
	Password  string    `db:"password_hash" sqlb:"hidden"`
	CreatedAt time.Time `db:"created_at" sqlb:"sort,readonly,default"`
}

func (User) TableName() string { return "users" }

func TestModelReflection(t *testing.T) {
	m := sqlb.ModelOf[User]()

	if m.Table != "users" {
		t.Errorf("table = %q, want %q", m.Table, "users")
	}
	if m.PK == nil || m.PK.Name != "id" {
		t.Fatalf("primary key = %v, want id", m.PK)
	}
	if !m.PK.HasDefault {
		t.Error("id should carry HasDefault from its `default` tag")
	}
	if got, want := len(m.Columns), 7; got != want {
		t.Errorf("mapped %d columns, want %d", got, want)
	}
	if col := m.Column("password_hash"); col == nil || !col.Hidden {
		t.Error("password_hash should be mapped and hidden")
	}
	if got, want := len(m.Selectable()), 6; got != want {
		t.Errorf("Selectable returned %d columns, want %d (hidden excluded)", got, want)
	}
	// search implies filter, so that ?email=... works on a searchable column.
	if col := m.Column("email"); !col.Filterable {
		t.Error("a searchable column should also be filterable")
	}
}

func TestTableNameDerivation(t *testing.T) {
	type Category struct {
		ID string `db:"id"`
	}
	type Box struct {
		ID string `db:"id"`
	}
	if got := sqlb.ModelOf[Category]().Table; got != "categories" {
		t.Errorf("Category maps to %q, want %q", got, "categories")
	}
	if got := sqlb.ModelOf[Box]().Table; got != "boxes" {
		t.Errorf("Box maps to %q, want %q", got, "boxes")
	}
}

func TestSelectSQL(t *testing.T) {
	tests := []struct {
		name string
		q    func() *sqlb.Builder[User]
		sql  string
		args []any
	}{
		{
			name: "all columns",
			q:    func() *sqlb.Builder[User] { return sqlb.Query[User]() },
			sql:  `SELECT "users"."id", "users"."email", "users"."name", "users"."age", "users"."org_id", "users"."password_hash", "users"."created_at" FROM "users"`,
		},
		{
			name: "single predicate needs no parentheses",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").Gte(18))
			},
			sql:  `SELECT "id" FROM "users" WHERE "age" >= $1`,
			args: []any{18},
		},
		{
			name: "conjunction is parenthesised so precedence never depends on order",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("age").Gte(18)).
					Where(sqlb.F("org_id").Eq("acme"))
			},
			sql:  `SELECT "id" FROM "users" WHERE ("age" >= $1) AND ("org_id" = $2)`,
			args: []any{18, "acme"},
		},
		{
			name: "disjunction nested in a conjunction",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(
					sqlb.F("org_id").Eq("acme"),
					sqlb.Or(sqlb.F("age").Lt(18), sqlb.F("age").Gt(65)),
				)
			},
			sql:  `SELECT "id" FROM "users" WHERE ("org_id" = $1) AND (("age" < $2) OR ("age" > $3))`,
			args: []any{"acme", 18, 65},
		},
		{
			name: "in list",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("org_id").OneOf("a", "b"))
			},
			sql:  `SELECT "id" FROM "users" WHERE "org_id" IN ($1, $2)`,
			args: []any{"a", "b"},
		},
		{
			// `IN (NULL)` is never true, so binding the nil made the set
			// quietly narrower than the caller wrote.
			name: "a nil member of OneOf widens the set with IS NULL",
			q: func() *sqlb.Builder[User] {
				var missing *string
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("name").OneOf("a", missing))
			},
			sql:  `SELECT "id" FROM "users" WHERE ("name" IN ($1)) OR ("name" IS NULL)`,
			args: []any{"a"},
		},
		{
			name: "a OneOf of nothing but nils is just IS NULL",
			q: func() *sqlb.Builder[User] {
				var missing *string
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("name").OneOf(missing, nil))
			},
			sql: `SELECT "id" FROM "users" WHERE "name" IS NULL`,
		},
		{
			// The widened form is a disjunction, so it has to keep its
			// parentheses when something else is ANDed alongside it.
			name: "a widened OneOf stays parenthesised under AND",
			q: func() *sqlb.Builder[User] {
				var missing *string
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("name").OneOf("a", missing), sqlb.F("age").Eq(18))
			},
			sql:  `SELECT "id" FROM "users" WHERE (("name" IN ($1)) OR ("name" IS NULL)) AND ("age" = $2)`,
			args: []any{"a", 18},
		},
		{
			// A set with no nil in it must not grow an OR.
			name: "OneOf without a nil member is unchanged",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("name").OneOf("a", "b"))
			},
			sql:  `SELECT "id" FROM "users" WHERE "name" IN ($1, $2)`,
			args: []any{"a", "b"},
		},
		{
			// The documented asymmetry: NotOneOf still binds the nil, pending
			// the null-aware negation work.
			name: "a nil member of NotOneOf still binds",
			q: func() *sqlb.Builder[User] {
				var missing *string
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("name").NotOneOf("a", missing))
			},
			sql:  `SELECT "id" FROM "users" WHERE "name" NOT IN ($1, $2)`,
			args: []any{"a", (*string)(nil)},
		},
		{
			name: "between renders without parenthesising its bounds",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").Between(18, 65))
			},
			sql:  `SELECT "id" FROM "users" WHERE "age" BETWEEN $1 AND $2`,
			args: []any{18, 65},
		},
		{
			name: "null test",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").IsNull())
			},
			sql: `SELECT "id" FROM "users" WHERE "age" IS NULL`,
		},
		{
			// The spelling a hand-written hook actually reaches for: every
			// nullable column maps to a pointer, so the comparand arrives as a
			// non-nil interface holding a nil pointer. It used to compile to
			// `= $1` bound to NULL and match nothing.
			name: "a nil pointer comparand is the same NULL as an untyped nil",
			q: func() *sqlb.Builder[User] {
				var missing *int
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").Eq(missing))
			},
			sql: `SELECT "id" FROM "users" WHERE "age" IS NULL`,
		},
		{
			name: "a nil pointer comparand negates to IS NOT NULL",
			q: func() *sqlb.Builder[User] {
				var missing *string
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("name").Neq(missing))
			},
			sql: `SELECT "id" FROM "users" WHERE "name" IS NOT NULL`,
		},
		{
			// The other direction: a pointer that *has* a value still binds,
			// rather than the fix swallowing every pointer.
			name: "a non-nil pointer comparand still binds",
			q: func() *sqlb.Builder[User] {
				age := 18
				return sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").Eq(&age))
			},
			sql: `SELECT "id" FROM "users" WHERE "age" = $1`,
			// DeepEqual follows pointers, so this compares the pointee.
			args: []any{ptr(18)},
		},
		{
			name: "ordering and pagination",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).
					OrderBy(sqlb.F("created_at").Desc().NullsLast(), sqlb.F("name").Asc()).
					Page(3, 20)
			},
			sql: `SELECT "id" FROM "users" ORDER BY "created_at" DESC NULLS LAST, "name" ASC LIMIT 20 OFFSET 40`,
		},
		{
			name: "grouped aggregate",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().
					Select(sqlb.F("org_id"), sqlb.Count().As("n"), sqlb.Avg(sqlb.F("age")).As("avg_age")).
					GroupBy(sqlb.F("org_id")).
					Having(sqlb.RawPred("count(*) > ?", 5))
			},
			sql:  `SELECT "org_id", count(*) AS "n", avg("age") AS "avg_age" FROM "users" GROUP BY "org_id" HAVING count(*) > $1`,
			args: []any{5},
		},
		{
			name: "join with alias",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("users.id"), sqlb.F("o.name")).
					Join("orgs", "o", sqlb.F("users.org_id").EqField(sqlb.F("o.id")))
			},
			sql: `SELECT "users"."id", "o"."name" FROM "users" JOIN "orgs" AS "o" ON "users"."org_id" = "o"."id"`,
		},
		{
			name: "locking",
			q: func() *sqlb.Builder[User] {
				return sqlb.Query[User]().Select(sqlb.F("id")).
					Where(sqlb.F("id").Eq("u1")).ForUpdate().SkipLocked()
			},
			sql:  `SELECT "id" FROM "users" WHERE "id" = $1 FOR UPDATE SKIP LOCKED`,
			args: []any{"u1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, args, err := tt.q().SQL()
			if err != nil {
				t.Fatalf("SQL() error: %v", err)
			}
			if got != tt.sql {
				t.Errorf("SQL mismatch\n got: %s\nwant: %s", got, tt.sql)
			}
			if !reflect.DeepEqual(normalise(args), normalise(tt.args)) {
				t.Errorf("args = %#v, want %#v", args, tt.args)
			}
		})
	}
}

// TestConditionalComposition covers the reason this builder exists: a filter
// that is absent must leave no trace in the SQL, without an if statement at
// every call site.
func TestConditionalComposition(t *testing.T) {
	build := func(search, org string, minAge int) string {
		q := sqlb.Query[User]().Select(sqlb.F("id")).
			Where(
				sqlb.If(search != "", sqlb.F("name").Contains(search)),
				sqlb.If(org != "", sqlb.F("org_id").Eq(org)),
				sqlb.If(minAge > 0, sqlb.F("age").Gte(minAge)),
			)
		sql, _, err := q.SQL()
		if err != nil {
			t.Fatalf("SQL() error: %v", err)
		}
		return sql
	}

	if got, want := build("", "", 0), `SELECT "id" FROM "users"`; got != want {
		t.Errorf("no filters:\n got: %s\nwant: %s", got, want)
	}
	if got, want := build("", "acme", 0), `SELECT "id" FROM "users" WHERE "org_id" = $1`; got != want {
		t.Errorf("one filter:\n got: %s\nwant: %s", got, want)
	}
	// And folds left, so the nesting is ((a AND b) AND c).
	want := `SELECT "id" FROM "users" WHERE (("name" ILIKE $1) AND ("org_id" = $2)) AND ("age" >= $3)`
	if got := build("ada", "acme", 18); got != want {
		t.Errorf("three filters:\n got: %s\nwant: %s", got, want)
	}
}

// TestLikeEscaping guards the case where a user types a wildcard into a search
// box and would otherwise match everything.
func TestLikeEscaping(t *testing.T) {
	_, args, err := sqlb.Query[User]().Select(sqlb.F("id")).
		Where(sqlb.F("name").Contains("100%_off")).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if got, want := args[0], `%100\%\_off%`; got != want {
		t.Errorf("pattern = %q, want %q", got, want)
	}
}

func TestCountSQL(t *testing.T) {
	h := newHarness(t, []string{"count"}, [][]any{{int64(3)}})
	defer h.close()

	q := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("name").Asc()).Page(2, 10)

	n, err := q.Count(context.Background(), h.db)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	// Ordering and pagination must not survive into the count, or a paged list
	// would report its page size as the total.
	want := `SELECT count(*) FROM "users" WHERE "org_id" = $1`
	if got := h.lastQuery(); got != want {
		t.Errorf("count SQL\n got: %s\nwant: %s", got, want)
	}
}

// A count has to agree with what All returns. Dropping the DISTINCT and
// counting the rows underneath answers a different question, and answers it
// too high — so ?count=exact would report more rows than the client can ever
// page through.
func TestCountOfADistinctQueryCountsDistinctRows(t *testing.T) {
	h := newHarness(t, []string{"count"}, [][]any{{int64(2)}})
	defer h.close()

	q := sqlb.Query[User]().Distinct().
		Select(sqlb.F("org_id")).
		Where(sqlb.F("name").Eq("Ada")).
		OrderBy(sqlb.F("org_id").Asc()).Page(2, 10)

	if _, err := q.Count(context.Background(), h.db); err != nil {
		t.Fatalf("Count: %v", err)
	}
	want := `SELECT count(*) FROM (SELECT DISTINCT "org_id" FROM "users" WHERE "name" = $1) AS "distinct_rows"`
	if got := h.lastQuery(); got != want {
		t.Errorf("count SQL\n got: %s\nwant: %s", got, want)
	}
}

func TestInsertOmitsDefaultedZeroColumns(t *testing.T) {
	u := &User{Email: "ada@example.com", Name: "Ada", OrgID: "acme"}
	sql, args, err := sqlb.InsertRows(u).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	// id and created_at carry database defaults and hold zero values, so they
	// are left for the database to fill.
	want := `INSERT INTO "users" ("email", "name", "age", "org_id", "password_hash") VALUES ($1, $2, $3, $4, $5)` +
		` RETURNING "id", "email", "name", "age", "org_id", "password_hash", "created_at"`
	if sql != want {
		t.Errorf("insert SQL\n got: %s\nwant: %s", sql, want)
	}
	if len(args) != 5 {
		t.Errorf("bound %d args, want 5", len(args))
	}
}

func TestInsertKeepsExplicitValueOverDefault(t *testing.T) {
	u := &User{ID: "fixed-id", Email: "ada@example.com"}
	sql, _, err := sqlb.InsertRows(u).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if !contains(sql, `"id"`) {
		t.Errorf("an explicitly set id must be written, got: %s", sql)
	}
}

// The default-zero rule is per row, not per statement. A defaulted column no
// row fills in leaves the statement, as above; but when one row in a batch sets
// it, the column stays — and the rows that left it zero used to bind an
// explicit zero, so the same row got the database's default when inserted alone
// and a zero when inserted beside a neighbour (#73). Postgres accepts the
// DEFAULT keyword per position in a multi-row VALUES, which is what makes the
// rule read the way the doc comment always claimed.
func TestInsertMixedBatchTakesTheDefaultPerRow(t *testing.T) {
	set := &User{ID: "fixed-id", Email: "ada@example.com"}
	unset := &User{Email: "bob@example.com"}

	sql, args, err := sqlb.InsertRows(set, unset).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	// Before RETURNING, which names every column by construction and would
	// make any assertion over the whole statement pass on anything.
	written := sql[:indexOf(sql, " RETURNING ")]
	if !contains(written, `"id"`) {
		t.Fatalf("the column one row set must be written:\n%s", sql)
	}
	values := written[indexOf(written, "VALUES "):]
	if !contains(values, "(DEFAULT, ") {
		t.Errorf("the row that left the defaulted column zero should take DEFAULT:\n%s", values)
	}
	// The DEFAULT keyword is not a bind, so the second row's id costs no
	// parameter — six columns over two rows, minus the one taking the default.
	if len(args) != 11 {
		t.Errorf("bound %d args, want 11: DEFAULT is a keyword, not a parameter", len(args))
	}

	// And the solo case is unchanged: nothing to keep the column for, so it
	// leaves the statement entirely rather than becoming a tuple of DEFAULTs.
	solo, _, err := sqlb.InsertRows(unset).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	soloWritten := solo[:indexOf(solo, " RETURNING ")]
	if contains(soloWritten, `"id"`) || contains(soloWritten, "DEFAULT") {
		t.Errorf("a single zero-valued row should omit the column, not spell DEFAULT:\n%s", solo)
	}
}

func TestUpsert(t *testing.T) {
	u := &User{Email: "ada@example.com", Name: "Ada"}
	sql, _, err := sqlb.InsertRows(u).OnConflictUpdate([]string{"email"}, "name").SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if !contains(sql, `ON CONFLICT ("email") DO UPDATE SET "name" = EXCLUDED."name"`) {
		t.Errorf("upsert clause missing from: %s", sql)
	}
}

// A multi-row insert writes the database's values back into the caller's
// structs by position, which is only sound because a VALUES insert returns one
// row per row written, in order.
func TestInsertWritesStoredValuesBack(t *testing.T) {
	h := newHarness(t, storedUserColumns, [][]any{
		storedUser("gen-1", "ada@example.com"),
		storedUser("gen-2", "bob@example.com"),
	})
	defer h.close()

	ada := &User{Email: "ada@example.com", Name: "Ada", OrgID: "acme"}
	bob := &User{Email: "bob@example.com", Name: "Bob", OrgID: "acme"}
	if _, err := sqlb.InsertRows(ada, bob).Exec(context.Background(), h.db); err != nil {
		t.Fatalf("Exec() error: %v", err)
	}
	if ada.ID != "gen-1" || bob.ID != "gen-2" {
		t.Errorf("generated ids = %q, %q; want gen-1, gen-2", ada.ID, bob.ID)
	}
}

// ON CONFLICT DO NOTHING drops the skipped row from the result, so the rows
// that come back no longer line up with the rows that went in. Writing them
// back by position would hand one row's generated primary key to a different
// row — a struct that then reads as a plausible row it is not. Nothing is
// written back at all in that case, and the returned slice is the account.
func TestInsertDoesNotWriteBackWhenAConflictSkippedARow(t *testing.T) {
	// Three rows in; the middle one conflicts, so the database returns two.
	h := newHarness(t, storedUserColumns, [][]any{
		storedUser("gen-ada", "ada@example.com"),
		storedUser("gen-cy", "cy@example.com"),
	})
	defer h.close()

	ada := &User{Email: "ada@example.com", Name: "Ada", OrgID: "acme"}
	bob := &User{Email: "bob@example.com", Name: "Bob", OrgID: "acme"}
	cy := &User{Email: "cy@example.com", Name: "Cy", OrgID: "acme"}

	stored, err := sqlb.InsertRows(ada, bob, cy).
		OnConflictDoNothing("email").Exec(context.Background(), h.db)
	if err != nil {
		t.Fatalf("Exec() error: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("returned %d rows, want 2", len(stored))
	}
	// The returned slice is complete and correct.
	if stored[0].ID != "gen-ada" || stored[1].ID != "gen-cy" {
		t.Errorf("returned ids = %q, %q; want gen-ada, gen-cy", stored[0].ID, stored[1].ID)
	}
	// Before the fix, bob took gen-cy and cy kept nothing: the skipped row
	// carried away its successor's identity.
	for _, row := range []struct {
		name string
		u    *User
	}{{"ada", ada}, {"bob", bob}, {"cy", cy}} {
		if row.u.ID != "" {
			t.Errorf("%s.ID = %q; a skipped row makes position meaningless, so no struct may be written back",
				row.name, row.u.ID)
		}
	}
}

// One after OnConflictDoNothing is refused, because the alternative is that the
// conflict — the case the clause exists to allow — arrives as ErrNotFound from
// a call whose job was to make the row exist (#146).
//
// The harness returns no rows, which is what the database does on the second
// call: with the refusal removed this test does not merely fail, it fails with
// the ErrNotFound the issue reported.
func TestOneIsRefusedAfterOnConflictDoNothing(t *testing.T) {
	h := newHarness(t, storedUserColumns, nil)
	defer h.close()

	u := &User{Email: "ada@example.com", Name: "Ada", OrgID: "acme"}
	_, err := sqlb.InsertRows(u).OnConflictDoNothing("email").One(context.Background(), h.db)
	if err == nil {
		t.Fatal("One after OnConflictDoNothing was accepted; it answers ErrNotFound on the idempotent path")
	}
	if errors.Is(err, sqlb.ErrNotFound) {
		t.Errorf("the refusal must not be ErrNotFound, which is the confusion it exists to remove: %v", err)
	}
	// ADR-0011: a rejection names what would have been accepted. Both routes
	// out are here, because which one is right depends on whether the caller
	// wants the row or only wants it to exist.
	for _, want := range []string{"Exec", `OnConflictUpdate([]string{"email"}, "email")`} {
		if !contains(err.Error(), want) {
			t.Errorf("the error should name %s, got: %v", want, err)
		}
	}
}

// The refusal is about DO NOTHING specifically. A conflict clause that updates
// something returns a row on every path, so One over it is exactly right — and
// it is the spelling the refusal recommends, so breaking it would leave the
// error pointing at a call that does not work.
func TestOneIsAllowedAfterOnConflictUpdate(t *testing.T) {
	h := newHarness(t, storedUserColumns, [][]any{storedUser("gen-ada", "ada@example.com")})
	defer h.close()

	u := &User{Email: "ada@example.com", Name: "Ada", OrgID: "acme"}
	got, err := sqlb.InsertRows(u).
		OnConflictUpdate([]string{"email"}, "email").
		One(context.Background(), h.db)
	if err != nil {
		t.Fatalf("One after OnConflictUpdate: %v", err)
	}
	if got.ID != "gen-ada" {
		t.Errorf("returned id = %q, want gen-ada", got.ID)
	}
}

// storedUserColumns is the RETURNING order writeReturning emits for User.
var storedUserColumns = []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"}

func storedUser(id, email string) []any {
	return []any{id, email, "", nil, "acme", "", time.Time{}}
}

func TestUnscopedMutationsAreRefused(t *testing.T) {
	if _, _, err := sqlb.UpdateRows[User]().Set("name", "x").SQL(); !errors.Is(err, sqlb.ErrUnscoped) {
		t.Errorf("unscoped update error = %v, want ErrUnscoped", err)
	}
	if _, _, err := sqlb.DeleteRows[User]().SQL(); !errors.Is(err, sqlb.ErrUnscoped) {
		t.Errorf("unscoped delete error = %v, want ErrUnscoped", err)
	}
	// Everything is the explicit opt-in.
	if _, _, err := sqlb.DeleteRows[User]().Everything().SQL(); err != nil {
		t.Errorf("Everything() should permit the delete, got %v", err)
	}
}

func TestUpdateRejectsUnknownColumn(t *testing.T) {
	_, _, err := sqlb.UpdateRows[User]().Set("nam", "typo").Where(sqlb.F("id").Eq("u1")).SQL()
	if err == nil {
		t.Fatal("expected an error naming the unknown column")
	}
	if !contains(err.Error(), `"nam"`) {
		t.Errorf("error should name the offending column, got: %v", err)
	}
}

// TestBeforeQueryHook covers the tenant-scoping path: one registration
// constrains every read of the model.
func TestBeforeQueryHook(t *testing.T) {
	type Post struct {
		ID    string `db:"id" sqlb:"pk"`
		OrgID string `db:"org_id" sqlb:"filter"`
		Title string `db:"title"`
	}

	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Post](reg)
	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Post]) error {
		q.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	h := newHarness(t, []string{"id", "org_id", "title"}, [][]any{{"p1", "acme", "Hello"}})
	defer h.close()

	posts, err := sqlb.Query[Post]().Where(sqlb.F("title").Eq("Hello")).All(context.Background(), h.handle(reg))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(posts) != 1 || posts[0].Title != "Hello" {
		t.Fatalf("rows = %#v", posts)
	}
	want := `SELECT "posts"."id", "posts"."org_id", "posts"."title" FROM "posts" WHERE ("title" = $1) AND ("org_id" = $2)`
	if got := h.lastQuery(); got != want {
		t.Errorf("hooked SQL\n got: %s\nwant: %s", got, want)
	}
}

// TestHooksDoNotAccumulate guards the in-place mutation design: running the
// same builder twice must not apply the hook's predicate twice.
func TestHooksDoNotAccumulate(t *testing.T) {
	type Doc struct {
		ID    string `db:"id" sqlb:"pk"`
		OrgID string `db:"org_id"`
	}
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Doc](reg)
	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Doc]) error {
		q.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	h := newHarness(t, []string{"id", "org_id"}, nil)
	defer h.close()

	q := sqlb.Query[Doc]()
	if _, err := q.All(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("first All: %v", err)
	}
	first := h.lastQuery()
	if _, err := q.All(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("second All: %v", err)
	}
	if second := h.lastQuery(); second != first {
		t.Errorf("running twice changed the SQL\nfirst:  %s\nsecond: %s", first, second)
	}
}

// The same guard for mutations, which had the opposite behaviour: hooks ran
// against the caller's statement rather than a copy, so a second Exec assigned
// twice and narrowed twice. Set("updated_at", …) is the example the BeforeUpdate
// doc comment itself gives, so accumulating was reachable from the documented
// use rather than from an exotic one.
func TestMutationHooksDoNotAccumulate(t *testing.T) {
	type Doc struct {
		ID        string `db:"id" sqlb:"pk"`
		OrgID     string `db:"org_id"`
		Title     string `db:"title"`
		UpdatedAt string `db:"updated_at"`
	}
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Doc](reg)
	hooks.BeforeUpdate(func(ctx context.Context, u *sqlb.Update[Doc]) error {
		u.Set("updated_at", "now")
		u.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})
	hooks.BeforeDelete(func(ctx context.Context, d *sqlb.Delete[Doc]) error {
		d.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	h := newHarness(t, []string{"id", "org_id", "title", "updated_at"}, nil)
	defer h.close()
	ctx := context.Background()

	u := sqlb.UpdateRows[Doc]().Set("title", "Hello").Where(sqlb.F("id").Eq("d1"))
	if _, err := u.Exec(ctx, h.handle(reg)); err != nil {
		t.Fatalf("first update: %v", err)
	}
	first := h.lastQuery()
	if _, err := u.Exec(ctx, h.handle(reg)); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if second := h.lastQuery(); second != first {
		t.Errorf("running the update twice changed the SQL\nfirst:  %s\nsecond: %s", first, second)
	}

	d := sqlb.DeleteRows[Doc]().Where(sqlb.F("id").Eq("d1"))
	if _, err := d.Exec(ctx, h.handle(reg)); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	firstDel := h.lastQuery()
	if _, err := d.Exec(ctx, h.handle(reg)); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if second := h.lastQuery(); second != firstDel {
		t.Errorf("running the delete twice changed the SQL\nfirst:  %s\nsecond: %s", firstDel, second)
	}
}

// deleteDoc is the model the AfterDeleteRows tests remove rows of.
type deleteDoc struct {
	ID    string `db:"id" sqlb:"pk"`
	OrgID string `db:"org_id"`
}

// AfterDeleteRows receives the rows, which is the whole of #144: a module
// publishing a deletion event needs the identity of what went, and a count
// cannot supply it.
func TestAfterDeleteRowsReceivesTheRows(t *testing.T) {
	reg := sqlb.NewRegistry()
	var got []deleteDoc
	var count int64
	sqlb.On[deleteDoc](reg).
		AfterDelete(func(_ context.Context, n int64) error { count = n; return nil }).
		AfterDeleteRows(func(_ context.Context, rows []deleteDoc) error { got = rows; return nil })

	h := newHarness(t, []string{"id", "org_id"}, [][]any{{"d1", "acme"}, {"d2", "acme"}})
	defer h.close()

	n, err := sqlb.DeleteRows[deleteDoc]().
		Where(sqlb.F("org_id").Eq("acme")).Exec(context.Background(), h.handle(reg))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The statement asked for them back, which is what makes the rest possible.
	if !strings.Contains(h.lastQuery(), "RETURNING") {
		t.Errorf("a registered AfterDeleteRows did not put RETURNING on the delete: %s", h.lastQuery())
	}
	if len(got) != 2 || got[0].ID != "d1" || got[1].ID != "d2" {
		t.Errorf("rows = %+v, want both removed rows", got)
	}
	// And both kinds ran, agreeing about how many rows went. RETURNING yields one
	// row per row removed, so the count is the same number by the other road —
	// a disagreement here would mean the count came from somewhere else.
	if n != 2 || count != 2 {
		t.Errorf("Exec returned %d and AfterDelete saw %d, want 2 and 2", n, count)
	}
}

// The other half, and the reason this is a second hook rather than a new
// signature for the first: nothing is added to the statement unless a hook asked
// for the rows, so a bulk delete is not silently made to materialise everything
// it removed.
//
// Proven both ways against the test above: same model, same statement, and the
// only difference is which hook is registered.
func TestADeleteWithNoRowHookDoesNotReturnRows(t *testing.T) {
	reg := sqlb.NewRegistry()
	var count int64
	sqlb.On[deleteDoc](reg).AfterDelete(func(_ context.Context, n int64) error { count = n; return nil })

	h := newHarness(t, []string{"id", "org_id"}, [][]any{{"d1", "acme"}})
	defer h.close()

	if _, err := sqlb.DeleteRows[deleteDoc]().
		Where(sqlb.F("org_id").Eq("acme")).Exec(context.Background(), h.handle(reg)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if strings.Contains(h.lastQuery(), "RETURNING") {
		t.Errorf("a delete nothing reads the rows of paid to scan them: %s", h.lastQuery())
	}
	if count != 1 {
		t.Errorf("AfterDelete saw %d rows, want the command tag's 1", count)
	}

	// SQL() has no executor and therefore no hooks, so it prints the statement a
	// delete sends when nothing wants the rows — under either registration.
	stmt, _, err := sqlb.DeleteRows[deleteDoc]().Where(sqlb.F("id").Eq("d1")).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(stmt, "RETURNING") {
		t.Errorf("SQL() invented a RETURNING it cannot know about: %s", stmt)
	}
}

// A row hook that fails aborts the delete, the same way AfterCreate does. It
// runs inside the caller's transaction, so this is what makes it usable for a
// validation and unusable for announcing anything the outside world can see.
func TestAfterDeleteRowsCanAbort(t *testing.T) {
	sentinel := errors.New("that row may not be removed")
	reg := sqlb.NewRegistry()
	sqlb.On[deleteDoc](reg).
		AfterDeleteRows(func(context.Context, []deleteDoc) error { return sentinel })

	h := newHarness(t, []string{"id", "org_id"}, [][]any{{"d1", "acme"}})
	defer h.close()

	if _, err := sqlb.DeleteRows[deleteDoc]().
		Where(sqlb.F("id").Eq("d1")).Exec(context.Background(), h.handle(reg)); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the hook's error", err)
	}
}

// TestHookErrorAborts confirms a failing hook stops the query rather than
// running it unscoped.
func TestHookErrorAborts(t *testing.T) {
	type Secret struct {
		ID string `db:"id" sqlb:"pk"`
	}
	sentinel := errors.New("no tenant in context")
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Secret](reg)
	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Secret]) error { return sentinel })

	h := newHarness(t, []string{"id"}, nil)
	defer h.close()

	if _, err := sqlb.Query[Secret]().All(context.Background(), h.handle(reg)); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the hook's error", err)
	}
	if h.lastQuery() != "" {
		t.Errorf("no query should have been issued, got: %s", h.lastQuery())
	}
}

func TestCollectIntoAggregateShape(t *testing.T) {
	type OrgSize struct {
		OrgID string `db:"org_id"`
		N     int64  `db:"n"`
	}
	h := newHarness(t, []string{"org_id", "n"}, [][]any{
		{"acme", int64(12)}, {"globex", int64(4)},
	})
	defer h.close()

	rows, err := sqlb.Collect[OrgSize](context.Background(), h.db,
		sqlb.Query[User]().Select(sqlb.F("org_id"), sqlb.Count().As("n")).GroupBy(sqlb.F("org_id")))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 2 || rows[0].OrgID != "acme" || rows[0].N != 12 {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestScanIgnoresUnmappedColumns(t *testing.T) {
	h := newHarness(t,
		[]string{"id", "email", "name", "age", "org_id", "password_hash", "created_at", "row_number"},
		[][]any{{"u1", "ada@example.com", "Ada", int64(36), "acme", "", time.Time{}, int64(1)}})
	defer h.close()

	users, err := sqlb.Query[User]().All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 || users[0].Name != "Ada" {
		t.Fatalf("rows = %#v", users)
	}
	if users[0].Age == nil || *users[0].Age != 36 {
		t.Errorf("nullable column did not scan: %#v", users[0].Age)
	}
}

func TestOneReportsAmbiguity(t *testing.T) {
	h := newHarness(t, []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"},
		[][]any{
			{"u1", "a@example.com", "A", nil, "acme", "", time.Time{}},
			{"u2", "b@example.com", "B", nil, "acme", "", time.Time{}},
		})
	defer h.close()

	_, err := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).One(context.Background(), h.db)
	if err == nil || !contains(err.Error(), "more than one row") {
		t.Errorf("error = %v, want an ambiguity report", err)
	}
}

func TestOneReturnsNotFound(t *testing.T) {
	h := newHarness(t, []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"}, nil)
	defer h.close()

	_, err := sqlb.Query[User]().Where(sqlb.F("id").Eq("nope")).One(context.Background(), h.db)
	if !errors.Is(err, sqlb.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	base := sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("org_id").Eq("acme"))
	derived := base.Clone().Where(sqlb.F("age").Gte(18))

	baseSQL, _, _ := base.SQL()
	derivedSQL, _, _ := derived.SQL()
	if contains(baseSQL, "age") {
		t.Errorf("mutating the clone leaked into the base: %s", baseSQL)
	}
	if !contains(derivedSQL, "age") {
		t.Errorf("clone lost its added predicate: %s", derivedSQL)
	}
}

func TestRawPlaceholderRenumbering(t *testing.T) {
	sql, args, err := sqlb.Query[User]().Select(sqlb.F("id")).
		Where(sqlb.F("org_id").Eq("acme"), sqlb.RawPred("age % ? = ?", 2, 0)).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	want := `SELECT "id" FROM "users" WHERE ("org_id" = $1) AND (age % $2 = $3)`
	if sql != want {
		t.Errorf("SQL\n got: %s\nwant: %s", sql, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %#v, want 3", args)
	}
}

func TestRawArgumentCountMismatch(t *testing.T) {
	_, _, err := sqlb.Query[User]().Where(sqlb.RawPred("age > ?")).SQL()
	if err == nil {
		t.Fatal("expected an error for a placeholder with no argument")
	}
}

// invLevel is the model #173's "claim under contention" example is about: a
// row where two columns, not one, decide whether a write may proceed.
type invLevel struct {
	ID       string `db:"id" sqlb:"pk"`
	OnHand   int32  `db:"on_hand"`
	Reserved int32  `db:"reserved"`
}

func (invLevel) TableName() string { return "inventory_levels" }

// TestMatchBuildsAPredicateOverAnArbitraryExpr is #173: Field's comparisons
// only ever compare a bare column to a parameter, which cannot express
// `(on_hand - reserved) >= $1` — a predicate spanning two columns. Match is
// the entry point that closes the gap without falling back to RawPred, which
// would leave "on_hand" and "reserved" unchecked against the schema.
func TestMatchBuildsAPredicateOverAnArbitraryExpr(t *testing.T) {
	claim := sqlb.Match(sqlb.Binary{
		Op:    ">=",
		Left:  sqlb.Sub(sqlb.F("on_hand"), sqlb.F("reserved")),
		Right: sqlb.Val(3),
	})

	sql, args, err := sqlb.UpdateRows[invLevel]().
		Set("reserved", 8).
		Where(sqlb.F("id").Eq("sku-1"), claim).
		SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}

	_, where, _ := strings.Cut(sql, " WHERE ")
	where, _, _ = strings.Cut(where, " RETURNING ")
	want := `("id" = $2) AND (("on_hand" - "reserved") >= $3)`
	if where != want {
		t.Errorf("WHERE\n got: %s\nwant: %s", where, want)
	}
	if !strings.HasPrefix(sql, `UPDATE "inventory_levels" SET "reserved" = $1`) {
		t.Errorf("SET clause missing or reordered: %s", sql)
	}
	if len(args) != 3 || args[0] != 8 || args[1] != "sku-1" || args[2] != 3 {
		t.Errorf("args = %#v, want [8 sku-1 3]", args)
	}
}

func TestIdentifierQuotingNeutralisesInjection(t *testing.T) {
	// F takes an arbitrary string, so a caller could pass something hostile.
	// Quoting must contain it rather than letting it close the identifier.
	sql, _, err := sqlb.Query[User]().Select(sqlb.F(`id" FROM "users"; DROP TABLE users --`)).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if contains(sql, "DROP TABLE users --;") {
		t.Fatalf("identifier escaped its quotes: %s", sql)
	}
	want := `SELECT "id"" FROM ""users""; DROP TABLE users --" FROM "users"`
	if sql != want {
		t.Errorf("SQL\n got: %s\nwant: %s", sql, want)
	}
}

func TestTypedColumnsCarryTheirType(t *testing.T) {
	age := sqlb.Typed[int32]("age")
	sql, args, err := sqlb.Query[User]().Select(sqlb.F("id")).Where(age.Gte(18)).SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if sql != `SELECT "id" FROM "users" WHERE "age" >= $1` {
		t.Errorf("SQL = %s", sql)
	}
	if _, ok := args[0].(int32); !ok {
		t.Errorf("arg type = %T, want int32", args[0])
	}
}

// --- test harness -----------------------------------------------------------

// harness is a pgx-shaped executor that records statements and replays canned
// rows, so the builder, hooks and scanner can be tested end to end without a
// live Postgres.
//
// It used to be a registered database/sql driver. ADR-0040 made pgx the
// contract, so what a test needs to stand in for is an Executor rather than a
// driver — which is less machinery, not more: no registration, no name
// sequence, and the statement arrives as text rather than through a prepared
// statement nobody was asserting on.
type harness struct {
	t    *testing.T
	db   *fakeDB
	mu   sync.Mutex
	log  []string
	cols []string
	rows [][]any
	err  error
	// Transaction control, used by db_test.go. BEGIN, COMMIT and ROLLBACK are
	// appended to log alongside statements so a test can assert on the shape
	// of a whole unit of work.
	txErr      error
	commitErr  error
	lastTxOpts pgx.TxOptions
}

func newHarness(t *testing.T, cols []string, rows [][]any) *harness {
	t.Helper()
	h := &harness{t: t, cols: cols, rows: rows}
	h.db = &fakeDB{h: h}
	return h
}

// handle wraps the harness as a sqlb handle carrying reg.
//
// This is how a test that registers hooks makes them apply: hooks resolve
// against the registry the handle carries, and a bare Executor carries none
// (ADR-0047). Passing h.db straight to a terminal method is still right for
// the tests that assert on SQL with no hooks in play.
func (h *harness) handle(reg *sqlb.Registry) *sqlb.DB {
	return sqlb.New(h.db).WithHooks(reg)
}

// close is kept for the call sites that defer it. There is no pool to release
// now that the harness is not a driver, and a test that stops calling it should
// not have to be found and edited to say so.
func (h *harness) close() {}

// failWith makes the next statements fail, standing in for a database that
// rejects a query.
func (h *harness) failWith(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = errors.New(msg)
}

// failWithErr makes the next statements fail with a specific error, so a test
// can present something driver-shaped rather than a bare string.
func (h *harness) failWithErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

// pgErr stands in for pgconn.PgError. It carries SQLState as a method, which is
// how the classifier reaches the class, and the constraint name as a field.
type pgErr struct {
	code       string
	constraint string
	message    string
}

func (e *pgErr) Error() string    { return "ERROR: " + e.message + " (SQLSTATE " + e.code + ")" }
func (e *pgErr) SQLState() string { return e.code }

func (h *harness) record(q string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, q)
}

func (h *harness) lastQuery() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.log) == 0 {
		return ""
	}
	return h.log[len(h.log)-1]
}

// fakeDB is the Executor the tests run against. It is also a Beginner, so
// WithTx works through it.
type fakeDB struct{ h *harness }

func (d *fakeDB) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	d.h.record(query)
	d.h.mu.Lock()
	err := d.h.err
	d.h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &pgfake.Rows{Cols: d.h.cols, Data: d.h.rows}, nil
}

func (d *fakeDB) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	d.h.record(query)
	d.h.mu.Lock()
	err := d.h.err
	d.h.mu.Unlock()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", len(d.h.rows))), nil
}

func (d *fakeDB) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	d.h.mu.Lock()
	if d.h.txErr != nil {
		err := d.h.txErr
		d.h.mu.Unlock()
		return nil, err
	}
	d.h.lastTxOpts = opts
	d.h.mu.Unlock()
	d.h.record("BEGIN")
	return &pgfake.Tx{
		Statements: d,
		OnCommit: func() error {
			d.h.record("COMMIT")
			d.h.mu.Lock()
			defer d.h.mu.Unlock()
			return d.h.commitErr
		},
		OnRollback: func() error {
			d.h.record("ROLLBACK")
			return nil
		},
	}, nil
}

// normalise makes bound arguments comparable across integer widths, which the
// builder does not narrow.
// ptr is the address of a literal, for the nullable-comparand cases.
func ptr[T any](v T) *T { return &v }

func normalise(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case int:
			out[i] = int64(v)
		case int32:
			out[i] = int64(v)
		default:
			out[i] = a
		}
	}
	return out
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A table name naming a Postgres schema must render as two identifiers.
// Quoting it as one makes Postgres look for a table literally called
// "billing.invoices", which fails a long way from its cause.
func TestQualifiedTableNames(t *testing.T) {
	type Invoice struct {
		ID     string `db:"id" sqlb:"pk"`
		Amount int64  `db:"amount"`
	}
	sqlb.Describe[Invoice]().Table("billing.invoices")

	sel, _, err := sqlb.Query[Invoice]().Select(sqlb.F("id")).
		Where(sqlb.F("amount").Gt(0)).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if want := `SELECT "id" FROM "billing"."invoices" WHERE "amount" > $1`; sel != want {
		t.Errorf("select\n got: %s\nwant: %s", sel, want)
	}

	del, _, err := sqlb.DeleteRows[Invoice]().Where(sqlb.F("id").Eq("i1")).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if want := `DELETE FROM "billing"."invoices" WHERE "id" = $1`; del != want {
		t.Errorf("delete\n got: %s\nwant: %s", del, want)
	}

	// An unqualified name must keep rendering as exactly one identifier.
	type Plain struct {
		ID string `db:"id" sqlb:"pk"`
	}
	plain, _, _ := sqlb.Query[Plain]().Select(sqlb.F("id")).SQL()
	if want := `SELECT "id" FROM "plains"`; plain != want {
		t.Errorf("unqualified\n got: %s\nwant: %s", plain, want)
	}
}

// dialect overriding is per statement, not global. There is deliberately no
// package-level setter: a mutable global read on every query's compile path
// would be a data race with no legitimate trigger.
type ansiDialect struct{}

func (ansiDialect) Placeholder(int) string     { return "?" }
func (ansiDialect) Name() string               { return "ansi" }
func (ansiDialect) QuoteIdent(s string) string { return "`" + s + "`" }

func TestDialectIsOverriddenPerStatement(t *testing.T) {
	q := sqlb.Query[User]().Select(sqlb.F("id")).Where(sqlb.F("age").Gte(18))

	def, _, err := q.Clone().SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if def != `SELECT "id" FROM "users" WHERE "age" >= $1` {
		t.Errorf("default dialect: %s", def)
	}

	alt, _, err := q.Clone().UseDialect(ansiDialect{}).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if alt != "SELECT `id` FROM `users` WHERE `age` >= ?" {
		t.Errorf("overridden dialect: %s", alt)
	}

	// The override must not leak into any other statement.
	after, _, _ := sqlb.Query[User]().Select(sqlb.F("id")).SQL()
	if after != `SELECT "id" FROM "users"` {
		t.Errorf("a per-statement override leaked globally: %s", after)
	}
}

// Postgres quotes identifiers straight into the compiler's buffer rather than
// through QuoteIdent's return value, which is one allocation per identifier and
// a statement names one per projected column. The optimisation is only sound if
// the two spellings agree, so this asserts they do — including on the embedded
// quote, which is the one input where writeIdent does something other than wrap
// the string, and which no other test in this package supplies.
func TestQuotingAgreesWithQuoteIdent(t *testing.T) {
	// Named through F rather than a model, because a column whose name contains
	// a quote is exactly the hand-written reference QuoteIdent is the backstop
	// for.
	for _, name := range []string{"id", `we"ird`, `""`, "ünïcode"} {
		sql, _, err := sqlb.Query[User]().ClearSelect().Select(sqlb.F(name)).SQL()
		if err != nil {
			t.Fatalf("SQL() for %q: %v", name, err)
		}
		want := "SELECT " + (sqlb.Postgres{}).QuoteIdent(name) + ` FROM "users"`
		if sql != want {
			t.Errorf("identifier %q\n got: %s\nwant: %s", name, sql, want)
		}
	}

	// The other direction: a dialect that does not implement the fast path must
	// still go through QuoteIdent. ansiDialect is one, so a change that made the
	// fast path unconditional fails here.
	alt, _, err := sqlb.Query[User]().ClearSelect().Select(sqlb.F(`we"ird`)).
		UseDialect(ansiDialect{}).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if want := "SELECT `we\"ird` FROM `users`"; alt != want {
		t.Errorf("dialect without the fast path\n got: %s\nwant: %s", alt, want)
	}
}

// A field with no matching result column would scan as its zero value, which
// is indistinguishable from a real zero: a mistyped alias on a Sum silently
// reports 0 revenue. Collect must refuse rather than return a wrong number.
func TestCollectRejectsUnmatchedFields(t *testing.T) {
	type Revenue struct {
		Status string  `db:"status"`
		Total  float64 `db:"revenue"`
	}
	// The query aliases "revenu" — one character off.
	h := newHarness(t, []string{"status", "revenu"}, [][]any{{"published", 1234.5}})
	defer h.close()

	_, err := sqlb.Collect[Revenue](context.Background(), h.db,
		sqlb.Query[User]().Select(sqlb.F("status"), sqlb.Sum(sqlb.F("total")).As("revenu")).
			GroupBy(sqlb.F("status")))
	if err == nil {
		t.Fatal("a mistyped alias must not scan as a silent zero")
	}
	for _, want := range []string{"Total", "revenue", "revenu"} {
		if !contains(err.Error(), want) {
			t.Errorf("error should name the field and both column names, got: %v", err)
		}
	}
}

func TestCollectAcceptsAnExactMatch(t *testing.T) {
	type Revenue struct {
		Status string  `db:"status"`
		Total  float64 `db:"revenue"`
	}
	h := newHarness(t, []string{"status", "revenue"}, [][]any{{"published", 1234.5}})
	defer h.close()

	rows, err := sqlb.Collect[Revenue](context.Background(), h.db,
		sqlb.Query[User]().Select(sqlb.F("status"), sqlb.Sum(sqlb.F("total")).As("revenue")))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 1 || rows[0].Total != 1234.5 {
		t.Fatalf("rows = %#v", rows)
	}
}

// All stays permissive: a projection legitimately leaves fields unfilled, which
// is what ?select=id,name is.
func TestAllToleratesPartialProjection(t *testing.T) {
	h := newHarness(t, []string{"id", "name"}, [][]any{{"u1", "Ada"}})
	defer h.close()

	users, err := sqlb.Query[User]().Select(sqlb.F("id"), sqlb.F("name")).All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 || users[0].Name != "Ada" || users[0].Email != "" {
		t.Fatalf("rows = %#v", users)
	}
}

// Describing a model after a statement has been built against it would race
// against every in-flight query and half-apply. It must refuse.
func TestDescribeAfterUsePanics(t *testing.T) {
	type Late struct {
		ID   string `db:"id" sqlb:"pk"`
		Name string `db:"name"`
	}
	_ = sqlb.Query[Late]() // closes the model

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("describing a model already in use should panic")
		}
		msg, _ := r.(string)
		if !contains(msg, "initialisation") {
			t.Errorf("the panic should say when Describe is safe: %v", r)
		}
	}()
	sqlb.Describe[Late]().Filterable("name")
}

// The engine's cap and the schema package's have to be the same number.
//
// They are declared twice on purpose: schema is a leaf package that publishes
// the cap into the manifest and the generated tags, and the engine applies it
// to a model that may have come from Describe with no schema package in sight.
// Two constants that must agree is exactly the arrangement that drifts, so the
// agreement is asserted rather than assumed. ADR-0022.
func TestTheDefaultExpansionCapAgreesWithTheSchemaPackage(t *testing.T) {
	type child struct {
		ID       string `db:"id" sqlb:"pk"`
		ParentID string `db:"parent_id"`
	}
	type parent struct {
		ID       string                  `db:"id" sqlb:"pk"`
		Children *sqlb.Collection[child] `db:"-" json:"children" sqlb:"expands=parent_id"`
	}

	rel := sqlb.ModelOf[parent]().Relation("children")
	if rel == nil {
		t.Fatal("the collection did not become a relation")
	}
	if rel.Cap() != schema.DefaultExpandLimit {
		t.Errorf("the engine caps an undeclared expansion at %d, the schema package publishes %d",
			rel.Cap(), schema.DefaultExpandLimit)
	}
}
