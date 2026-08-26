package sqlb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sql.Scanner and driver.Valuer are still the interfaces a type implements to
// say it encodes itself, because pgx honours both as its last-resort plan and
// pgtype implements them throughout. They are the only two names database/sql
// keeps here (ADR-0040).
var (
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
	valuerType  = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
)

// Executor is the subset of pgx that sqlb runs statements through. *pgxpool.Pool,
// *pgx.Conn and pgx.Tx all satisfy it as they stand, as does any instrumenting
// wrapper over them.
//
// Taking pgx rather than database/sql is [ADR-0040], and the thing it buys that
// an abstraction could not is that a caller's own pgx.Tx *is* an Executor: sqlb
// writes join a unit of work the application already opened, rather than
// opening a second transaction against the same pool.
//
// [ADR-0040]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#the-driver-is-a-dependency
type Executor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// rowSource is the result set every scanner in this package reads. Scanning is
// Columns once, then Next/Scan per row, then Err — five methods, and naming
// them as an interface is what made the driver flip an adapter rather than a
// rewrite of scan, mutate and their type-mapping tests.
//
// It is now a test seam rather than a driver seam (ADR-0040): there is one
// driver, and the only other implementation is the fake the engine's own suite
// scans, which is how those tests run without a database. It stays unexported
// for that reason — the public contract is Executor.
type rowSource interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// pgxRows adapts a pgx result set onto rowSource. Exactly one method differs:
// pgx reports its columns as field descriptions.
//
// Close is pgx's, and returns nothing. That shape is deliberate rather than
// inherited — a Close that reported an error would be one every caller here
// defers and therefore ignores, and the failure is not lost either way: pgx
// records it on the result set and every scanner ends by reading Err.
type pgxRows struct{ pgx.Rows }

func (r pgxRows) Columns() ([]string, error) {
	fields := r.FieldDescriptions()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = f.Name
	}
	return cols, nil
}

// runQuery runs a statement and hands back its rows already adapted, which is
// the one place the driver's shape meets the scanners'. Named for the verb
// rather than the noun because every caller has a local `query` holding the SQL.
func runQuery(ctx context.Context, db Executor, query string, args ...any) (rowSource, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapQueryErr(err, query)
	}
	return pgxRows{rows}, nil
}

// Resolved returns a copy of the query with everything the exec path adds to it
// before the wire: the BeforeQuery hooks registered for T against db, and the
// scopes of any relation this query expands.
//
// It is the statement that will actually run, which [Builder.SQL] on its own is
// not. `SQL()` renders what the caller built, and for a model whose reads are
// confined by a hook that is a statement with the confinement missing — so a
// reader checking that the tenant predicate really is on every read by printing
// `SQL()` concludes that it is not, and raw SQL assembled alongside a rendered
// predicate silently counts rows the query would never have returned (#153).
//
//	q, err := sqlb.Query[Post]().Where(…).Resolved(ctx, db)
//	sql, args, err := q.SQL()   // WHERE status = $1 AND org_id = $2
//
// This is the only supported way to read the hooks' effect as text, and it is
// what [Explain] uses. Applying them by hand at a second call site is the
// duplication the hook exists to remove — and the copy it duplicates is a
// security predicate, which is the worst kind to keep in two places.
//
// The receiver is untouched, as it is on every exec path: hooks amend a clone,
// so resolving the same builder twice does not accumulate their predicates.
func (b *Builder[T]) Resolved(ctx context.Context, db Executor) (*Builder[T], error) {
	q := b.Clone()
	// The nested queries the caller wrote, taken before the hooks can add any.
	// One that appears only afterwards was added by a hook, and the refusal
	// below has to say something different about it: resolving it first is the
	// fix everywhere else and a hook has no executor to do it with (#288).
	var authored subqueryWalk
	q.walkSubqueries(&authored)
	// Same question for the CTE, which arrives by its own clause rather than
	// through the expression walk: With is public on *Builder[T], so a hook can
	// attach one.
	authoredWith := q.withQuery

	if err := hooksFor[T](db).runBeforeQuery(ctx, q, releasedFrom(db)); err != nil {
		return nil, err
	}
	if err := q.resolveExpansionScopes(ctx, db); err != nil {
		return nil, err
	}
	// After the hooks rather than before, because a hook is free to add a
	// predicate carrying a nested query of its own, and one added here is as
	// unresolved as one the caller wrote.
	w := subqueryWalk{
		authored: authored.set(),
		hook:     "BeforeQuery",
		owner:    q.model.Type.Name(),
	}
	q.walkSubqueries(&w)
	if err := w.check(ctx, db); err != nil {
		return nil, err
	}
	// With's query is compiled straight into this statement rather than run,
	// the same reason a nested Subquery is; see guardFrom.
	if q.withQuery != nil {
		if err := guardCTE(ctx, db, cte{
			clause: "With", name: q.withName, query: q.withQuery,
			byHook: q.withQuery != authoredWith,
			hook:   "BeforeQuery", owner: q.model.Type.Name(),
		}); err != nil {
			return nil, err
		}
	}
	q.resolved = true
	return q, nil
}

// resolvedSQL renders the statement that will run, and is what [Explain] calls
// through the resolver interface when it is handed a builder.
func (b *Builder[T]) resolvedSQL(ctx context.Context, db Executor) (string, []any, error) {
	q, err := b.Resolved(ctx, db)
	if err != nil {
		return "", nil, err
	}
	return q.SQL()
}

// All runs the query and returns every matching row.
//
// The builder is cloned first, so query hooks amend a copy and running the same
// builder twice does not accumulate their predicates.
func (b *Builder[T]) All(ctx context.Context, db Executor) ([]T, error) {
	q, err := b.Resolved(ctx, db)
	if err != nil {
		return nil, err
	}
	query, args, err := q.SQL()
	if err != nil {
		return nil, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll[T](rows, b.model)
}

// One runs the query and returns the single matching row. It returns
// ErrNotFound if nothing matched, and an error if more than one row did, since
// a caller asking for one row is asserting that only one exists.
func (b *Builder[T]) One(ctx context.Context, db Executor) (T, error) {
	var zero T
	// Fetching two rows makes the ambiguity detectable without a second query.
	probe := b.Clone().Limit(2)
	rows, err := probe.All(ctx, db)
	if err != nil {
		return zero, err
	}
	switch len(rows) {
	case 0:
		return zero, ErrNotFound
	case 1:
		return rows[0], nil
	default:
		return zero, fmt.Errorf("sqlb: One matched more than one row in %s", b.model.Table)
	}
}

// First returns the first matching row, or ErrNotFound. Unlike One it accepts
// multiple matches, so it should be paired with OrderBy to be deterministic.
func (b *Builder[T]) First(ctx context.Context, db Executor) (T, error) {
	var zero T
	rows, err := b.Clone().Limit(1).All(ctx, db)
	if err != nil {
		return zero, err
	}
	if len(rows) == 0 {
		return zero, ErrNotFound
	}
	return rows[0], nil
}

// Count returns the number of matching rows, ignoring pagination. For a
// grouped query it counts groups.
func (b *Builder[T]) Count(ctx context.Context, db Executor) (int64, error) {
	q, err := b.Resolved(ctx, db)
	if err != nil {
		return 0, err
	}
	query, args, err := q.countSQL()
	if err != nil {
		return 0, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	return scanCount(rows)
}

// scanCount reads the single value a COUNT projection returns.
//
// An empty result set counts zero rather than failing: a grouped count over no
// groups returns no rows at all, and nought is the honest answer to how many
// there were.
func scanCount(rows rowSource) (int64, error) {
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}

// Exists reports whether the query matches at least one row.
func (b *Builder[T]) Exists(ctx context.Context, db Executor) (bool, error) {
	probe := b.Clone()
	probe.sel = []Selection{RawSel("1")}
	probe.orders = nil
	probe.Limit(1)

	query, args, err := probe.resolvedSQL(ctx, db)
	if err != nil {
		return false, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return scanExists(rows)
}

// scanExists reports whether a result set has a first row. The error is read
// after Next, because a Next that returns false may be a failure rather than an
// end.
func scanExists(rows rowSource) (bool, error) {
	found := rows.Next()
	return found, rows.Err()
}

// Collect runs a query and scans its rows into R rather than the model type.
// It is how grouped and aggregated queries are read, where the result shape is
// not the table shape:
//
//	type Revenue struct {
//	    Status string  `db:"status"`
//	    Total  float64 `db:"revenue"`
//	}
//	rows, err := sqlb.Collect[Revenue](ctx, db,
//	    sqlb.Query[Order]().
//	        GroupBy(sqlb.F("status")).
//	        Select(sqlb.F("status"), sqlb.Sum(sqlb.F("total")).As("revenue")))
//
// Query hooks still run, so tenant scoping applies to aggregates too.
func Collect[R, T any](ctx context.Context, db Executor, b *Builder[T]) ([]R, error) {
	query, args, err := b.resolvedSQL(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Exact, unlike All: R was declared specifically to receive this
	// projection, so a field with no matching column is a mistake rather than
	// a deliberate partial select.
	return scan[R](rows, ModelOf[R](), scanExact)
}

// scanMode controls how strictly a result set must match its destination.
type scanMode int

const (
	// scanPartial allows model fields to go unfilled, which is what a
	// projection is: ?select=id,name legitimately leaves the rest zero.
	scanPartial scanMode = iota
	// scanExact requires every model field to be filled by some result
	// column. Used where the destination type was written to match the
	// projection, so an unfilled field means a mismatch rather than an
	// intention.
	scanExact
)

// scanAll maps a result set onto a slice of T, tolerating unfilled fields.
func scanAll[T any](rows rowSource, m *Model) ([]T, error) {
	return scan[T](rows, m, scanPartial)
}

// scan maps a result set onto a slice of T. Result columns with no matching
// model field are read and discarded, so a query selecting extra expressions
// still scans.
func scan[T any](rows rowSource, m *Model, mode scanMode) ([]T, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	// No columns is how a failure the driver deferred arrives here. pgx sends
	// the statement and returns before the server has answered, so a rejected
	// write — a unique violation, a parameter count over the protocol limit —
	// comes back as a result set describing nothing, and the error only
	// appears once the iteration has been driven. Driving it here means the
	// caller sees "extended protocol limited to 65535 parameters" rather than
	// a complaint about their db tags.
	//
	// A statement always projects at least one column, so there is no honest
	// empty case this could be mistaking for a failure.
	if len(cols) == 0 {
		rows.Next()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	targets := make([][]int, len(cols))
	// expansions maps a result column onto the relation it carries. An
	// expanded relation arrives as one JSON value rather than as columns, so
	// it is scanned into []byte here and decoded per row below.
	var expansions map[int]*RelationInfo
	matched := 0
	for i, name := range cols {
		if ci := m.Column(name); ci != nil {
			targets[i] = ci.Index
			matched++
			continue
		}
		if rel, found := strings.CutPrefix(name, expandPrefix); found {
			if info := m.Relation(rel); info != nil {
				if expansions == nil {
					expansions = make(map[int]*RelationInfo, 1)
				}
				expansions[i] = info
				matched++
			}
		}
	}
	if matched == 0 {
		return nil, fmt.Errorf("sqlb: none of the result columns %v map to %s; check the db tags or the Select aliases", cols, m.Type)
	}

	// A field left unfilled would scan as its zero value, which is
	// indistinguishable from a real zero — a mistyped alias on a Sum would
	// silently report 0 revenue rather than failing. Name the offenders.
	if mode == scanExact && matched < len(m.Columns) {
		filled := make(map[string]bool, matched)
		for i, name := range cols {
			if targets[i] != nil {
				filled[name] = true
			}
		}
		// An expanded relation fills no column, so it neither satisfies nor
		// violates the exact-scan requirement.
		_ = expansions
		var missing []string
		for _, col := range m.Columns {
			if !filled[col.Name] {
				missing = append(missing, fmt.Sprintf("%s (db:%q)", col.Field, col.Name))
			}
		}
		return nil, fmt.Errorf(
			"sqlb: %s has no result column for %s; the query returned %v — check the Select aliases match the db tags",
			m.Type, strings.Join(missing, ", "), cols)
	}

	var (
		out     []T
		dest    = make([]any, len(cols))
		discard = make([]any, len(cols))
	)
	for rows.Next() {
		var row T
		rv := reflect.ValueOf(&row).Elem()
		raws := make(map[int]*[]byte, len(expansions))
		for i := range cols {
			if rel, isExpansion := expansions[i]; isExpansion {
				_ = rel
				raw := new([]byte)
				raws[i] = raw
				dest[i] = raw
				continue
			}
			if targets[i] == nil {
				if discard[i] == nil {
					discard[i] = new(any)
				}
				dest[i] = discard[i]
				continue
			}
			field, err := fieldByIndex(rv, targets[i])
			if err != nil {
				return nil, err
			}
			// An array column needs nothing special here. It did under
			// database/sql, which has no array case in either direction and
			// cost this package a 449-line literal codec; pgx decodes one into
			// the slice field's own address (ADR-0033, ADR-0040).
			dest[i] = field.Addr().Interface()
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("sqlb: scanning %s: %w", m.Type, err)
		}
		for i, raw := range raws {
			if err := scanExpansion(rv, expansions[i], *raw); err != nil {
				return nil, err
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// fieldByIndex walks an index path, allocating nil embedded pointers on the
// way so that mixins reached through a pointer are scannable.
func fieldByIndex(v reflect.Value, index []int) (reflect.Value, error) {
	for i, x := range index {
		if i > 0 {
			if v.Kind() == reflect.Pointer {
				if v.IsNil() {
					if !v.CanSet() {
						return reflect.Value{}, fmt.Errorf("sqlb: cannot allocate embedded field at index %v", index)
					}
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
		}
		v = v.Field(x)
	}
	return v, nil
}

// wrapQueryErr attaches the failing SQL to a driver error. Without it a
// Postgres syntax or type error names a column but not the statement, which is
// unhelpful when the statement was assembled from a filter expression.
//
// A constraint violation is additionally classified, so a caller can tell its
// own mistake from an outage with errors.Is rather than by reading the message
// this builds. The statement stays in the wrapped error either way: it is what
// a log needs, and it is exactly what a response must not carry.
func wrapQueryErr(err error, query string) error {
	wrapped := fmt.Errorf("sqlb: executing %s: %w", truncate(query, 400), err)
	if ce, ok := classifyConstraint(err); ok {
		ce.err = wrapped
		return ce
	}
	return wrapped
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
