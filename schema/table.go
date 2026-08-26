package schema

import "strings"

// Op is a bitmask of the REST operations a table exposes.
type Op uint8

const (
	OpCreate Op = 1 << iota
	OpRead      // GET /resource/{id}
	OpUpdate
	OpDelete
	OpList // GET /resource with filter, sort, search, pagination

	// OpSingleton is GET /resource — the caller's one row, with no {id}
	// segment anywhere on the resource.
	//
	// A table keyed by its own scope column has one row per caller, and until
	// this there was no shape for it: OpList answered a one-element envelope
	// every client unwrapped forever, and OpRead asked the client to send back
	// the tenant id the server already holds — where a mismatch is a 404
	// meaning "you typed your own name wrong" (#166). Settings-per-tenant,
	// profile-per-user and subscription-per-org are all this, and each one was
	// a permanent hand-written handler beside an otherwise declared module.
	//
	// It changes the resource rather than adding a route: the item path loses
	// its {id}, so OpUpdate is PATCH /resource and OpDelete is DELETE
	// /resource. The row those address is the one the scope hook leaves, which
	// is why this is only legal on a table with a [Field.Scoped] column —
	// without one the read answers an arbitrary row and the write reaches every
	// row there is. OpList and OpRead are refused alongside it: the first
	// collides on the route and the second is the id-shaped question this
	// exists to delete.
	OpSingleton
)

// CRUD is the conventional single-row operation set. Combine it with OpList
// for a fully exposed collection.
const CRUD = OpCreate | OpRead | OpUpdate | OpDelete

// Reads is the read-only exposure: generated reads, hand-written writes.
//
// The peer of CRUD, and the shape an application adopting sqlb into an
// existing REST surface reaches for — it already has its writes, and the
// reasons they stay hand-written are domain reasons that do not expire. See
// [rest.Reads] for the worked version of why (issue #101).
const Reads = OpRead | OpList

// Has reports whether the mask contains op.
func (o Op) Has(op Op) bool { return o&op != 0 }

// String renders the mask for diagnostics.
func (o Op) String() string {
	var parts []string
	for _, e := range []struct {
		op   Op
		name string
	}{
		{OpCreate, "create"}, {OpRead, "read"}, {OpUpdate, "update"},
		{OpDelete, "delete"}, {OpList, "list"}, {OpSingleton, "singleton"},
	} {
		if o.Has(e.op) {
			parts = append(parts, e.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// REST describes how a table is exposed over HTTP.
type REST struct {
	// Path is the collection path, e.g. "/users". Defaults to "/"+table name.
	Path string
	// Ops is the set of exposed operations.
	Ops Op
	// DefaultPageSize applies when the request omits a page size. Zero means
	// the package default.
	DefaultPageSize int
	// MaxPageSize caps the page size a client may request. Zero means the
	// package default. This is a hard ceiling, not a hint.
	MaxPageSize int
	// MaxFilters caps how many filter predicates one request may carry, which
	// bounds the cost of a single query. Zero means the package default.
	MaxFilters int
	// MaxSortTerms caps how many columns one ?sort may name. Zero means the
	// package default.
	MaxSortTerms int
	// MaxOffset bounds how deep ?page= and ?offset= may reach into the result
	// set, and a request past it is refused with a message pointing at
	// ?cursor=. Zero means the package default.
	//
	// The default has to be safe for a table nobody described, which puts it
	// orders of magnitude above what any particular resource wants: a catalog
	// of ten thousand products has no legitimate offset past ten thousand, and
	// every one above it is a guaranteed empty page that still costs a scan to
	// the end. The right number is a function of the row count, which is known
	// here and nowhere else (#151).
	MaxOffset int
	// DefaultSort is the ordering a list request that names no ?sort gets,
	// written in the grammar ?sort uses: column names, a leading "-" for
	// descending, most significant first.
	//
	//	DefaultSort: []string{"-pinned", "-published_at", "-created_at"}
	//
	// Every other field here bounds a dimension of a list request. This one
	// says what the list *is*. For many resources the ordering is part of the
	// collection's meaning rather than a client preference — a feed is pinned
	// first and then newest, and a feed in primary-key order is not the feed —
	// and with nowhere to declare it, every caller restates it on every request
	// and the one that forgets gets a well-formed 200 that is quietly the wrong
	// product (#151's shape, reported as #165).
	//
	// Zero means primary-key order, which is what silence already meant; the
	// difference is that the answer is now declarable, so it reaches the
	// generated clients, the OpenAPI description and the generated skill instead
	// of living in whichever SDK facade a consumer hand-maintains.
	//
	// Every term must name a column of this table that declares Sortable.
	// `sqlb generate` refuses one that does not, and [rest.Resource] refuses
	// again at mount.
	DefaultSort []string
	// Tag groups the resource's operations in the OpenAPI document. Defaults
	// to the table name.
	Tag string

	// CreateInput declares properties the create body carries that are not
	// columns. Build it with [Body], the same way an action's body is built.
	//
	//	Children.Expose(schema.REST{
	//	    Ops: schema.OpCreate | schema.Reads,
	//	    CreateInput: schema.Body(
	//	        schema.Varchar("pin", 4).Comment("Four digits. Hashed on the way in; never stored as sent."),
	//	    ),
	//	})
	//
	// The generated body still carries every writable column. This adds to it,
	// and what it adds never reaches the row: the value arrives at BeforeCreate
	// through the context, as [sqlb.CreateInputFrom], and what the hook does
	// with it — hash it, resolve it into rows of another table, refuse the
	// request — is the application's.
	//
	// # Why a create needs this when nothing else does
	//
	// A create body is derived from the columns, so before this there was
	// nowhere to put a property that is an *input to* a column rather than the
	// column itself. The case is not exotic — a signup taking a password, an
	// invite carrying a token that is hashed and never stored, a broadcast
	// whose audience is a list that becomes rows of another table — and it had
	// two workarounds, both of which lie (#309).
	//
	// Marking the hashed column WriteOnly puts it in the body under its own
	// name, so the client sends a plaintext PIN in a property called pin_hash;
	// there is no per-field wire override to rename it, by design ("The wire is
	// the column name"). Renaming the column to `pin` fixes the wire and moves
	// the lie into the DDL, where a VARCHAR called `pin` holds a bcrypt digest.
	// Either way the honest alternative was to hand-write the create — and with
	// it the DTO, the route, the OpenAPI operation and all four clients.
	//
	// An action already declares properties in this vocabulary independent of
	// the columns; this is the same mechanism reaching the one write that could
	// not use it.
	//
	// # What a property may say
	//
	// Only what describes a value: name, type, size, nullability, enum values,
	// default and comment. The capabilities that place a column in a table —
	// Filterable, PrimaryKey, Hidden and the rest — are refused by Validate
	// rather than ignored, exactly as they are in an action's body, because a
	// property claiming Filterable reads as though it did something.
	//
	// A property may not take the name of a column, in either spelling: the two
	// share one JSON object, so the collision is a body with two meanings for
	// one key. Declaring one at all requires OpCreate — there is no other body
	// for it to be part of.
	CreateInput []*Field
}

// Index is a secondary index.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Method  string // "btree", "gin", ...; empty means the dialect default
	Where   string // optional partial-index predicate

	// Opclasses names the operator class each column is indexed under, keyed by
	// column name. An absent entry takes the type's default.
	//
	// For most indexes an operator class is a tuning decision. For some it is
	// the whole meaning: pgvector's `hnsw` has *no* default class, because the
	// class is what selects the distance function, so an index emitted without
	// one is rejected outright —
	//
	//	ERROR: data type vector has no default operator class for access method "hnsw"
	//
	// — and a schema that could not express it could not describe its own
	// database (issue #53).
	//
	//	AddIndex(schema.Index{
	//	    Name:      "idx_chunks_embedding",
	//	    Columns:   []string{"embedding"},
	//	    Method:    "hnsw",
	//	    Opclasses: map[string]string{"embedding": "vector_cosine_ops"},
	//	    With:      map[string]string{"m": "16", "ef_construction": "64"},
	//	})
	Opclasses map[string]string

	// With is the index's storage parameters — `WITH (m = 16)`. Rendered in
	// sorted key order, because a map has none and a migration that reorders
	// its own DDL between runs is a diff nobody can read.
	With map[string]string

	// Orders names the sort order each column is indexed under, keyed by column
	// name, in the same shape Opclasses uses and for the same reason: the DDL
	// layer renders it without knowing anything about index position.
	//
	// An absent entry is ascending with Postgres's default null placement,
	// which is what almost every index wants. It is here because for the
	// indexes that do not, the ordering *is* the index — an index backing
	// `ORDER BY position ASC NULLS FIRST, created_at DESC` is unusable in any
	// other order — and a declaration that could not say so proposed dropping
	// the live index and could not tell "missing" from "differently ordered"
	// (issue #64).
	//
	//	AddIndex(schema.Index{
	//	    Name:    "idx_tasks_project_position",
	//	    Columns: []string{"project_id", "position", "created_at"},
	//	    Orders: map[string]schema.IndexOrder{
	//	        "position":   {Nulls: schema.NullsFirst},
	//	        "created_at": {Desc: true},
	//	    },
	//	})
	Orders map[string]IndexOrder
}

// IndexOrder is one column's sort order within an index.
//
// Structured rather than written SQL, because a written suffix would have to
// reproduce Postgres's normalisation to compare equal — it omits ASC, and omits
// the null placement that follows from the direction — and that is the failure
// mode issue #63 is about. A zero IndexOrder means ascending with the default
// placement, so a map entry is only ever needed for a column that departs from
// it.
type IndexOrder struct {
	Desc  bool
	Nulls Nulls
}

// Nulls is where NULLs sort within one index column. The zero value follows
// Postgres's own default, which is not a single placement: NULLS LAST for
// ascending, NULLS FIRST for descending.
type Nulls string

const (
	NullsDefault Nulls = ""
	NullsFirst   Nulls = "first"
	NullsLast    Nulls = "last"
)

// Suffix renders the order as the DDL fragment that follows the column, empty
// when the order is the one Postgres assumes.
//
// Normalised the way Postgres normalises: an explicit ASC is dropped, and so is
// a null placement that already follows from the direction. That is what makes
// two spellings of the same order compare equal, and it is why Suffix is also
// what the diff fingerprints — a declaration written `{Desc: true, Nulls:
// NullsFirst}` and one written `{Desc: true}` are the same index, and reading
// the second back from the catalog must not propose replacing the first.
func (o IndexOrder) Suffix() string {
	var out string
	if o.Desc {
		out = " DESC"
	}
	switch {
	case o.Nulls == NullsFirst && !o.Desc:
		out += " NULLS FIRST"
	case o.Nulls == NullsLast && o.Desc:
		out += " NULLS LAST"
	}
	return out
}

// Check is a table-level check constraint.
type Check struct {
	Name string
	Expr string
}

// Unique is a table-level UNIQUE constraint over one or more columns — the
// table-level peer of Field.Unique().
//
// It is a different object from a unique index, and the difference is not
// cosmetic: only a constraint can be the target of
// FOREIGN KEY … REFERENCES t (a, b) or be named in ON CONFLICT ON CONSTRAINT.
// Declaring one where the database has the other produces a migration that
// drops and rebuilds, which on a live table is the expensive kind.
type Unique struct {
	Name    string
	Columns []string

	// Deferrable is when Postgres checks the constraint. The zero value is
	// NOT DEFERRABLE, which is checked at the statement and is what almost
	// every unique constraint wants.
	//
	// [DeferredCheck] is the one that is not merely a tuning knob. A rule about
	// the *committed* state cannot be enforced per statement when the rows that
	// satisfy it are written in more than one statement: a variant identified by
	// its option values is inserted before the option values that identify it,
	// so every new variant passes through a state where its denormalised
	// signature is still the default and two variants of one product collide on
	// it (issue #154). The alternatives weaken the rule — a partial index
	// excluding the placeholder — or move it into application code, where two
	// concurrent writers interleave between the check and the insert.
	Deferrable Deferrable
}

// Deferrable is when Postgres checks a constraint: at the statement, or at
// COMMIT. The zero value is NOT DEFERRABLE.
//
// Structured rather than written SQL, and normalised by [Deferrable.Suffix] for
// the reason [IndexOrder] is: the diff fingerprints the rendered clause, so two
// spellings of the same answer have to render the same or every run proposes
// replacing a constraint that has not changed.
type Deferrable string

const (
	// NotDeferrable checks at the end of each statement. The default, and the
	// only setting under which a constraint violation names the statement that
	// caused it rather than the COMMIT that found it.
	NotDeferrable Deferrable = ""
	// DeferrableCheck may be deferred, and is not unless a transaction says
	// SET CONSTRAINTS ... DEFERRED. It is the setting for a rule that is
	// per-statement in general and per-transaction for one caller.
	DeferrableCheck Deferrable = "immediate"
	// DeferredCheck is deferred by default: the constraint holds over the
	// committed state and says nothing about the middle of a transaction.
	DeferredCheck Deferrable = "deferred"
)

// Valid reports whether the value is one of the three Postgres has. A typed
// string is open, and the alternative to checking it here is emitting DDL that
// Postgres refuses halfway through a migration.
func (d Deferrable) Valid() bool {
	switch d {
	case NotDeferrable, DeferrableCheck, DeferredCheck:
		return true
	}
	return false
}

// Suffix renders the clause that follows a constraint definition, empty for the
// default. It is what the DDL emits and what the diff compares.
func (d Deferrable) Suffix() string {
	switch d {
	case DeferrableCheck:
		return " DEFERRABLE INITIALLY IMMEDIATE"
	case DeferredCheck:
		return " DEFERRABLE INITIALLY DEFERRED"
	}
	return ""
}

// Exclusion is an EXCLUDE constraint: no two rows may hold values that are
// pairwise related by the given operators.
//
// It is the one constraint with no near miss. A composite UNIQUE has a unique
// index; a composite primary key has a surrogate; smallint has integer. Dropping
// an exclusion has no equivalent at all — the alternatives are enforcing it in
// application code, where two concurrent requests interleave between the check
// and the insert, or leaving it as unmanaged DDL and holding a permanent
// known-difference exception in the drift gate. It is the only construct in
// either adoption corpus where not declaring it loses a *correctness* property
// rather than a performance or ergonomic one (issue #121).
//
//	AddExclude(schema.Exclusion{
//	    Name:     "bookings_no_double_booking",
//	    Using:    "gist",
//	    Elements: "coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&",
//	    Where:    "status = 'confirmed'",
//	})
//
// Elements and Where are hand-written SQL, the way [TableDef.Check] takes
// hand-written SQL and for the same reason: Postgres stores a parse tree and
// renders it back in its own spelling, so any structured form here would have to
// reproduce that spelling exactly or every diff would propose replacing a
// constraint that had not changed. Both are put through the same probe a check
// goes through before a diff (shadow.Normalize), which asks Postgres rather than
// guessing.
//
// An exclusion over a scalar with `=` needs the btree_gist extension, which no
// generated DDL creates — introspect reports the extensions a database has so
// the list is knowable before the first bootstrap rather than after 228 errors
// (issue #115).
type Exclusion struct {
	Name string
	// Using is the index method. Empty means Postgres's default, which is
	// btree — and which almost no exclusion wants, since the operators that
	// make one useful (&&, and = over a range) live in gist.
	Using string
	// Elements is the body of the constraint: the comma-separated
	// `<column-or-expression> WITH <operator>` list, without the surrounding
	// parentheses.
	Elements string
	// Where is the optional predicate that narrows which rows the constraint
	// applies to, without the surrounding parentheses.
	Where string
}

// TableDef is a table declaration. Build one with Table, which also registers
// it in the default registry.
type TableDef struct {
	name     string // storage name, including any module prefix
	local    string // name as declared, without the prefix
	module   string
	comment  string
	pkName   string
	oldName  string // previous storage name, from RenamedFrom
	fields   []*Field
	indexes  []Index
	checks   []Check
	uniques  []Unique
	excls    []Exclusion
	pkCols   []string // a composite PRIMARY KEY, when the key is not one column
	rest     *REST
	actions  []Action
	queries  []Query
	typeName string // TypeName override; "" means derive it from local

	// isView and viewQuery are set by View rather than Table. A view has none
	// of the DDL machinery above — no indexes, checks, uniques, exclusions or
	// composite key — because those describe how a table stores its own rows,
	// and a view has none; its rows are viewQuery's, read fresh every time.
	isView    bool
	viewQuery string
}

// Table declares a table and registers it in the default registry. This is the
// form a schema file uses.
func Table(name string, specs ...FieldSpec) *TableDef {
	return defaultRegistry.Table(name, specs...)
}

// Table declares a table in a specific registry. Use it to keep a schema
// isolated from the default one, which is mainly what tests want.
func (r *Registry) Table(name string, specs ...FieldSpec) *TableDef {
	t := &TableDef{name: r.Qualify(name), local: name, module: r.module}
	for _, s := range specs {
		if s == nil {
			continue
		}
		t.fields = append(t.fields, s.fields()...)
	}
	r.Add(t)
	return t
}

// View declares a read-only view and registers it in the default registry.
//
// query is the view's own SELECT, written by hand — sqlb does not derive a
// view's query from a builder expression, the same way a table's DDL is
// derived from its fields but a computed column's FromSQL expression is
// still written by hand (schema/field.go). cols describes the view's own
// output columns the same way a table's do, so the columns need to be typed
// even though nothing here checks them against query: a view's shape is a
// contract with the database, and a mismatch surfaces as a scan error at
// query time, not at declaration time.
//
// A view has no primary key requirement of its own. Give it one with
// PrimaryKeyColumns/PrimaryKeyNamed if it has a natural key a REST resource
// can address rows by; without one, Expose can still list it
// (schema.OpList alone) but not read or update a single row by key, the
// same rule rest.Resource already enforces for a keyless table.
func View(name, query string, cols ...FieldSpec) *TableDef {
	return defaultRegistry.View(name, query, cols...)
}

// View declares a view in a specific registry. See [View].
func (r *Registry) View(name, query string, cols ...FieldSpec) *TableDef {
	t := &TableDef{name: r.Qualify(name), local: name, module: r.module, isView: true, viewQuery: query}
	for _, s := range cols {
		if s == nil {
			continue
		}
		t.fields = append(t.fields, s.fields()...)
	}
	r.Add(t)
	return t
}

// IsView reports whether this declaration is a view rather than a table.
func (t *TableDef) IsView() bool { return t.isView }

// ViewQuery is the SELECT a view was declared with. Empty for a table.
func (t *TableDef) ViewQuery() string { return t.viewQuery }

// columnIndexes are the indexes a column asked for with [Field.Indexed] rather
// than by naming one at the table level.
//
// Only a declaration puts one here. Until #259 an external reference carried
// one implicitly, on the argument that a soft foreign key exists to be joined
// on — true, and still the advice [Lint] gives, but an implication is not
// something a registry built by reading a database can honour. introspect
// imports a self-referencing key as an ExternalRef, so that registry claimed an
// index the database did not have, and every diff against it proposed dropping
// what was never created.
//
// They are resolved when the index set is read rather than appended here, and
// that ordering is the whole of it: everything a declaration says about a table
// after Table() returns — .Index("org_id"), .UniqueIndex("org_id", "code"), an
// index introspect read back out of the database — arrives later. Deciding
// earlier meant deciding against an empty list, so a table that went on to
// declare an index on the same column ended up with two indexes of the same
// name, and a registry introspect built carried an index the database does not
// have (issues #54 and #55, found by the drift gate).
func (t *TableDef) columnIndexes() []Index {
	var out []Index
	for _, f := range t.fields {
		if f.d.indexWanted && !t.hasLeadingIndex(f.d.Name) {
			out = append(out, Index{
				Name:    indexName(t.name, []string{f.d.Name}, false),
				Columns: []string{f.d.Name},
			})
		}
	}
	return out
}

// hasLeadingIndex reports whether a column already leads an index, is unique,
// or is the primary key — the cases Postgres can seek on directly.
func (t *TableDef) hasLeadingIndex(column string) bool {
	for _, f := range t.fields {
		if f.d.Name == column && (f.d.PrimaryKey || f.d.Unique) {
			return true
		}
	}
	for _, idx := range t.indexes {
		if len(idx.Columns) > 0 && idx.Columns[0] == column {
			return true
		}
	}
	return false
}

// Name is the table's storage name, including any module prefix. This is the
// name that reaches SQL.
func (t *TableDef) Name() string { return t.name }

// LocalName is the name as declared, without the module prefix.
func (t *TableDef) LocalName() string { return t.local }

// Module is the owning module name, or "" if the table is not in one.
func (t *TableDef) Module() string { return t.module }

// Fields returns the table's columns in declaration order, computed ones
// included: they are columns to every consumer that describes the row — the
// model, the clients, the CLI, the OpenAPI document.
func (t *TableDef) Fields() []*Field { return t.fields }

// StoredFields returns the columns the database actually holds.
//
// It is what the DDL and the diff read, and the only distinction either of them
// has to make about a computed column: an expression has no type to declare, no
// default to write and no ALTER to propose, so a migration that saw one would
// propose creating a column that must not exist and then propose dropping it
// again on the next run (ADR-0041).
func (t *TableDef) StoredFields() []*Field {
	out := make([]*Field, 0, len(t.fields))
	for _, f := range t.fields {
		if f.d.Computed() {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Field returns the named column, or nil.
func (t *TableDef) Field(name string) *Field {
	for _, f := range t.fields {
		if f.d.Name == name {
			return f
		}
	}
	return nil
}

// AddField adds one column to the table and returns it, so the declaration
// that adds a column and whatever needs to refer back to it can be the same
// statement:
//
//	status := Task.AddField(schema.Enum("status", "todo", "done").Default(schema.Value("todo")))
//	Task.AddAction(schema.Action{Writes: []string{status.Name()}})
//
// It exists beside schema.Table's variadic form rather than instead of it.
// Most columns want the compact, one-expression declaration every table in
// this codebase already uses; AddField is for the rare column something
// else — an Action.Writes, a cross-table reference — needs a handle to,
// without pulling every column in the table out to a named var to get one.
//
// Unlike AddIndex and the other Add* methods it returns the field rather
// than the table, on the grounds that a caller reaching for this already has
// the table and is here for the field.
func (t *TableDef) AddField(f *Field) *Field {
	t.fields = append(t.fields, f)
	return f
}

// AddFields is AddField for a spec that contributes more than one column —
// [Timestamps] and [SoftDelete] both do. It is what makes a reusable base
// column set (an audit trio, a soft-delete marker) applyable procedurally,
// the way schema.Table's variadic form already applies it declaratively:
//
//	func withAudit(t *TableDef) *TableDef {
//	    t.AddFields(Timestamps())
//	    return t
//	}
//
//	withAudit(Task)
//	withAudit(List)
func (t *TableDef) AddFields(specs ...FieldSpec) []*Field {
	var added []*Field
	for _, s := range specs {
		if s == nil {
			continue
		}
		fs := s.fields()
		t.fields = append(t.fields, fs...)
		added = append(added, fs...)
	}
	return added
}

// StoredField returns the named column if the database holds it, and nil for a
// computed one — which is what a migration wants: turning a stored column into
// a computed one means the storage goes away, and a diff that saw the
// declaration would leave the old column behind forever.
func (t *TableDef) StoredField(name string) *Field {
	if f := t.Field(name); f != nil && !f.d.Computed() {
		return f
	}
	return nil
}

// PrimaryKey returns the primary key column, or nil if the table declares none.
func (t *TableDef) PrimaryKey() *Field {
	for _, f := range t.fields {
		if f.d.PrimaryKey {
			return f
		}
	}
	return nil
}

// Relations returns the table's reference columns.
func (t *TableDef) Relations() []*Field {
	var out []*Field
	for _, f := range t.fields {
		if f.d.Ref != nil {
			out = append(out, f)
		}
	}
	return out
}

// Indexes returns the table's secondary indexes: the ones named at the table
// level, and the ones a column asked for with [Field.Indexed] where nothing
// else already covers the column.
func (t *TableDef) Indexes() []Index {
	fromColumns := t.columnIndexes()
	if len(fromColumns) == 0 {
		return t.indexes
	}
	// Column-declared first, which is where they were when Table added them, so
	// the order of a generated migration does not depend on when this moved.
	return append(fromColumns, t.indexes...)
}

// Checks returns the declared check constraints.
func (t *TableDef) Checks() []Check { return t.checks }

// Exclusions returns the declared EXCLUDE constraints.
func (t *TableDef) Exclusions() []Exclusion { return t.excls }

// CompositeKey returns the columns of a composite primary key, or nil when the
// table's key is a single column — which [TableDef.PrimaryKey] returns — or when
// it declares none.
//
// Named for what it holds rather than as the getter half of
// [TableDef.PrimaryKeyColumns], because PrimaryKey is already taken by the
// single-column accessor and Go has no overloading. The asymmetry is worth one
// odd name: every existing caller of PrimaryKey keeps working and sees nil,
// which is the behaviour a composite-key table wants from all of them.
func (t *TableDef) CompositeKey() []string { return t.pkCols }

// Rest returns the REST exposure, or nil if the table is not exposed.
func (t *TableDef) Rest() *REST { return t.rest }

// Comment returns the table description.
func (t *TableDef) Comment() string { return t.comment }

// PrimaryKeyName returns the pinned primary key constraint name, if any.
func (t *TableDef) PrimaryKeyName() string { return t.pkName }

// PrimaryKeyNamed pins the primary key constraint name, for adopting an
// existing database whose constraint is not called <table>_pkey.
func (t *TableDef) PrimaryKeyNamed(name string) *TableDef {
	t.pkName = name
	return t
}

// TypeNameOverride returns the pinned generated type name, or "" when none was
// set and codegen should derive one from the table's local name.
func (t *TableDef) TypeNameOverride() string { return t.typeName }

// TypeName pins the exported Go/TypeScript/Dart identifier codegen emits for
// this table, independently of its SQL name.
//
// Without it, the generated name is derived by singularising the local name —
// board_columns becomes BoardColumn — which can collide with a name a
// different table's codegen already derives for something else, such as a
// selectable-fields union codegen names after the table it belongs to. The
// storage name usually should not change to fix that: it is a live-data
// migration for a naming problem that has nothing to do with the data model.
// TypeName only changes what codegen calls the table; [TableDef.RenamedFrom]
// is for when the table itself was actually renamed.
func (t *TableDef) TypeName(name string) *TableDef {
	t.typeName = name
	return t
}

// RenamedFromName returns the table's previous storage name, or "".
func (t *TableDef) RenamedFromName() string { return t.oldName }

// RenamedFrom declares that this table used to be called local, so that a
// generated migration renames it rather than dropping it and creating a new
// one. See Field.RenamedFrom for why a rename is declared rather than inferred,
// and for how long the hint is needed.
//
// The old name is local, without the module prefix, and is qualified with the
// same prefix as the current one — so this renames a table within a module, not
// between modules. Moving a table between modules changes which registry
// declares it, and is a drop and a create until something asks for otherwise.
func (t *TableDef) RenamedFrom(local string) *TableDef {
	t.oldName = local
	if t.module != "" {
		t.oldName = t.module + "_" + local
	}
	return t
}

// Index adds a secondary index over the given columns, named by convention:
// posts_org_id_idx. Use [TableDef.IndexNamed] when the name matters.
func (t *TableDef) Index(columns ...string) *TableDef {
	t.indexes = append(t.indexes, Index{
		Name:    indexName(t.name, columns, false),
		Columns: columns,
	})
	return t
}

// UniqueIndex adds a composite unique index, named by convention:
// posts_org_id_slug_uniq. Use [TableDef.UniqueIndexNamed] when the name
// matters, which for a unique index it more often does — see below.
func (t *TableDef) UniqueIndex(columns ...string) *TableDef {
	t.indexes = append(t.indexes, Index{
		Name:    indexName(t.name, columns, true),
		Columns: columns,
		Unique:  true,
	})
	return t
}

// IndexNamed adds a secondary index under a name you choose, rather than the
// one the convention would derive.
//
//	t.IndexNamed("idx_projects_org_id", "org_id")
//
// It exists for adopting a database somebody else's tool built. A declared
// index whose name does not match the live one is a rename, and a schema of any
// size turns "declare the tables sqlb already agrees with" into "rename every
// index in the database" — which is a migration nobody asked for, on a database
// where it is the least welcome (issue #57).
//
// # An index name is not always inert
//
// Postgres reports a violated constraint by name, and matching that name is the
// standard way to tell one unique violation from another:
//
//	pgErr.Code == "23505" && pgErr.ConstraintName == "idx_projects_org_code"
//
// So renaming an index can turn a handled collision — retry with the next
// suffix — into an unhandled 500, without touching the code that handles it.
// That is the reason this is a declaration rather than a lint: the schema has
// to be able to say what the name *is*, not merely prefer it.
func (t *TableDef) IndexNamed(name string, columns ...string) *TableDef {
	t.indexes = append(t.indexes, Index{Name: name, Columns: columns})
	return t
}

// UniqueIndexNamed adds a composite unique index under a name you choose. See
// [TableDef.IndexNamed], and note that a unique index is the kind whose name an
// application is most likely to be matching on.
func (t *TableDef) UniqueIndexNamed(name string, columns ...string) *TableDef {
	t.indexes = append(t.indexes, Index{Name: name, Columns: columns, Unique: true})
	return t
}

// PrimaryKeyColumns declares a composite primary key over two or more columns.
//
// It is the table-level peer of [Field.PrimaryKey], and it exists because the
// alternative was a schema change: a table whose identity is a pair had to grow
// a surrogate UUID that nothing points at, plus an index to make the real key
// unique, purely so the declaration language could describe it. On a natural-key
// cache that is 16 bytes a row and an extra index, identifying something no
// other table references — a change nobody would defend if sqlb vanished
// tomorrow, which is the test an adopter applies (issue #109).
//
//	schema.Table("llmcatalog_models", ...).PrimaryKey("provider", "model_id")
//
// # What a composite key cannot do
//
// [TableDef.PrimaryKey] returns a *Field and returns nil for a table declared
// this way, so a composite-key table takes the same path as a keyless one
// everywhere row identity is assumed:
//
//   - it cannot be the target of [Ref] or [ExternalRef], because a reference is
//     single-column here too;
//   - it cannot be exposed over REST, because /{id} addresses one column;
//   - it cannot carry a non-collection [TableDef.AddAction], for the same reason.
//
// Those refusals are the point rather than a shortfall. The tables this is for
// — association tables where the pair *is* the row, and natural-key caches that
// are re-derivable and referenced by nothing — are not resources, and what they
// needed was to be *declarable*, so that one of them stops taking its whole
// module out of the drift gate.
//
// Use [TableDef.PrimaryKeyNamed] to pin the constraint's name.
func (t *TableDef) PrimaryKeyColumns(columns ...string) *TableDef {
	t.pkCols = columns
	return t
}

// AddExclude adds an EXCLUDE constraint. See [Exclusion].
func (t *TableDef) AddExclude(e Exclusion) *TableDef {
	t.excls = append(t.excls, e)
	return t
}

// ReplaceExclusion rewrites an already-declared exclusion's body and predicate,
// for the same caller and the same reason as [TableDef.ReplaceCheckExpr]:
// shadow.Normalize puts the declared spelling through Postgres and writes back
// what Postgres stores, so the two sides of a diff are comparable.
func (t *TableDef) ReplaceExclusion(name, using, elements, where string) bool {
	for i := range t.excls {
		if t.excls[i].Name != name {
			continue
		}
		t.excls[i].Using = using
		t.excls[i].Elements = elements
		t.excls[i].Where = where
		return true
	}
	return false
}

// AddIndex adds a fully specified index, for cases the shorthands do not cover
// such as GIN indexes or partial indexes.
func (t *TableDef) AddIndex(idx Index) *TableDef {
	if idx.Name == "" {
		idx.Name = indexName(t.name, idx.Columns, idx.Unique)
	}
	t.indexes = append(t.indexes, idx)
	return t
}

// Check adds a table-level check constraint.
func (t *TableDef) Check(name, expr string) *TableDef {
	t.checks = append(t.checks, Check{Name: name, Expr: expr})
	return t
}

// Unique adds a composite UNIQUE constraint, named the way Postgres names one
// itself: secrets_tenant_kind_tenant_id_name_key.
//
//	t.Unique("tenant_kind", "tenant_id", "name")
//
// # Why this is not UniqueIndex
//
// [TableDef.UniqueIndex] renders CREATE UNIQUE INDEX, which enforces the same
// rule through a different object. Two of those differences are load-bearing:
// a unique index cannot be the target of FOREIGN KEY … REFERENCES t (a, b),
// and it cannot be named in ON CONFLICT ON CONSTRAINT. `UNIQUE (a, b)` written
// inline in CREATE TABLE is also what a hand-written migration reaches for by
// default, so a database being adopted usually has the constraint.
//
// Declaring the index where the database has the constraint is therefore not a
// near-miss that diffs to nothing. It diffs to a drop and a rebuild, which is a
// real migration on live data forced by the declaration language rather than by
// anything the schema needs (issue #108).
//
// Use [TableDef.UniqueNamed] when the live name does not follow the
// convention — which for a constraint an application may be matching on by
// name, the same way [TableDef.IndexNamed] describes.
func (t *TableDef) Unique(columns ...string) *TableDef {
	t.uniques = append(t.uniques, Unique{
		Name:    uniqueConstraintName(t.name, columns),
		Columns: columns,
	})
	return t
}

// UniqueNamed adds a composite UNIQUE constraint under a name you choose,
// rather than the one the convention would derive. See [TableDef.Unique].
func (t *TableDef) UniqueNamed(name string, columns ...string) *TableDef {
	t.uniques = append(t.uniques, Unique{Name: name, Columns: columns})
	return t
}

// AddUnique adds a composite UNIQUE constraint given whole, which is the form
// that reaches the fields the two shorthands above do not — currently
// [Unique.Deferrable].
//
//	AddUnique(schema.Unique{
//	    Name:       "variants_product_option_combination_key",
//	    Columns:    []string{"product_id", "option_signature"},
//	    Deferrable: schema.DeferredCheck,
//	})
//
// An empty Name takes the one the convention derives, so the constraint is
// still recognised in a database that has it under Postgres's own spelling.
func (t *TableDef) AddUnique(u Unique) *TableDef {
	if u.Name == "" {
		u.Name = uniqueConstraintName(t.name, u.Columns)
	}
	t.uniques = append(t.uniques, u)
	return t
}

// Uniques returns the table-level unique constraints.
func (t *TableDef) Uniques() []Unique { return t.uniques }

// uniqueConstraintName derives the name Postgres would have given the
// constraint, which is what makes an adopted database diff to nothing.
func uniqueConstraintName(table string, columns []string) string {
	return table + "_" + strings.Join(columns, "_") + "_key"
}

// ReplaceIndexWhere rewrites a partial index's predicate, for the same reason
// and by the same caller as ReplaceCheckExpr below.
//
// A partial-index predicate is stored the way a CHECK is — as a parse tree,
// rendered back by pg_get_expr — so `latitude IS NOT NULL` comes back as
// `(latitude IS NOT NULL)` and a declaration written the obvious way never
// matches the live index. The diff then proposes creating an index that is
// already there, with DDL that looks identical to what the database holds
// (issue #63).
//
// ReplaceCheckExpr rewrites the expression of an already-declared check, and
// reports whether there was one by that name.
//
// This exists for one caller and it is worth naming, because a setter on a
// declaration is otherwise a smell. Postgres does not store a CHECK expression
// as it was written: it stores a parse tree, and hands back a normalised
// spelling — fully parenthesised, with explicit casts on literals. So a
// registry read back by introspect and a registry declared here disagree about
// every check they have in common, and a diff between them proposes dropping
// and re-adding each one forever (issue #24).
//
// The only reliable way to compare them is to put the declared expression
// through the same normalisation, which means asking a Postgres. That is what
// shadow.Normalize does, and this is how it writes the answer back.
// Comparing the two spellings textually instead was rejected: stripping
// parentheses can make two genuinely different expressions look equal, and a
// diff that reports "unchanged" for a changed constraint is silently wrong,
// where churn is merely loud.
func (t *TableDef) ReplaceIndexWhere(name, expr string) bool {
	for i := range t.indexes {
		if t.indexes[i].Name == name {
			t.indexes[i].Where = expr
			return true
		}
	}
	return false
}

func (t *TableDef) ReplaceCheckExpr(name, expr string) bool {
	for i := range t.checks {
		if t.checks[i].Name == name {
			t.checks[i].Expr = expr
			return true
		}
	}
	return false
}

// Describe attaches a table description, emitted into DDL and OpenAPI.
func (t *TableDef) Describe(s string) *TableDef {
	t.comment = s
	return t
}

// Expose publishes the table over HTTP. Without this call the table is
// reachable from Go code but has no REST surface at all.
func (t *TableDef) Expose(r REST) *TableDef {
	if r.Path == "" {
		// The URL uses the local name: a module prefix is a storage concern,
		// and leaking it into the API would make moving a table between
		// modules a breaking API change.
		r.Path = "/" + t.local
	}
	if r.Tag == "" {
		r.Tag = t.local
	}
	t.rest = &r
	return t
}

func indexName(table string, columns []string, unique bool) string {
	kind := "idx"
	if unique {
		kind = "uniq"
	}
	return table + "_" + strings.Join(columns, "_") + "_" + kind
}
