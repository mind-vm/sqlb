package introspect

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/schema"
)

// Options control what is read and how it is named.
type Options struct {
	// Schema is the Postgres schema to read. Defaults to "public".
	Schema string

	// Module, when set, produces a module registry (schema.NewModule) and
	// strips the module's prefix from every table name it finds, so that a
	// database whose tables are called billing_invoices imports as a module
	// named billing holding a table called invoices.
	//
	// Tables without the prefix are left alone and reported, since a module
	// registry would silently rename them on the way back out.
	Module string

	// Only limits the import to the named tables. Empty reads everything,
	// which is what an import of a database sqlb is taking over wants.
	//
	// A drift gate wants the opposite. An incremental adoption declares a
	// handful of tables while the database holds dozens, and diffing a
	// declaration of five tables against an import of sixty-nine reports the
	// other sixty-four as tables to drop — so the gate has to narrow one side,
	// and this is where it is narrowed (issue #54). Names are storage names,
	// before any module prefix is stripped, because that is what the database
	// calls them.
	//
	// A named table that is not in the database is reported rather than
	// ignored: a typo in this list would otherwise silently shrink what the
	// gate checks, which is the one failure a gate must not have.
	Only []string

	// Exclude drops the named tables from the import. It applies after Only,
	// so the two compose — read this schema except the queue tables — and it
	// is the right shape for the migration-history table every runner keeps,
	// which no declaration will ever describe.
	Exclude []string
}

func (o Options) schemaName() string {
	if o.Schema == "" {
		return "public"
	}
	return o.Schema
}

// Registry reads the database and returns the schema it describes, along with
// a Report of everything that could not be represented.
//
// The registry is validated before it is returned: a registry that does not
// validate would produce DDL for a schema that cannot exist, and finding that
// out here beats finding it out from a migration.
//
// Capabilities are not inferred. Nothing in a database says which columns
// should be filterable or exposed over REST, and guessing would publish
// columns nobody chose to publish — so everything imports with no capabilities
// at all and widening them is a deliberate, reviewable edit (ADR-0014).
func Registry(ctx context.Context, db sqlb.Executor, opts Options) (*schema.Registry, *Report, error) {
	cat, err := read(ctx, db, opts.schemaName())
	if err != nil {
		return nil, nil, err
	}
	return build(cat, opts)
}

// catalog is everything read from the database, before any of it is
// interpreted. Keeping the reading and the interpreting apart is what lets the
// interpretation be tested exhaustively against rows written by hand, without
// a database anywhere near it.
type catalog struct {
	tables      []tableRow
	columns     []columnRow
	constraints []constraintRow
	indexes     []indexRow
	// extensions is every non-plpgsql extension installed in the database.
	//
	// Not a table-level construct, and read anyway: an extension is invisible
	// to Diff rather than skipped by it, so a clean Report and a clean Diff
	// both said "everything is represented" about a schema that could not be
	// created at all (issue #115).
	extensions []extensionRow
}

type extensionRow struct {
	Name   string
	Schema string
}

type tableRow struct {
	Name    string
	Comment string
}

type columnRow struct {
	Table     string
	Name      string
	Type      string // format_type, e.g. "character varying(200)"
	NotNull   bool
	Default   string // pg_get_expr of the default, "" for none
	Comment   string
	Identity  string // attidentity: "" | "a" | "d"
	Generated string // attgenerated: "" | "s"
}

type constraintRow struct {
	Table    string
	Name     string
	Type     string // contype: p, u, f, c, n, x, t
	Columns  []string
	RefTable string
	RefCols  []string
	OnDelete string // confdeltype
	OnUpdate string // confupdtype
	Expr     string // pg_get_expr of a CHECK
	Def      string // pg_get_constraintdef, for reporting what was skipped

	// Deferrable and Deferred are condeferrable and condeferred: whether the
	// constraint may be deferred, and whether it is by default.
	//
	// Read for every constraint kind, not only the ones the DSL can declare it
	// on. A property nothing reads is a property the round trip is a fixpoint
	// *about* — both sides blind to the same thing — and that is the failure
	// mode ADR-0014 exists to refuse (issue #154).
	Deferrable bool
	Deferred   bool
}

type indexRow struct {
	Table      string
	Name       string
	Unique     bool
	Method     string
	Where      string
	Columns    []string
	Expression bool // an index over an expression rather than plain columns
	Def        string
	// Opclasses is the operator class of each indexed column, in the same order
	// as Columns, with "" where the column takes its type's default. For
	// pgvector it is the distance function and there is no default, so an index
	// read back without it cannot be reapplied (issue #53).
	Opclasses []string
	// Options is the index's storage parameters, as reloptions hands them back:
	// "m=16", "ef_construction=64".
	Options []string
	// Sort is the per-column sort order, in the same order as Columns, decoded
	// from pg_index.indoption. Postgres packs it as a bitmask per column: bit
	// 0 is DESC, bit 1 is NULLS FIRST. An index whose ordering cannot be read
	// back is one the declaration cannot reproduce, so the diff proposes
	// dropping the live index and cannot tell "missing" from "differently
	// ordered" (issue #64).
	Sort []int16
}

// The catalog queries.
//
// They are written against what Postgres actually returns rather than against
// what it ought to: a varchar column reports as "character varying(200)", an
// enum's CHECK normalises to "= ANY (ARRAY[...])" rather than the IN () it was
// written as, and a literal default comes back with its cast attached. Each of
// those was observed before the mapping that handles it was written.
//
// Every catalog column of type "char" is cast to text. That is not decoration:
// attgenerated is a zero byte on an ordinary column, and a driver is entitled
// to decode that as a one-character string rather than an empty one — which is
// how every column in a database briefly became a generated column here. The
// cast makes the empty case empty on any driver.

const tableQuery = `
SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), '')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'r'
ORDER BY c.relname`

// extensionQuery reads what CREATE EXTENSION installed.
//
// plpgsql is excluded because every Postgres has it and nobody declared it. No
// schema filter: an extension is installed per database, not per schema, so an
// extension living in a dedicated "extensions" schema is still the one the
// introspected tables depend on.
const extensionQuery = `
SELECT e.extname, n.nspname
FROM pg_extension e
JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname <> 'plpgsql'
ORDER BY e.extname`

const columnQuery = `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
       COALESCE(col_description(c.oid, a.attnum), ''),
       a.attidentity::text, a.attgenerated::text
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
WHERE n.nspname = $1 AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY c.relname, a.attnum`

// constraintQuery keeps conkey and confkey in their declared order. Postgres
// stores them as arrays whose order is meaningful — it is the column order of a
// composite key — and unnesting without WITH ORDINALITY loses it.
const constraintQuery = `
SELECT c.relname, con.conname, con.contype::text,
       COALESCE(k.cols, ''), COALESCE(ft.relname, ''), COALESCE(fk.cols, ''),
       con.confdeltype::text, con.confupdtype::text,
       COALESCE(pg_get_expr(con.conbin, con.conrelid), ''),
       pg_get_constraintdef(con.oid),
       con.condeferrable, con.condeferred
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class ft ON ft.oid = con.confrelid
LEFT JOIN LATERAL (
  SELECT string_agg(a.attname, ',' ORDER BY u.ord) AS cols
  FROM unnest(con.conkey) WITH ORDINALITY AS u(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = u.attnum
) k ON true
LEFT JOIN LATERAL (
  SELECT string_agg(a.attname, ',' ORDER BY u.ord) AS cols
  FROM unnest(con.confkey) WITH ORDINALITY AS u(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = u.attnum
) fk ON true
WHERE n.nspname = $1
ORDER BY c.relname, con.conname`

// indexQuery skips the indexes that exist only to enforce a constraint, which
// are reported as the constraint instead.
//
// The match is on the constraint's own table as well as its index: a foreign
// key's conindid points at the *referenced* table's unique index, so testing
// conindid alone would hide the primary key of every table anything references.
const indexQuery = `
SELECT c.relname, i.relname, x.indisunique, am.amname,
       COALESCE(pg_get_expr(x.indpred, x.indrelid), ''),
       COALESCE(k.cols, ''), (0 = ANY (x.indkey::int2[])),
       pg_get_indexdef(x.indexrelid),
       COALESCE(oc.classes, ''),
       COALESCE(array_to_string(i.reloptions, ','), ''),
       COALESCE(array_to_string(x.indoption::int2[], ','), '')
FROM pg_index x
JOIN pg_class i ON i.oid = x.indexrelid
JOIN pg_class c ON c.oid = x.indrelid
JOIN pg_am am ON am.oid = i.relam
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN LATERAL (
  SELECT string_agg(a.attname, ',' ORDER BY u.ord) AS cols
  FROM unnest(x.indkey::int2[]) WITH ORDINALITY AS u(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attnum = u.attnum
) k ON true
-- The operator class per indexed column, in index order, and empty where the
-- column takes its type's default: comparing against opcdefault is what keeps
-- an ordinary btree from acquiring "text_ops" in every declaration.
LEFT JOIN LATERAL (
  SELECT string_agg(CASE WHEN op.opcdefault THEN '' ELSE op.opcname END, ',' ORDER BY u.ord) AS classes
  FROM unnest(x.indclass::oid[]) WITH ORDINALITY AS u(class, ord)
  JOIN pg_opclass op ON op.oid = u.class
) oc ON true
WHERE n.nspname = $1
  AND NOT EXISTS (
    SELECT 1 FROM pg_constraint con
    WHERE con.conindid = x.indexrelid AND con.conrelid = x.indrelid
      AND con.contype IN ('p', 'u', 'x')
  )
ORDER BY c.relname, i.relname`

func read(ctx context.Context, db sqlb.Executor, nspname string) (*catalog, error) {
	if db == nil {
		return nil, fmt.Errorf("introspect: no database given")
	}
	cat := &catalog{}

	if err := query(ctx, db, tableQuery, func(rows pgx.Rows) error {
		var r tableRow
		if err := rows.Scan(&r.Name, &r.Comment); err != nil {
			return err
		}
		cat.tables = append(cat.tables, r)
		return nil
	}, nspname); err != nil {
		return nil, fmt.Errorf("introspect: reading tables: %w", err)
	}

	if err := query(ctx, db, columnQuery, func(rows pgx.Rows) error {
		var r columnRow
		if err := rows.Scan(&r.Table, &r.Name, &r.Type, &r.NotNull, &r.Default,
			&r.Comment, &r.Identity, &r.Generated); err != nil {
			return err
		}
		cat.columns = append(cat.columns, r)
		return nil
	}, nspname); err != nil {
		return nil, fmt.Errorf("introspect: reading columns: %w", err)
	}

	if err := query(ctx, db, constraintQuery, func(rows pgx.Rows) error {
		var r constraintRow
		var cols, refCols string
		if err := rows.Scan(&r.Table, &r.Name, &r.Type, &cols, &r.RefTable, &refCols,
			&r.OnDelete, &r.OnUpdate, &r.Expr, &r.Def,
			&r.Deferrable, &r.Deferred); err != nil {
			return err
		}
		r.Columns, r.RefCols = splitList(cols), splitList(refCols)
		cat.constraints = append(cat.constraints, r)
		return nil
	}, nspname); err != nil {
		return nil, fmt.Errorf("introspect: reading constraints: %w", err)
	}

	if err := query(ctx, db, extensionQuery, func(rows pgx.Rows) error {
		var r extensionRow
		if err := rows.Scan(&r.Name, &r.Schema); err != nil {
			return err
		}
		cat.extensions = append(cat.extensions, r)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("introspect: reading extensions: %w", err)
	}

	if err := query(ctx, db, indexQuery, func(rows pgx.Rows) error {
		var r indexRow
		var cols, classes, options, sortOpts string
		if err := rows.Scan(&r.Table, &r.Name, &r.Unique, &r.Method, &r.Where,
			&cols, &r.Expression, &r.Def, &classes, &options, &sortOpts); err != nil {
			return err
		}
		r.Columns = splitList(cols)
		r.Sort = parseIndexOptions(sortOpts)
		// Not splitList: an empty entry means "this column takes the default
		// class" and has to keep its position, which a filter would lose.
		if classes != "" {
			r.Opclasses = strings.Split(classes, ",")
		}
		r.Options = splitList(options)
		cat.indexes = append(cat.indexes, r)
		return nil
	}, nspname); err != nil {
		return nil, fmt.Errorf("introspect: reading indexes: %w", err)
	}

	return cat, nil
}

// parseIndexOptions decodes pg_index.indoption, one bitmask per indexed column.
// Not splitList: a zero entry is the common case — ascending, default null
// placement — and has to keep its position, which a filter would lose.
func parseIndexOptions(raw string) []int16 {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int16, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			// An option this package cannot read is reported as the default
			// rather than guessed at: the index still imports, and the drift
			// gate is what surfaces the disagreement.
			n = 0
		}
		out = append(out, int16(n))
	}
	return out
}

func query(ctx context.Context, db sqlb.Executor, sqlText string, scan func(pgx.Rows) error, args ...any) error {
	rows, err := db.Query(ctx, sqlText, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
