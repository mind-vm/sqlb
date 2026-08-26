package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Diagnostic is one finding from [Registry.Lint]: a schema that is correct but
// operationally unwise. Diagnostics are advisory — nothing fails because of
// one, and a schema may have good reasons to keep one, such as a filterable
// column on a table of twenty rows that does not need an index.
type Diagnostic struct {
	Rule     string
	Table    string
	Column   string
	Severity Severity
	Message  string
	// Fix is the concrete change that would resolve it, where there is one.
	Fix string
}

// Severity ranks how much a diagnostic should be believed.
type Severity string

const (
	// SeverityWarn is a problem that will very likely bite in production.
	SeverityWarn Severity = "warn"
	// SeverityInfo is worth a look but is often fine.
	SeverityInfo Severity = "info"
)

func (d Diagnostic) String() string {
	loc := d.Table
	if d.Column != "" {
		loc += "." + d.Column
	}
	s := fmt.Sprintf("[%s] %s: %s: %s", d.Severity, d.Rule, loc, d.Message)
	if d.Fix != "" {
		s += "\n    fix: " + d.Fix
	}
	return s
}

// Diagnostics is an ordered set of lint results.
type Diagnostics []Diagnostic

func (ds Diagnostics) String() string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String()
	}
	return strings.Join(parts, "\n")
}

// Warnings returns only the warn-level diagnostics, for callers that want to
// fail a build on those but tolerate info.
func (ds Diagnostics) Warnings() Diagnostics {
	var out Diagnostics
	for _, d := range ds {
		if d.Severity == SeverityWarn {
			out = append(out, d)
		}
	}
	return out
}

// alreadyNamed reports whether some earlier rule has already told the reader to
// index this column. Four rules want the same index for different reasons, and
// four warnings naming one column read as four problems.
func alreadyNamed(ds Diagnostics, table, column string) bool {
	for _, d := range ds {
		if d.Table != table || d.Column != column {
			continue
		}
		switch d.Rule {
		case "unindexed-filter", "unindexed-sort", "unindexed-expand", "unindexed-inverse-expand":
			return true
		}
	}
	return false
}

// Lint reports schemas that are correct but operationally unwise.
//
// Validate answers "is this schema well-formed?" and returns errors. Lint
// answers "will this schema behave badly in production?" and returns advice.
// The distinction matters: a table can pass validation completely and still
// expose an unindexed filter that sequential-scans a large table on every
// request, which is the kind of mistake that is invisible in review and obvious
// at three in the morning.
func (r *Registry) Lint() Diagnostics {
	var out Diagnostics
	add := func(d Diagnostic) { out = append(out, d) }

	for _, t := range r.Tables() {
		indexed := t.indexedColumns()
		// What the declaration guarantees about every read of this table, which
		// is what decides whether an unindexed column below costs a scan of the
		// table or of one tenant's rows — and which index would fix it (#296).
		scope := scopeOf(t, indexed)

		for _, f := range t.fields {
			d := f.Desc()

			// A computed column can never be the leading column of an index —
			// there is no column to index — so the two index rules below would
			// fire on every one of them and say nothing actionable. What is
			// worth saying is said once, here, and the cost differs by tier: a
			// row-local expression is arithmetic per row, a correlated subquery
			// is a query per row.
			if d.Computed() {
				if d.Filterable || d.Sortable {
					add(Diagnostic{
						Rule: "computed-without-index", Table: t.name, Column: d.Name,
						Severity: SeverityInfo,
						Message: "column is computed, so filtering or sorting on it evaluates the expression for every candidate row and no index can serve it" +
							subqueryNote(d),
						Fix: "keep the expression row-local, or store the value and maintain it — a trigger-kept counter is a real column and can be indexed",
					})
				}
				continue
			}

			// TypeText with no Default and no Nullable is NOT NULL with no
			// fallback, which the generated create body renders as required
			// (codegen.optionalOnCreate: !Nullable && !DatabaseSupplied()).
			// That is correct far more often than not, so this says nothing
			// about the column being wrong. It exists because the first
			// place a porting author (coming from a framework where a blank
			// string is the unmarked default, e.g. Django's blank=True)
			// discovers the requirement is a 422 at request time, not a line
			// in the schema. Scoped to TypeText rather than every
			// string-shaped column, because Text is the free-form
			// body/description/notes case the mismatch actually comes from;
			// Varchar's declared size already reads as a structured field (a
			// name, a slug, an email) where "required" is unambiguous and
			// this note would be pure noise.
			if t.rest != nil && t.rest.Ops.Has(OpCreate) &&
				d.Type == TypeText && !d.Nullable && !d.DatabaseSupplied() &&
				!d.ReadOnly && !d.Hidden && !d.PrimaryKey {
				add(Diagnostic{
					Rule: "text-required-on-create", Table: t.name, Column: d.Name,
					Severity: SeverityInfo,
					Message: "no .Default(...) and not nullable, so this column is required on create; " +
						"this is correct if a value must always be given, and worth a second look only if " +
						"callers were expected to be able to leave it blank",
					Fix: `if blank should be allowed, add .Default(schema.Value("")) or .Nullable(); otherwise no change is needed`,
				})
			}

			// A raw default that spells out what a helper renders on one
			// particular target.
			//
			// Expr takes arbitrary SQL and nothing downstream questions it, so
			// the column works — on that target. What it gave up is that the
			// helper was still asking migrate which Postgres this migration is
			// for, and the raw string has answered once, in the declaration,
			// for every target there will ever be. The reporter of #293 wrote
			// Expr("uuidv7()") believing GenUUIDv7 needed the pg_uuidv7
			// extension; it does not on 18, and the hand-written version is
			// the one that stops being portable.
			//
			// Exact match, like the renderer's own resolve: an expression that
			// merely contains the builtin — coalesce(uuidv7()::text, '') — is
			// a composite the helper could not have produced, and rewriting it
			// is not being proposed.
			if d.Default != nil {
				for _, td := range TargetDefaults() {
					if d.Default.Raw != td.Builtin {
						continue
					}
					add(Diagnostic{
						Rule: "raw-default-has-helper", Table: t.name, Column: d.Name,
						Severity: SeverityWarn,
						Message: fmt.Sprintf(
							"the default %s is what %s renders once the migration targets Postgres %d; "+
								"written out here it is %s on every target, including the ones that do not have it",
							td.Builtin, td.Helper, td.Since, td.Builtin),
						Fix: fmt.Sprintf(
							"use %s and let migrate.MinPostgres(%d) choose the spelling; without it the helper "+
								"emits %s, which needs the extension",
							td.Helper, td.Since, td.Canonical),
					})
					break
				}
			}

			// A filterable column with no index leading a btree means every
			// request that uses it scans the table.
			if d.Filterable && !d.Hidden && !indexed[d.Name] && !isLowCardinality(d) &&
				!scope.single {
				// Searchable implies Filterable, so a column that declared
				// only the first has no .Filterable() to drop. Advising it
				// sends the reader looking for a call that is not there.
				drop := ".Filterable()"
				if d.Searchable {
					drop = ".Searchable(), which implies Filterable,"
				}
				msg := "column is filterable but is not the leading column of any index, " +
					"so filtering on it scans the table"
				fix := fmt.Sprintf("add .Index(%q) to the table, or drop %s from the column", d.Name, drop)
				if scope.confined() {
					// The unqualified version overstates the cost by the number
					// of tenants, and — worse — advises an index Postgres will
					// mostly decline to use. The predicate is never this column
					// alone; it is the scope's and then this one, so the index
					// that serves it leads with the scope (#296).
					msg = fmt.Sprintf(
						"column is filterable and is not indexed with %q, which every read of this "+
							"table is confined by, so filtering on it scans one tenant's rows",
						scope.column)
					fix = fmt.Sprintf("%s, or drop %s from the column",
						scope.composite(d.Name), drop)
				}
				add(Diagnostic{
					Rule: "unindexed-filter", Table: t.name, Column: d.Name,
					Severity: SeverityWarn,
					Message:  msg,
					Fix:      fix,
				})
			}

			// Sorting an unindexed column forces a sort of the whole result
			// set, which pagination makes worse by repeating it per page.
			//
			// The suggested index carries the primary key as a trailing column,
			// because that is the ordering a paged list actually runs: every
			// list ends with the key so the page boundary is unambiguous
			// (ADR-0027), and an index on the sort column alone cannot serve
			// the cursor's seek.
			if d.Sortable && !indexed[d.Name] && !scope.single {
				pk := pkColumn(t)
				fix := fmt.Sprintf("add .Index(%q) if this table will grow", d.Name)
				if pk != "" && pk != d.Name {
					fix = fmt.Sprintf(
						"add .AddIndex(schema.Index{Columns: []string{%q, %q}}) if this table will grow; "+
							"the key is the second column because a paged list orders by it to break ties",
						d.Name, pk)
				}
				msg := "column is sortable but not indexed, so each page re-sorts the matching rows"
				if scope.confined() {
					// Same correction as the filter rule, one column longer: a
					// paged list under a scope is WHERE scope = $1 ORDER BY this
					// column, then the key to break the tie, and the index that
					// serves all three in one seek spells them in that order.
					msg = fmt.Sprintf(
						"column is sortable and is not indexed with %q, which every read of this "+
							"table is confined by, so each page re-sorts one tenant's rows",
						scope.column)
					cols := []string{d.Name}
					if pk != "" && pk != d.Name {
						cols = append(cols, pk)
					}
					fix = scope.composite(cols...) + " if this table will grow"
					if len(cols) > 1 {
						fix += "; the scope leads because every read carries its predicate, and the " +
							"key is last because a paged list orders by it to break ties"
					}
				}
				add(Diagnostic{
					Rule: "unindexed-sort", Table: t.name, Column: d.Name,
					Severity: SeverityInfo,
					Message:  msg,
					Fix:      fix,
				})
			}

			// Search compiles to ILIKE '%term%', which no btree index can
			// serve. A trigram GIN index is the usual answer.
			if d.Searchable {
				if !hasIndexMethod(t, d.Name, "gin") {
					add(Diagnostic{
						Rule: "search-without-trigram", Table: t.name, Column: d.Name,
						Severity: SeverityWarn,
						Message:  "searchable columns compile to ILIKE '%…%', which no btree index can serve",
						Fix: fmt.Sprintf(
							`add a trigram index: .AddIndex(schema.Index{Columns: []string{%q}, Method: "gin"}) with pg_trgm installed`,
							d.Name),
					})
				}
			}

			// Expanding a relation joins on the foreign key column.
			if d.Expandable && d.Ref != nil && !indexed[d.Name] {
				add(Diagnostic{
					Rule: "unindexed-expand", Table: t.name, Column: d.Name,
					Severity: SeverityWarn,
					Message:  "relation is expandable but its foreign key is not indexed, so expansion joins without one",
					Fix:      fmt.Sprintf("add .Index(%q)", d.Name),
				})
			}

			// A foreign key that nothing above has already spoken for. The
			// other three rules each name a *read* that scans without an
			// index; this one is about the write side, which has no capability
			// to key off: a delete or update of the target row takes a
			// referential-action check with it, and without an index that check
			// scans this table once per affected row.
			//
			// Last of the four, and suppressed when one of them has already
			// named the column, because a reader fixing one diagnostic fixes
			// all of them with the same word.
			if d.Ref != nil && !indexed[d.Name] && !alreadyNamed(out, t.name, d.Name) {
				add(Diagnostic{
					Rule: "unindexed-ref", Table: t.name, Column: d.Name,
					Severity: SeverityWarn,
					Message: "column is a reference and is not indexed, so a delete or update of the row it points at " +
						"scans this table to enforce the referential action",
					Fix: "add .Indexed() to the column",
				})
			}

			if d.Ref != nil && d.Ref.InverseExpandable {
				// The reverse direction runs one correlated subquery per base
				// row, so an unindexed foreign key is a sequential scan per row
				// of the page rather than one extra scan for the statement.
				// Same rule as above, a worse consequence.
				if !indexed[d.Name] {
					add(Diagnostic{
						Rule: "unindexed-inverse-expand", Table: t.name, Column: d.Name,
						Severity: SeverityWarn,
						Message: fmt.Sprintf(
							"%q collects these rows, and an expansion runs one subquery per row of the page; without an index on this column each of those scans the table",
							d.Ref.Inverse),
						Fix: fmt.Sprintf("add .Index(%q)", d.Name),
					})
				}
				// An expanded collection is capped, and past the cap the caller
				// is expected to follow this table's own endpoint filtered by
				// this column. If it cannot filter by it, the overflow has
				// nowhere to go and the truncation is a dead end.
				if !d.Filterable {
					add(Diagnostic{
						Rule: "uncapped-inverse-overflow", Table: t.name, Column: d.Name,
						Severity: SeverityWarn,
						Message: fmt.Sprintf(
							"an expansion of %q is capped and reports has_more, but this column is not filterable, so a caller has no way to read the rest",
							d.Ref.Inverse),
						Fix: "add .Filterable() to this column",
					})
				}
			}
		}

		// A table outside any module in a codebase that uses them is usually
		// an oversight, and it is the one that will collide later.
		if t.module == "" && r.module != "" {
			add(Diagnostic{
				Rule: "unnamespaced-table", Table: t.name,
				Severity: SeverityInfo,
				Message:  "table is not in a module, so its name is not namespaced",
			})
		}

		if t.rest == nil {
			continue
		}

		// This used to warn that paging could repeat or skip rows. It cannot
		// any more: filter.Apply appends the primary key to every list, so the
		// ordering is total whether or not the schema offers a way to choose
		// it (ADR-0027). What is left is a usability point rather than a
		// correctness one, so it is an info — a list whose order no client can
		// influence is usually an oversight, not a decision.
		if t.rest.Ops.Has(OpList) && len(capableColumns(t, capSortable)) == 0 {
			add(Diagnostic{
				Rule: "list-without-sort", Table: t.name,
				Severity: SeverityInfo,
				Message: "list endpoint has no sortable column, so every client gets the same " +
					"primary-key order and none can ask for another",
				Fix: "mark at least one column .Sortable(), conventionally created_at",
			})
		}

		// A list endpoint with no filters can only be paged through, which is
		// usually an oversight rather than an intention.
		if t.rest.Ops.Has(OpList) && len(capableColumns(t, capFilterable)) == 0 {
			add(Diagnostic{
				Rule: "list-without-filters", Table: t.name,
				Severity: SeverityInfo,
				Message:  "list endpoint exposes no filterable column, so clients can only page through everything",
			})
		}

		// Without an explicit ceiling the package default applies, which may
		// not be what this table can afford.
		if t.rest.Ops.Has(OpList) && t.rest.MaxPageSize == 0 {
			add(Diagnostic{
				Rule: "no-max-page-size", Table: t.name,
				Severity: SeverityInfo,
				Message:  "no MaxPageSize, so the package default applies as the hard ceiling",
				Fix:      "set MaxPageSize on the REST exposure to a value this table can serve",
			})
		}

		// DefaultPageSize and MaxPageSize only mean anything on a list route —
		// there is no page to bound without one. A table that sets either while
		// OpList is absent from Ops compiles fine and does nothing: this is the
		// inverse of no-max-page-size above, one register down from the
		// mount-time refusal on Ops == CRUD, and it is the exact shape that hit
		// every one of sixteen tables in a real port, found only by end-to-end
		// testing because nothing flagged it first (#201).
		if !t.rest.Ops.Has(OpList) && (t.rest.DefaultPageSize != 0 || t.rest.MaxPageSize != 0) {
			add(Diagnostic{
				Rule: "page-size-without-list", Table: t.name,
				Severity: SeverityInfo,
				Message:  "DefaultPageSize or MaxPageSize is set but OpList is not in Ops, so there is no list route for it to bound",
				Fix:      "add schema.OpList to Ops, or drop DefaultPageSize/MaxPageSize",
			})
		}

		// Writable endpoints on a table with no key cannot address a row.
		if t.rest.Ops.Has(OpCreate) && t.PrimaryKey() == nil {
			add(Diagnostic{
				Rule: "create-without-key", Table: t.name,
				Severity: SeverityWarn,
				Message:  "table accepts creates but has no primary key, so a created row cannot be addressed afterwards",
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == SeverityWarn
		}
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// Lint checks the default registry.
func Lint() Diagnostics { return defaultRegistry.Lint() }

// tenantScope is what a table's declaration guarantees about every read of it.
//
// Scoped is an obligation rather than a note: rest.Resource refuses to mount a
// resource whose exposed operations have no hook behind them (ADR-0030), so an
// *exposed* table with a Scoped column either carries that column's predicate on
// every generated read or the server does not start. That is enough to say what
// an unindexed filter on such a table actually costs, without Lint knowing
// anything about which hooks a registry happens to hold (#296).
//
// Restricted to exposed tables because that is where the obligation bites. A
// Scoped column on an internal table states the same intent and is checked by
// nothing, so the diagnostics there keep the unqualified wording.
//
// The scope column has to be indexed itself for any of this to follow. An
// unindexed one means the scope predicate is the scan, and the diagnostic
// naming *that* column is the one worth reading first — so this reports nothing
// and the other rules speak plainly.
type tenantScope struct {
	// column is the Scoped column every read is confined by, or empty when the
	// table has none this can rely on.
	column string
	// single reports that the scope predicate selects at most one row, because
	// the scope column is also the primary key or unique. A table keyed by its
	// own tenant has one row per caller, so nothing else about it can be slow.
	single bool
}

func (s tenantScope) confined() bool { return s.column != "" }

func scopeOf(t *TableDef, indexed map[string]bool) tenantScope {
	if t.rest == nil {
		return tenantScope{}
	}
	for _, f := range t.fields {
		d := f.Desc()
		if !d.Scoped || !indexed[d.Name] {
			continue
		}
		return tenantScope{column: d.Name, single: d.PrimaryKey || d.Unique}
	}
	return tenantScope{}
}

// composite spells the index that serves a predicate on col, scope column
// first, because every read carries the scope's predicate and none carries
// col's alone.
func (s tenantScope) composite(cols ...string) string {
	all := append([]string{s.column}, cols...)
	quoted := make([]string, len(all))
	for i, c := range all {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	return fmt.Sprintf("add .AddIndex(schema.Index{Columns: []string{%s}})",
		strings.Join(quoted, ", "))
}

// indexedColumns returns the columns that lead an index, are unique, or are the
// primary key — the ones Postgres can seek on directly.
func (t *TableDef) indexedColumns() map[string]bool {
	out := map[string]bool{}
	for _, f := range t.fields {
		d := f.Desc()
		if d.PrimaryKey || d.Unique {
			out[d.Name] = true
		}
	}
	for _, idx := range t.Indexes() {
		if len(idx.Columns) > 0 {
			// Only the leading column of a btree is seekable on its own.
			out[idx.Columns[0]] = true
		}
	}
	return out
}

func hasIndexMethod(t *TableDef, column, method string) bool {
	for _, idx := range t.Indexes() {
		if !strings.EqualFold(idx.Method, method) {
			continue
		}
		for _, c := range idx.Columns {
			if c == column {
				return true
			}
		}
	}
	return false
}

type capKind int

const (
	capFilterable capKind = iota
	capSortable
)

func capableColumns(t *TableDef, k capKind) []string {
	var out []string
	for _, f := range t.fields {
		d := f.Desc()
		if d.Hidden {
			continue
		}
		if (k == capFilterable && d.Filterable) || (k == capSortable && d.Sortable) {
			out = append(out, d.Name)
		}
	}
	return out
}

// isLowCardinality reports whether an index would probably not help. A boolean
// or a short enum has too few distinct values for a btree to beat a scan, so
// flagging it as unindexed would be noise.
func isLowCardinality(d *FieldDesc) bool {
	if d.Type == TypeBool {
		return true
	}
	return d.Type == TypeEnum && len(d.EnumValues) > 0 && len(d.EnumValues) <= 4
}

// pkColumn is the primary key's column name, or empty when the table declares
// none. Diagnostics use it to suggest the composite index a paged list wants.
func pkColumn(t *TableDef) string {
	pk := t.PrimaryKey()
	if pk == nil {
		return ""
	}
	return pk.Desc().Name
}

// subqueryNote adds the sharper half of the computed-column warning when the
// expression is a subquery: the per-row cost is then a query rather than
// arithmetic, and it scales with the page size.
func subqueryNote(d *FieldDesc) string {
	if !strings.Contains(strings.ToLower(d.Expr), "select") {
		return ""
	}
	return "; this one runs a subquery, so the cost of a page grows with its size"
}
