package sqlb_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// keyless has no primary key, which is the case Stable has to tolerate and the
// cursor calls have to refuse.
type keyless struct {
	Name  string `db:"name" sqlb:"sort"`
	Score int32  `db:"score" sqlb:"sort"`
}

func (keyless) TableName() string { return "keyless" }

// cursorAt builds the cursor a row at the given position would carry, by
// running CursorFor over a query ordered the same way the test orders it. It
// exists so the tests exercise the encode and decode halves against each other
// rather than against a golden string, which would only prove the encoder
// agrees with itself.
func cursorAt(t *testing.T, order []sqlb.Order, row User) sqlb.Cursor {
	t.Helper()
	c, err := sqlb.Query[User]().OrderBy(order...).CursorFor(row)
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}
	return c
}

func TestStableAppendsThePrimaryKey(t *testing.T) {
	tests := []struct {
		name  string
		order []sqlb.Order
		want  string
	}{
		{
			name:  "ascending sort takes an ascending tiebreaker",
			order: []sqlb.Order{sqlb.F("name").Asc()},
			want:  `ORDER BY "name" ASC, "id" ASC`,
		},
		{
			name:  "descending sort takes a descending one, so the ordering reads one way throughout",
			order: []sqlb.Order{sqlb.F("created_at").Desc()},
			want:  `ORDER BY "created_at" DESC, "id" DESC`,
		},
		{
			name:  "the direction comes from the last term, not the first",
			order: []sqlb.Order{sqlb.F("name").Asc(), sqlb.F("created_at").Desc()},
			want:  `ORDER BY "name" ASC, "created_at" DESC, "id" DESC`,
		},
		{
			name:  "an ordering that already names the key is left alone",
			order: []sqlb.Order{sqlb.F("id").Desc()},
			want:  `ORDER BY "id" DESC`,
		},
		{
			name:  "the key is not appended twice when it is not last either",
			order: []sqlb.Order{sqlb.F("id").Asc(), sqlb.F("name").Desc()},
			want:  `ORDER BY "id" ASC, "name" DESC`,
		},
		{
			name:  "an unordered query still gets a total order",
			order: nil,
			want:  `ORDER BY "id" ASC`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := sqlb.Query[User]().Select(sqlb.F("id")).OrderBy(tc.order...).Stable().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tc.want) {
				t.Errorf("SQL = %s\nwant it to contain %s", sql, tc.want)
			}
		})
	}
}

func TestStableIsIdempotent(t *testing.T) {
	once, _, err := sqlb.Query[User]().Select(sqlb.F("id")).OrderBy(sqlb.F("name").Asc()).Stable().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	twice, _, err := sqlb.Query[User]().Select(sqlb.F("id")).OrderBy(sqlb.F("name").Asc()).
		Stable().Stable().Stable().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if once != twice {
		t.Errorf("Stable is not idempotent:\n once = %s\ntwice = %s", once, twice)
	}
}

// A model with no key can still be listed, so Stable leaves it alone rather
// than failing every query that passes through filter.Apply.
func TestStableWithoutAPrimaryKeyIsANoOp(t *testing.T) {
	sql, _, err := sqlb.Query[keyless]().Select(sqlb.F("name")).OrderBy(sqlb.F("name").Asc()).Stable().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "name" ASC`) {
		t.Errorf("SQL = %s, want the ordering untouched", sql)
	}
}

// The cursor calls are where the missing key genuinely bites, so that is where
// it is reported — and the message says what to do instead.
func TestCursorWithoutAPrimaryKeyIsRefused(t *testing.T) {
	_, err := sqlb.Query[keyless]().OrderBy(sqlb.F("name").Asc()).CursorFor(keyless{Name: "a"})
	if err == nil {
		t.Fatal("CursorFor on a keyless model should fail")
	}
	for _, want := range []string{"primary key", "Limit and Offset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestSeekPredicateShapes(t *testing.T) {
	when := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	age := int32(30)

	tests := []struct {
		name  string
		order []sqlb.Order
		row   User
		want  string
		args  []any
	}{
		{
			name:  "one non-null column and the key take the row constructor, which Postgres can seek on",
			order: []sqlb.Order{sqlb.F("created_at").Asc()},
			row:   User{ID: "u2", CreatedAt: when},
			want:  `WHERE ("created_at", "id") > ($1, $2)`,
			args:  []any{when, "u2"},
		},
		{
			name:  "descending flips the row comparison rather than the terms",
			order: []sqlb.Order{sqlb.F("created_at").Desc()},
			row:   User{ID: "u2", CreatedAt: when},
			want:  `WHERE ("created_at", "id") < ($1, $2)`,
			args:  []any{when, "u2"},
		},
		{
			name:  "mixed directions cannot be one comparison, so the lexicographic form is used",
			order: []sqlb.Order{sqlb.F("name").Asc(), sqlb.F("created_at").Desc()},
			row:   User{ID: "u2", Name: "Ada", CreatedAt: when},
			want: `WHERE (("name" > $1) ` +
				`OR (("name" = $2) AND ("created_at" < $3))) ` +
				`OR ((("name" = $4) AND ("created_at" = $5)) AND ("id" < $6))`,
			args: []any{"Ada", "Ada", when, "Ada", when, "u2"},
		},
		{
			name:  "sorting by the key alone needs no tiebreaker and no expansion",
			order: []sqlb.Order{sqlb.F("id").Asc()},
			row:   User{ID: "u2"},
			want:  `WHERE "id" > $1`,
			args:  []any{"u2"},
		},
		{
			name: "a nullable column keeps the expanded form even when this boundary is not null, " +
				"because the rows being compared may still hold NULLs",
			order: []sqlb.Order{sqlb.F("age").Asc()},
			row:   User{ID: "u2", Age: &age},
			want: `WHERE (("age" > $1) OR ("age" IS NULL)) ` +
				`OR (("age" = $2) AND ("id" > $3))`,
			args: []any{age, age, "u2"},
		},
		{
			name:  "descending puts NULLs first, so a real boundary leaves them behind",
			order: []sqlb.Order{sqlb.F("age").Desc()},
			row:   User{ID: "u2", Age: &age},
			want:  `WHERE ("age" < $1) OR (("age" = $2) AND ("id" < $3))`,
			args:  []any{age, age, "u2"},
		},
		{
			name:  "a NULL boundary under NULLS LAST has nothing after it but the tiebreaker",
			order: []sqlb.Order{sqlb.F("age").Asc()},
			row:   User{ID: "u2", Age: nil},
			want:  `WHERE ("age" IS NULL) AND ("id" > $1)`,
			args:  []any{"u2"},
		},
		{
			name:  "a NULL boundary under NULLS FIRST is followed by every real value",
			order: []sqlb.Order{sqlb.F("age").Desc()},
			row:   User{ID: "u2", Age: nil},
			want:  `WHERE ("age" IS NOT NULL) OR (("age" IS NULL) AND ("id" < $1))`,
			args:  []any{"u2"},
		},
		{
			name:  "an explicit NULLS LAST on a descending sort is honoured over the Postgres default",
			order: []sqlb.Order{sqlb.F("age").Desc().NullsLast()},
			row:   User{ID: "u2", Age: &age},
			want: `WHERE (("age" < $1) OR ("age" IS NULL)) ` +
				`OR (("age" = $2) AND ("id" < $3))`,
			args: []any{age, age, "u2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cur := cursorAt(t, tc.order, tc.row)

			sql, args, err := sqlb.Query[User]().Select(sqlb.F("id")).
				OrderBy(tc.order...).After(cur).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tc.want) {
				t.Errorf("SQL = %s\nwant it to contain %s", sql, tc.want)
			}
			if len(args) != len(tc.args) {
				t.Fatalf("args = %v, want %v", args, tc.args)
			}
			for i := range args {
				if args[i] != tc.args[i] {
					t.Errorf("arg %d = %v, want %v", i, args[i], tc.args[i])
				}
			}
		})
	}
}

// The boundary belongs to pagination, not to selection, so a cursor never
// changes which rows the query is about — only where reading starts.
func TestSeekIsAppliedAfterTheCallersOwnPredicates(t *testing.T) {
	cur := cursorAt(t, []sqlb.Order{sqlb.F("created_at").Desc()}, User{ID: "u2"})

	sql, _, err := sqlb.Query[User]().Select(sqlb.F("id")).
		Where(sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("created_at").Desc()).After(cur).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `WHERE ("org_id" = $1) AND (("created_at", "id") < ($2, $3))`) {
		t.Errorf("SQL = %s", sql)
	}
}

// ?count=exact answers "how big is this result set", which must not shrink as
// a client pages through it.
func TestCountIgnoresTheCursor(t *testing.T) {
	h := newHarness(t, []string{"count"}, [][]any{{int64(42)}})
	defer h.close()

	cur := cursorAt(t, []sqlb.Order{sqlb.F("created_at").Desc()}, User{ID: "u2"})
	q := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("created_at").Desc()).After(cur)

	n, err := q.Count(context.Background(), h.db)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}
	got := h.lastQuery()
	if strings.Contains(got, "created_at") {
		t.Errorf("count query carried the cursor boundary: %s", got)
	}
	if !strings.Contains(got, `"org_id" = $1`) {
		t.Errorf("count query dropped the caller's filter: %s", got)
	}
}

func TestZeroCursorIsANoOp(t *testing.T) {
	withCursor, _, err := sqlb.Query[User]().Select(sqlb.F("id")).
		OrderBy(sqlb.F("name").Asc()).After("").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(withCursor, "WHERE") {
		t.Errorf("an empty cursor added a boundary: %s", withCursor)
	}
	// It still normalises the ordering, so the first page and the second are
	// ordered the same way — which is what makes the cursor it issues valid.
	if !strings.Contains(withCursor, `ORDER BY "name" ASC, "id" ASC`) {
		t.Errorf("SQL = %s, want a total order", withCursor)
	}
}

func TestCursorRejections(t *testing.T) {
	valid := cursorAt(t, []sqlb.Order{sqlb.F("name").Asc()}, User{ID: "u2", Name: "Ada"})

	tests := []struct {
		name   string
		order  []sqlb.Order
		cursor sqlb.Cursor
		want   []string
	}{
		{
			name:   "not base64",
			order:  []sqlb.Order{sqlb.F("name").Asc()},
			cursor: "not a cursor!!",
			want:   []string{"not decodable", "drop the cursor"},
		},
		{
			name:   "base64 of something that is not a cursor",
			order:  []sqlb.Order{sqlb.F("name").Asc()},
			cursor: sqlb.Cursor(base64.RawURLEncoding.EncodeToString([]byte("hello"))),
			want:   []string{"not decodable"},
		},
		{
			name:   "issued for a different column",
			order:  []sqlb.Order{sqlb.F("created_at").Asc()},
			cursor: valid,
			want:   []string{"name asc, id asc", "created_at asc, id asc", "drop the cursor when the sort changes"},
		},
		{
			name:   "issued for the same column in the other direction",
			order:  []sqlb.Order{sqlb.F("name").Desc()},
			cursor: valid,
			want:   []string{"name asc", "name desc"},
		},
		{
			name:   "issued for a longer ordering",
			order:  []sqlb.Order{sqlb.F("name").Asc(), sqlb.F("created_at").Asc()},
			cursor: valid,
			want:   []string{"name asc, id asc", "name asc, created_at asc, id asc"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sqlb.Query[User]().OrderBy(tc.order...).After(tc.cursor).SQL()
			if err == nil {
				t.Fatal("expected the cursor to be refused")
			}
			if !errors.Is(err, sqlb.ErrBadCursor) {
				t.Errorf("error %v does not wrap ErrBadCursor, so REST cannot map it to 400", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// A cursor is opaque by convention, so a client can edit one. What stops that
// mattering is that the ordering is checked, not that the payload is secret —
// so a value of the wrong shape for its column is refused rather than bound.
func TestTamperedCursorValueIsRefused(t *testing.T) {
	forged := func(t *testing.T, payload string) sqlb.Cursor {
		t.Helper()
		return sqlb.Cursor(base64.RawURLEncoding.EncodeToString([]byte(payload)))
	}

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "a string where the column holds a timestamp",
			payload: `{"k":[{"c":"created_at","v":"tomorrow"},{"c":"id","v":"u2"}]}`,
			want:    "not a time.Time",
		},
		{
			name:    "null for a column that cannot hold one",
			payload: `{"k":[{"c":"created_at","v":null},{"c":"id","v":"u2"}]}`,
			want:    "not nullable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sqlb.Query[User]().OrderBy(sqlb.F("created_at").Asc()).
				After(forged(t, tc.payload)).SQL()
			if err == nil {
				t.Fatal("expected the forged cursor to be refused")
			}
			if !errors.Is(err, sqlb.ErrBadCursor) {
				t.Errorf("error %v does not wrap ErrBadCursor", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Editing a cursor's *values* is allowed, and uninteresting: it moves the
// boundary along a column the caller was already permitted to sort by, which is
// something ?created_at=gt. would have done anyway.
func TestEditingACursorValueOnlyMovesTheBoundary(t *testing.T) {
	when := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	edited := sqlb.Cursor(base64.RawURLEncoding.EncodeToString([]byte(
		`{"k":[{"c":"created_at","v":"2026-07-27T10:00:00Z"},{"c":"id","v":"anything"}]}`)))

	_, args, err := sqlb.Query[User]().Select(sqlb.F("id")).
		OrderBy(sqlb.F("created_at").Asc()).After(edited).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 2 || args[0] != when || args[1] != "anything" {
		t.Errorf("args = %v, want the edited position bound as parameters", args)
	}
}

// A cursor's value for a column is the same JSON the API showed for it, which
// is what lets a client reason about one it has decoded.
func TestCursorEncodesTheColumnsOwnJSON(t *testing.T) {
	when := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	cur := cursorAt(t, []sqlb.Order{sqlb.F("created_at").Desc()}, User{ID: "u7", CreatedAt: when})

	buf, err := base64.RawURLEncoding.DecodeString(string(cur))
	if err != nil {
		t.Fatalf("a cursor should be base64url: %v", err)
	}
	var got struct {
		Terms []struct {
			Column string          `json:"c"`
			Desc   bool            `json:"d"`
			Value  json.RawMessage `json:"v"`
		} `json:"k"`
	}
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("a cursor should be JSON: %v", err)
	}
	if len(got.Terms) != 2 {
		t.Fatalf("cursor carries %d terms, want the sort column and the key", len(got.Terms))
	}
	if got.Terms[0].Column != "created_at" || !got.Terms[0].Desc {
		t.Errorf("first term = %+v, want created_at desc", got.Terms[0])
	}
	if string(got.Terms[0].Value) != `"2026-07-27T10:00:00Z"` {
		t.Errorf("timestamp encoded as %s, want the RFC 3339 form the API emits", got.Terms[0].Value)
	}
	if string(got.Terms[1].Value) != `"u7"` {
		t.Errorf("key encoded as %s, want \"u7\"", got.Terms[1].Value)
	}
}

// An ordering a cursor cannot describe is one the caller assembled in Go, so it
// is a programming error rather than a bad request — and the message says which
// term is the problem.
func TestCursorOverAnExpressionOrderingIsRefused(t *testing.T) {
	_, err := sqlb.Query[User]().
		OrderBy(sqlb.OrderBy(sqlb.Raw{SQL: `lower("name")`})).
		CursorFor(User{ID: "u1"})
	if err == nil {
		t.Fatal("expected an expression ordering to be refused")
	}
	if errors.Is(err, sqlb.ErrBadCursor) {
		t.Error("this is the caller's mistake, not the client's, so it should not read as a bad cursor")
	}
	if !strings.Contains(err.Error(), "orders by columns only") {
		t.Errorf("error = %q", err)
	}
}

// A joined column that happens to share the primary key's bare name is the
// hand-written-join footgun the cursor machinery used to walk into: `o.id` is
// not `users.id`, and matching on the name alone made Stable believe the
// ordering was already total, made CursorFor encode the *base* row's id as the
// position on the join's column, and made After seek `"o"."id" < $1` with that
// wrong value. Silently wrong pages, which is the single failure keyset paging
// exists to prevent (#72).
func TestCursorOverAJoinedColumnIsRefused(t *testing.T) {
	// Stable must not believe the key is present: `o.id` is a different column.
	sql, _, err := sqlb.Query[User]().
		Join("orgs", "o", sqlb.F("o.id").EqField(sqlb.F("users.org_id"))).
		OrderBy(sqlb.F("o.id").Desc()).
		Stable().
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "o"."id" DESC, "users"."id" DESC`) {
		t.Errorf("Stable did not add the tiebreaker, so the ordering is not total:\n%s", sql)
	}

	// And a cursor over that ordering is refused rather than encoding the base
	// row's value as a position on the joined column.
	_, err = sqlb.Query[User]().
		Join("orgs", "o", sqlb.F("o.id").EqField(sqlb.F("users.org_id"))).
		OrderBy(sqlb.F("o.id").Desc()).
		CursorFor(User{ID: "u1"})
	if err == nil {
		t.Fatal("a cursor over a joined column was accepted; it encodes the base row's " +
			"value as the position on another table's column")
	}
	if errors.Is(err, sqlb.ErrBadCursor) {
		t.Error("this is the caller's mistake, not the client's, so it should not read as a bad cursor")
	}
	for _, want := range []string{`"o"."id"`, "users"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name the term and the table that would work, got: %v", err)
		}
	}
}

// The other direction, so the refusal above is not a ban on qualifying at all:
// the base table's own name is how a caller writing a join spells its side, and
// it still resolves.
func TestCursorOverTheBaseTableQualifiedIsAccepted(t *testing.T) {
	c, err := sqlb.Query[User]().
		Join("orgs", "o", sqlb.F("o.id").EqField(sqlb.F("users.org_id"))).
		OrderBy(sqlb.F("users.name").Asc()).
		CursorFor(User{ID: "u1", Name: "Ada"})
	if err != nil {
		t.Fatalf("CursorFor over the base table's own qualifier: %v", err)
	}
	if c == "" {
		t.Error("CursorFor returned an empty cursor")
	}
}

// Paging to the end and asking for one more page should return nothing, not
// everything — the zero Pred that Where would skip is the trap here.
func TestExhaustedOrderingSeeksNothing(t *testing.T) {
	// age ascending puts NULLs last, and the key is forced to null by a forged
	// cursor, so no disjunct is satisfiable.
	forged := sqlb.Cursor(base64.RawURLEncoding.EncodeToString([]byte(
		`{"k":[{"c":"age","v":null}]}`)))

	sql, _, err := sqlb.Query[keylessNullable]().Select(sqlb.F("age")).
		OrderBy(sqlb.F("age").Asc()).After(forged).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "WHERE false") {
		t.Errorf("SQL = %s, want an unsatisfiable boundary rather than no boundary", sql)
	}
}

// keylessNullable orders on a nullable column and declares a key, so the
// "nothing is after this position" branch can be reached without the key's own
// disjunct rescuing it.
type keylessNullable struct {
	Age *int32 `db:"age" sqlb:"pk,sort"`
}

func (keylessNullable) TableName() string { return "nullable_keys" }
