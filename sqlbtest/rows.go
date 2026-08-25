package sqlbtest

import (
	"reflect"

	"github.com/jryannel/sqlb"
)

// Rows scripts a reply out of model values, so a test says what the database
// holds rather than how the driver spells it.
//
//	db := sqlbtest.New(sqlbtest.Rows(
//	    Note{ID: "n1", SpaceID: "acme", Title: "Hello"},
//	))
//
// The columns are T's, in the declaration order the default projection compiles
// — and the values are read off each struct through the field index the model
// already carries, so the two cannot disagree. That is the half a hand-written
// [Reply] gets wrong: Cols and Rows are two literals that have to agree on
// order, and when they do not the failure surfaces several frames later as a
// scan error about db tags rather than at the line that wrote them.
//
// A [Reply] is still the thing to write when the question is about the shape of
// the result rather than about rows of a model — a count, a Select of two
// aliased expressions, a column the model does not map. Rows is the common case
// made typed, not a replacement.
//
// # What it answers, and what it does not
//
// Computed columns are left out, because the default projection leaves them out
// (a correlated subquery declared for a list screen is not attached to every
// read of the model). A test that asks for one with WithComputed scripts it as
// a [Reply]: whether a query wanted a computed column is a per-query decision,
// and the double never sees the query until it arrives.
//
// Hidden and WriteOnly columns *are* answered, because the engine's default
// projection selects them — those two are REST response rules, applied where a
// resource is bound rather than where a statement is compiled. A test asserting
// that a hidden column stayed out of a projection is asking about the SQL, and
// reads [DB.LastStatement].
func Rows[T any](vals ...T) Reply {
	m := sqlb.ModelOf[T]()
	cols := make([]*sqlb.ColumnInfo, 0, len(m.Columns))
	names := make([]string, 0, len(m.Columns))
	for _, col := range m.Columns {
		if col.Computed() {
			continue
		}
		cols = append(cols, col)
		names = append(names, col.Name)
	}
	out := Reply{Cols: names, Rows: make([][]any, 0, len(vals))}
	for _, v := range vals {
		out.Rows = append(out.Rows, rowOf(reflect.ValueOf(v), cols))
	}
	return out
}

// rowOf reads one model value into the column order [Rows] established. A
// column it cannot read is left nil, which the canned result set sends as NULL:
// there is no honest error here, because every case that reaches it — a nil
// value, a nil embedded pointer — is a struct that genuinely has nothing at
// that column.
func rowOf(rv reflect.Value, cols []*sqlb.ColumnInfo) []any {
	row := make([]any, len(cols))
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return row
		}
		rv = rv.Elem()
	}
	for i, col := range cols {
		f, err := rv.FieldByIndexErr(col.Index)
		if err != nil {
			continue // a nil embedded pointer: every column behind it is NULL
		}
		// A nil pointer field is left as an untyped nil rather than a typed
		// one. Both scan to the same zero, so this is not about the round
		// trip; it is about [Reply] being a value a test may read back. A
		// script written by hand spells a NULL `nil`, and a Reply built here
		// should hold what one written there would.
		if f.Kind() == reflect.Pointer && f.IsNil() {
			continue
		}
		row[i] = f.Interface()
	}
	return row
}

// Count answers the count statement a paged read issues, which is the other
// half of nearly every paging test and the half with a trap in it: the value
// has to arrive as an int64, because that is what Postgres sends and what the
// scalar scanner narrows from, and a plain 42 fails at scan time.
//
// Its Match selects the count statement from the page statement in a script
// holding both, so it goes first — replies are tried in order, and the page
// query's is usually a catch-all:
//
//	db := sqlbtest.New(
//	    sqlbtest.Count(42),
//	    sqlbtest.Rows(notes...),
//	)
func Count(n int) Reply {
	return Reply{Match: "count(", Cols: []string{"count"}, Rows: [][]any{{int64(n)}}}
}

// Matching narrows a reply to statements containing sub, for a script holding
// more than one. It returns a copy, so it composes with the constructors above
// rather than needing a variable to reach the field:
//
//	sqlbtest.Rows(archived...).Matching(`"archived_at" IS NOT NULL`)
func (r Reply) Matching(sub string) Reply {
	r.Match = sub
	return r
}
