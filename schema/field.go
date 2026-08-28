package schema

import (
	"fmt"
	"strings"
)

// Field is a column under construction. Its methods are chainable setters, so
// the DSL reads as a declaration:
//
//	schema.Text("email").Unique().Searchable()
//
// Code generators read the result through Desc.
type Field struct {
	d FieldDesc
}

// FieldDesc is the resolved description of a column: everything a generator or
// the runtime needs to know about it.
type FieldDesc struct {
	Name string
	// Type names the *element* type when Array is set, and the column type
	// otherwise. Keeping the element rather than fusing the two is what lets
	// the filter parser bind `?tags=has.urgent` as a text value, and what keeps
	// EnumValues and Size attached to something that has them (ADR-0033).
	Type Type
	// Array makes the column a one-dimensional Postgres array of Type.
	Array bool
	Size  int // varchar length, or a numeric's precision; 0 means unbounded
	// Scale is a numeric's scale — the digits after the point. Meaningful only
	// beside a Size, since numeric(s) is not a thing: a numeric declares a
	// precision or nothing.
	Scale int
	// Dim is the number of components in a TypeVector column, and is part of
	// the type rather than a constraint on it: Postgres will not store a
	// 768-component value in a vector(1536). Zero for every other type.
	//
	// It is a Go expression at the call site, which is the whole answer to the
	// substitution sentinel a migration file needs otherwise — the dimension
	// wants to be a value and SQL text has nowhere to put one (ADR-0026).
	Dim     int
	Comment string

	Nullable   bool
	PrimaryKey bool
	Unique     bool
	Default    *Default
	EnumValues []string

	// Pattern is a regular expression a text value must match, and Min and Max
	// bound a numeric one inclusively. All three are checked at the API
	// boundary and nowhere else: they reach the generated request bodies as
	// the struct tags Huma already reads, and from there the OpenAPI document.
	//
	// They are *format* rules about a value, which is what makes them belong
	// in the same vocabulary as the type and the enum values. They are not
	// database constraints — nothing writes a CHECK from them — so a row
	// arriving by any path but a request is unconstrained by them. Use
	// [TableDef.Check] for a rule the database must hold (#311).
	//
	// Min and Max are float64 because a JSON Schema bound is a JSON number,
	// and the clients that enforce it hold one in a double. A bound beyond
	// 2^53 cannot be stated exactly here for the same reason it cannot be
	// stated exactly in the document; a range that wide wants a Check.
	Pattern string
	Min     *float64
	Max     *float64

	// SharedAs names the Go type codegen emits for an enum column, and
	// declares that type shared: every other enum column in the registry
	// carrying the same SharedAs name emits no type of its own and uses this
	// one instead, rather than the table-and-column name codegen derives by
	// default. See [Field.SharedAs].
	SharedAs string

	// UniqueDeferrable is when the column's own unique constraint is checked.
	// The zero value is NOT DEFERRABLE. See [Field.Deferred] and, for the
	// table-level constraint this mirrors, [Unique.Deferrable].
	UniqueDeferrable Deferrable

	// Auto makes the database supply the column's value: a sequence, or an
	// identity. Only an integer column may carry one, and never beside a
	// Default — see [Auto], and [Serial] and [Field.Identity] for the two
	// spellings.
	Auto Auto

	// ScopeName groups this column with the other columns answering the same
	// question, for the hooks codegen emits. Empty means the column name is the
	// grouping key, which is right for a reference to the tenant table and is
	// refused on a primary key. See [Field.Scoped].
	ScopeName string

	// Capabilities. Each is opt-in and gates one specific REST affordance.
	Filterable bool // may be used in a REST filter expression
	Sortable   bool // may appear in ?sort
	Searchable bool // included in the ?search fan-out
	Expandable bool // relation may be pulled in via ?expand (references only)

	// SortNulls is where NULLs sit whenever ?sort names this column, in either
	// direction. Empty leaves Postgres's default, which is NULLS LAST for
	// ascending and NULLS FIRST for descending — not one placement but two, so
	// a column whose NULLs mean something cannot rely on it (#88).
	//
	// It is the same Nulls the index orders use, and deliberately so: a
	// resource sorted `published_at DESC NULLS LAST` wants the index declared
	// the same way, and one vocabulary makes the pair legible as a pair.
	SortNulls Nulls

	// Write protection, enforced by the REST layer. Go code going through the
	// query engine directly is trusted and bypasses these.
	ReadOnly  bool // never settable through REST
	Immutable bool // settable at create, rejected on update
	Hidden    bool // never serialised into a REST response

	// WriteOnly is Hidden's converse: never serialised into a REST response,
	// but — unlike Hidden — still settable through the generated create and
	// update bodies, and still present in the typed column facade so trusted
	// Go code can read and query it. See [Field.WriteOnly].
	WriteOnly bool

	// LookupKey says a Hidden column is found by its own value, and so keeps
	// its entry in the generated typed-column facade.
	//
	// It changes nothing about the REST surface and nothing at runtime — it is
	// not in [FieldDesc.Capabilities], because there is nothing for the engine
	// to read: `sqlb.F("token_hash")` has always worked. What it changes is
	// whether the compiler helps, and what the generated file says about which
	// kind of secret this column is. See [Field.LookupKey].
	LookupKey bool

	// Obligations. Neither of these changes a query. They are read once, at
	// startup, where rest refuses to mount a resource whose declarations have
	// no hook behind them; nothing on the request path reads either one.
	Scoped     bool // every exposed operation must be constrained by a hook
	SoftDelete bool // the column a soft-delete predicate is expected to filter

	// indexWanted is [Field.Indexed]: a single-column btree on this column,
	// named by the same convention the table-level form uses. It is a
	// declaration like any other — nothing infers it from the column's shape,
	// which is what keeps a registry read out of a database from claiming an
	// index that database does not have (#259).
	indexWanted bool

	// ConstraintName pins the name of the constraint this column declares —
	// its unique constraint, or its foreign key if it is a reference. Set it
	// when adopting an existing database, so that a generated migration
	// recognises the constraint already there instead of dropping and
	// recreating it under a name of its own choosing.
	ConstraintName string

	// CheckName pins the name of the CHECK an enum column emits, which is a
	// second constraint on the same column and so cannot share the field above:
	// a column may be unique *and* an enum, and one name cannot serve both.
	//
	// An enum is text plus a CHECK (ADR-0017), and the check's name is the one
	// thing about it that introspection cannot recover from the expression. Left
	// unpinned, a database whose check is called chk_org_plan is rebuilt with
	// one called orgs_plan_check — so a diff against it proposes dropping and
	// re-adding that constraint on every run, forever (issue #53).
	CheckName string

	// RenamedFrom is the column's previous name, declared for one release so
	// that a migration renames the column instead of dropping and re-adding
	// it. Nothing else reads it.
	RenamedFrom string

	// Expr is the SQL a computed column renders as, in place of its name, and
	// it is the one thing about a column that no struct tag can carry: a tag is
	// a comma-separated list and SQL is not. Codegen writes it into a
	// ComputedColumns method instead (ADR-0041).
	//
	// A column with an expression stores nothing. It emits no DDL in either
	// direction, no insert names it and no update sets it — and it is a column
	// everywhere else, so Hidden hides it, Filterable gates it, and it lands in
	// the row type, the JSON, the client types and the CLI like any other.
	Expr string
	// Needs names the binds Expr's `?` placeholders take, in order. A computed
	// column with none is row-local. One with a bind is answered per request,
	// and the value arrives through Builder.Bind — which rest refuses to mount
	// a resource without.
	Needs []string

	Ref *Reference
}

// Reference describes a relationship to another table.
//
// A reference is either internal — a real foreign key to a table in the same
// registry — or external, which is a column holding another module's identifier
// with no database-level constraint behind it.
type Reference struct {
	Name     string // relation name, e.g. "org" for column "org_id"
	Table    *TableDef
	Column   string // referenced column; defaults to the target primary key
	OnDelete RefAction
	OnUpdate RefAction

	// External marks a reference across a module boundary. No FOREIGN KEY is
	// emitted for one, so the modules stay independently deployable and
	// independently migratable. Referential integrity becomes the
	// application's responsibility, which is the trade a module architecture
	// is already making everywhere else.
	External bool
	// Target names what is referenced, for documentation and for the
	// manifest: "tenants.id", or "platform/users.users.id". It is free text,
	// deliberately — resolving it would require the dependency this is
	// designed to avoid.
	Target string
	// Enforced turns an external reference into a real FOREIGN KEY to the
	// table Target names, without resolving that table's declaration. It is
	// the case an incremental adoption lives in: the database has a live,
	// enforced constraint and the table it points at has not been declared yet
	// (issue #55). See Field.Enforced.
	Enforced bool

	// Inverse is the name the target knows this relation by, and declaring it
	// is what makes the reverse relation exist at all.
	//
	// It cannot be derived. Two references from posts to authors — the writer
	// and the reviewer — would both derive to "posts" on the far side, and an
	// author's posts are not the posts an author reviewed. The distinction
	// exists only in the head of whoever wrote the schema, so the schema is
	// where it has to be written. ADR-0022.
	Inverse string
	// InverseExpandable exposes the reverse relation through ?expand on the
	// target's endpoint. It is a separate decision from Expandable, about a
	// different endpoint, and neither implies the other — ADR-0006.
	InverseExpandable bool
	// InverseOrder is the column an expanded collection is ordered by, with a
	// leading "-" for descending. It names a column of *this* table, since
	// these are the rows being collected. Empty means the primary key.
	InverseOrder string
	// InverseLimit caps an expanded collection. Zero takes sqlb's default.
	InverseLimit int
}

// Desc returns the column description. The pointer aliases the field's own
// state, so generators must treat it as read-only.
func (f *Field) Desc() *FieldDesc { return &f.d }

// Name is the column name.
func (f *Field) Name() string { return f.d.Name }

// FieldSpec is anything contributing columns to a table. Both *Field and the
// grouped helpers (Timestamps, SoftDelete) implement it, so they mix freely in
// a single Table call.
type FieldSpec interface {
	fields() []*Field
}

// Group is an ordered set of fields inserted into a table as a unit. Use it to
// factor recurring column sets out of a schema.
type Group []*Field

func (g Group) fields() []*Field { return g }

func (f *Field) fields() []*Field { return []*Field{f} }

func newField(name string, t Type) *Field {
	return &Field{d: FieldDesc{Name: name, Type: t}}
}

// Column type constructors.

func Text(name string) *Field   { return newField(name, TypeText) }
func Int(name string) *Field    { return newField(name, TypeInt) }
func BigInt(name string) *Field { return newField(name, TypeBigInt) }
func Float(name string) *Field  { return newField(name, TypeFloat) }

// SmallInt is the 2-byte integer, Go int16.
//
// It is a sibling of [Int] rather than a width argument to it, which is how
// [BigInt] is already spelled. A schema that could not say `smallint` had to
// widen the column to `integer` to be declarable at all — a schema change whose
// only justification is the declaration language, which is exactly the change
// an adopter cannot defend (issue #114).
//
// It filters, sorts and orders exactly as [Int] does; there are no capability
// semantics of its own.
func SmallInt(name string) *Field { return newField(name, TypeSmallInt) }

// Serial is the 4-byte auto-incrementing integer, Go int32, `serial` — one of
// three alongside [BigSerial] (`bigserial`, Go int64) and [SmallSerial]
// (`smallserial`, Go int16).
//
//	schema.BigSerial("id").PrimaryKey()
//
// They are siblings of [Int], [BigInt] and [SmallInt] rather than a modifier on
// them, for the same reason [SmallInt] is a sibling of [Int]: the width is part
// of what is being declared and reads better in front. The Go type, the filter
// grammar and the sort machinery are the plain integer's — a serial *is* a
// bigint, which is why [Auto] is a property of the column and not a [Type].
//
// A serial is what an existing database has, and being unable to declare one
// left no auto-incrementing integer key expressible at all: an adoption's only
// options were to widen the key to a UUID — an API change and 16 bytes a row on
// the highest-volume tables in a system — or to leave the table, and every
// module holding it, outside the gate (issue #132).
//
// [Field.Identity] is the modern spelling of the same idea and is what Postgres
// now recommends. Prefer it for a new column; declare a serial when the
// database already has one, because changing between them is a migration rather
// than a rename.
//
// The column is not nullable and takes no [Field.Default]: the sequence is the
// default, and Postgres refuses both combinations.
func Serial(name string) *Field { return serial(name, TypeInt) }

// BigSerial is the 8-byte auto-incrementing integer, Go int64. See [Serial].
func BigSerial(name string) *Field { return serial(name, TypeBigInt) }

// SmallSerial is the 2-byte auto-incrementing integer, Go int16. See [Serial].
func SmallSerial(name string) *Field { return serial(name, TypeSmallInt) }

func serial(name string, t Type) *Field { return newField(name, t).Serial() }

// Serial makes an integer column draw its value from a sequence Postgres
// creates and owns — the modifier form of [Serial], [BigSerial] and
// [SmallSerial], which is the same relationship [UUIDv7] has to [UUID].
//
// Reach for the constructors when writing a schema by hand; this is what an
// import uses, where the type is decided before the column's auto-ness is read.
func (f *Field) Serial() *Field {
	f.d.Auto = AutoSerial
	return f
}

// Real is the 4-byte float, Go float32.
//
// The peer of [SmallInt] in the float family, and filed for the same reason: a
// model-confidence score or any other value never compared for equality is what
// `real` is for, and widening it to `double precision` to suit the DSL is a
// schema change the adopter cannot justify on its own merits (issue #120).
func Real(name string) *Field { return newField(name, TypeReal) }

// Numeric is an exact decimal. Called with no arguments it is unbounded —
// `numeric` — which is what a rate, a rating or anything else that wants
// arbitrary precision should be.
//
// Given a precision and a scale it renders `numeric(p, s)`, which is a
// *different type* from the unbounded one and the only faithful way to declare
// a column an existing database already has that way. A schema that could not
// say so had two bad options: declare it unbounded and hold a permanent
// `add column` waiver in the drift gate, or leave the column out and have the
// model, the REST surface and the generated clients silently lack a field the
// hand-written API carries (issue #81).
//
//	schema.Numeric("rating")                     // numeric
//	schema.Numeric("contracted_hours", 5, 2)     // numeric(5, 2)
//
// A precision alone is legal Postgres — `numeric(5)` means `numeric(5, 0)` —
// and is accepted here as the one-argument form. More than two arguments is a
// declaration error, reported when the field is resolved rather than silently
// truncated.
func Numeric(name string, precision ...int) *Field {
	f := newField(name, TypeNumeric)
	switch len(precision) {
	case 0:
	case 1:
		f.d.Size = precision[0]
	case 2:
		f.d.Size, f.d.Scale = precision[0], precision[1]
	default:
		// Panic, like the other declaration errors here: a schema is Go, this
		// runs at init, and a wrong declaration should not compile into a
		// process that then writes DDL from it.
		panic(fmt.Sprintf("sqlb/schema: Numeric(%q) takes a precision and an optional scale, got %d arguments",
			name, len(precision)))
	}
	return f
}
func Bool(name string) *Field      { return newField(name, TypeBool) }
func UUID(name string) *Field      { return newField(name, TypeUUID) }
func Timestamp(name string) *Field { return newField(name, TypeTimestamp) }
func Date(name string) *Field      { return newField(name, TypeDate) }
func Time(name string) *Field      { return newField(name, TypeTime) }
func JSON(name string) *Field      { return newField(name, TypeJSON) }
func Bytes(name string) *Field     { return newField(name, TypeBytes) }

// Varchar is a length-bounded text column.
func Varchar(name string, size int) *Field {
	f := newField(name, TypeVarchar)
	f.d.Size = size
	return f
}

// Vector is a pgvector embedding of dim components.
//
// The dimension is an ordinary Go expression, so it can come from wherever the
// embedder's width comes from:
//
//	schema.Vector("embedding", ragcfg.Dim)
//
// That is deliberately stricter than reading it from the environment at
// startup, which is what a project does when the schema cannot hold it: the
// dimension is fixed when the code is generated, so one binary can no longer
// serve a 768- and a 1,536-component embedder. What it buys is that the
// dimension is in the declaration, so `Diff` proposes a migration when it
// changes instead of a comment asking someone to remember (ADR-0026).
//
// The column is Hidden and not optionally so. An embedding is twenty kilobytes
// of float that no client has a use for, and serialising one by accident is the
// kind of mistake that shows up as a bandwidth bill. Go callers reading through
// the query engine still get it.
//
// A vector is storable and orderable and nothing else yet: there is no index
// kind, no metric declaration and no REST search operation, which ADR-0026
// stages as a second decision to be taken when a corpus outgrows an exact scan.
// Until then a similarity search is an exact scan over the rows a filter
// already selected, which is the shape the module this was designed against
// actually runs.
func Vector(name string, dim int) *Field {
	f := newField(name, TypeVector)
	f.d.Dim = dim
	f.d.Hidden = true
	return f
}

// Computed declares a derived column: an expression the query renders in place
// of a column name, rather than a value the table stores.
//
//	schema.Computed("is_overdue", schema.TypeBool,
//	    schema.FromSQL("due_date < current_date AND open_tasks > 0")).
//	    Filterable().Sortable()
//
// It is a column everywhere it matters — it lands in the row type, the JSON,
// the TypeScript and Dart types and the CLI column set, and Hidden, Filterable
// and Sortable gate it exactly as they gate a stored one. What it is not is
// storage: it emits no DDL in either direction, Diff does not see it, no insert
// names it and no update assigns it. ADR-0041 has the shape and the reasons.
//
// # Nullability runs the other way
//
// A computed column is [Field.Nullable] unless it says otherwise, which is the
// opposite of a stored one and the same as SQL: a correlated subquery that
// matches nothing is NULL, an arithmetic expression over a nullable column is
// NULL, and a comparison against one is NULL. A stored column reads its
// nullability off `NOT NULL` in the DDL and the round trip checks it; an
// expression has no DDL, so the default is doing all the work, and the default
// that assumed otherwise failed at scan time on the first row with nothing to
// match — a 500 with `cannot scan NULL into *string`, from a declaration
// `sqlb generate` and the drift gate were both happy with (#147).
//
// [Field.NotNull] is the opt-in for an expression that cannot produce one:
//
//	schema.Computed("total_tasks", schema.TypeInt,
//	    schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)")).
//	    NotNull()
//
// It is a claim, not a check — nothing parses the SQL — and it fails in the
// direction the default does not: a nullable column typed as a pointer scans a
// non-null value fine, where the reverse is the 500.
//
// # What each form may claim
//
// The expression is rendered as written, so a name in it resolves the way
// Postgres resolves it. In a statement that joins — one with `?expand` in it —
// a bare column name shared with the joined table is ambiguous and Postgres
// says so; qualify it with the table's own name when that is a possibility.
//
// A row-local expression may be Filterable and Sortable, since the compiler can
// put it in a WHERE and an ORDER BY as readily as in the projection:
//
//	schema.Computed("is_overdue", schema.TypeBool,
//	    schema.FromSQL("due_date < current_date AND open_tasks > 0")).Filterable()
//
// A correlated subquery is projection-only unless Filterable is written out,
// because a subquery in a WHERE runs once per row — the declaration is the
// acknowledgement that this was considered:
//
//	schema.Computed("total_tasks", schema.TypeInt,
//	    schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)"))
//
// A parameterised expression takes its value from the request. Each `?` binds
// the key named at the matching position of Needs, and `??` is a literal
// question mark:
//
//	schema.Computed("is_starred", schema.TypeBool,
//	    schema.FromSQL("EXISTS (SELECT 1 FROM stars s "+
//	        "WHERE s.project_id = projects.id AND s.member_id = ?)")).
//	    Needs("viewer").Filterable()
//
// Needs writes no value, exactly as [Field.Scoped] writes no predicate. What it
// does is oblige a hook: rest.Resource refuses to mount the resource until a
// BeforeQuery hook calls Bind for every key it names. Without that check an
// unbound expression renders `member_id = NULL`, returns false for every row
// forever, and looks precisely like a feature that works (ADR-0030).
//
// # What it will not accept
//
// Sortable over a volatile expression — one reading now() or random() —
// because a keyset pages on the sort column and an unstable one lets page 1 and
// page 50 disagree about a row.
// A default, a primary key, a unique constraint, a reference or an index, all
// of which are statements about storage. Validate reports each of them.
//
// Searchable used to be on this list and is not any more. The reason given was
// that "?search fans out over text columns with ILIKE and an expression has no
// reading there" — which is a claim about *type*, and the rule that Searchable
// requires a text column already makes it, for stored and computed columns
// alike. What the blanket refusal actually cost was the only way to search
// across a relation, since a chat named by its participants has no name column
// of its own to fan out over (#93). The cost objection — a correlated subquery
// per candidate row — is answered where cost belongs: a resource searches an
// expression only if it selected it (#92).
func Computed(name string, t Type, e ComputedExpr) *Field {
	f := newField(name, t)
	f.d.Expr = e.sql
	// Nothing can write an expression, and saying so here rather than asking
	// every write path to check is what keeps the generated create and update
	// bodies correct without knowing this feature exists.
	f.d.ReadOnly = true
	// Nullable by default, which is the opposite of a stored column and the
	// same as SQL. See the doc comment above for the argument.
	f.d.Nullable = true
	return f
}

// ComputedExpr is how a computed column is produced. Build one with FromSQL.
//
// It is a type rather than a bare string so that the declaration reads as a
// choice. FromSQL is the only one, and now the only one there will be: ADR-0041
// staged a Go-side FromGo as a separate decision and then cut it, on the trigger
// that record set for itself — the first two applications expressed every
// derived value in SQL (#17). The type stays because a constructor is still the
// right shape for the argument, and because reopening it is additive.
type ComputedExpr struct{ sql string }

// FromSQL computes a column from a SQL expression over the row's own columns.
//
// The expression is raw SQL and nothing parses it: `sqlb generate` refuses the
// declarations that are wrong on their face — a volatile Sortable one, a bind
// count that disagrees with Needs, a Searchable one whose type is not text —
// but a typo inside the SQL reaches Postgres. That is the cost [ADR-0024]'s bar
// admits here because there is finally a consumer for the annotation, and
// [sqlb.Builder.Explain] against a real database is what catches it early.
//
// # Whose table does it name
//
// Nothing checks that either, and it is the question worth asking before
// writing a subquery here — not "is this a subquery" but "whose table does it
// name". A subquery over this module's own tables is what this feature is for:
// a chat's `participant_ids` over its own `chat_members` is correct and deletes
// an N+1. A subquery naming *another module's* table is the coupling
// [ExternalRef] refuses to expand for, arriving through a door nothing guards.
//
// The difference from a join in one handler is the footprint, and it is larger
// than it looks. A computed column travels with the model:
//
//   - it is selectable by every mount that opts into it, so the coupling is not
//     confined to the one query that wanted the value;
//   - it is in the RETURNING of every INSERT, UPDATE and — with
//     [sqlb.Hooks.AfterDeleteRows] — DELETE that asks for it.
//
// "The coupling is identical to the LEFT JOIN I am replacing" is the plausible
// and wrong reasoning a port applies. It is not identical: a module that
// replaced `LEFT JOIN projects` in one handler with
// `Computed("project_name", FromSQL("(SELECT name FROM projects …)"))` found the
// subquery in the RETURNING of every insert, so the table could not be written
// at all without `projects` present, and the module's own isolation boot test
// failed on its seed with `relation "projects" does not exist`
// ([#167](https://github.com/mind-vm/sqlb/issues/167)). Since
// [#164](https://github.com/mind-vm/sqlb/issues/164) a write evaluates only the
// computed columns it asked for, which shrinks that footprint — but the read
// path still carries the coupling to every mount that selects the column, so the
// rule stands.
//
// The answer is [ExternalRef]'s own: fetch the other side through that module's
// API. sqlb cannot check this — resolving a table name out of raw SQL is exactly
// the dependency ExternalRef's free-text target exists to avoid — which is why
// it is written here rather than enforced.
//
// [ADR-0024]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#no-annotation-slot
func FromSQL(sql string) ComputedExpr { return ComputedExpr{sql: sql} }

// Needs names the binds this column's expression takes, in the order its `?`
// placeholders appear. See [Computed].
func (f *Field) Needs(keys ...string) *Field {
	f.d.Needs = append(f.d.Needs, keys...)
	return f
}

// Enum is a text column constrained to a fixed set of values. Codegen emits a
// Go string type with one constant per value.
func Enum(name string, values ...string) *Field {
	f := newField(name, TypeEnum)
	f.d.EnumValues = values
	return f
}

// SharedAs names the Go type codegen emits for this enum column, and opts
// into sharing it: any other enum column in the registry that declares the
// same SharedAs name uses this type too, instead of a type of its own.
//
// Left undeclared, codegen names an enum's type after the table and column —
// LessonStatus, CourseStatus — even when two columns declare the identical
// value set. That is two structurally identical, nominally incompatible Go
// types, and reusing the value between them costs a needless string(x) round
// trip for no reason the schema expresses (issue #197). SharedAs is the
// answer, and an explicit one rather than an inferred one: two columns
// happening to declare the same values today does not mean they are the same
// enum — a lesson's status and a notification's priority could both be
// {"low","medium","high"} and mean nothing in common — so nothing here
// deduplicates by matching value sets on its own. The author says once, on
// every column meant to share:
//
//	schema.Enum("status", "draft", "published", "archived").SharedAs("Status")
//
// declared identically — same values, same order — on each participating
// column. codegen emits exactly one `type Status string` and one block of
// `StatusDraft`, `StatusPublished`, `StatusArchived` constants, and every
// column's model field is typed Status; [Registry.Validate] refuses the
// schema, naming both columns, if two declarations under the same SharedAs
// name disagree about the order or the values.
//
// name must be an exported Go identifier: it becomes a type name in the
// generated package, alongside every other type codegen names for you.
//
// This is a codegen-only declaration (ADR-0010). It changes no DDL and
// nothing about how the column is reached through sqlb.F, a struct tag, or
// the filter grammar — a Go string is a Go string with or without a shared
// name on it, and the type this names only comes to exist once `sqlb
// generate` runs.
func (f *Field) SharedAs(name string) *Field {
	f.d.SharedAs = name
	return f
}

// UUIDv7 is the conventional primary key column: a UUID defaulting to a
// generated, time-ordered v7 value.
func UUIDv7(name string) *Field {
	f := newField(name, TypeUUID)
	f.d.Default = GenUUIDv7()
	return f
}

// Ref declares a foreign key to target. The column is named name+"_id" and the
// relation is named name, so Ref("org", Org) yields column "org_id" reachable
// as ?expand=org once marked Expandable.
func Ref(name string, target *TableDef) *Field {
	f := newField(name+"_id", TypeUUID)
	f.d.Ref = &Reference{Name: name, Table: target, OnDelete: NoAction, OnUpdate: NoAction}
	if target != nil {
		if pk := target.PrimaryKey(); pk != nil {
			f.d.Type = pk.Desc().Type
			f.d.Ref.Column = pk.Desc().Name
		}
	}
	return f
}

// ExternalRef declares a reference to a table this module does not own.
//
// It produces a column named relation+"_id" holding the other side's
// identifier, and an index to join on — but no FOREIGN KEY, so the two modules
// can be migrated and deployed independently, and either can be moved to its
// own database without dropping a constraint:
//
//	// in the billing module, with no import of the tenants module
//	schema.ExternalRef("tenant", "tenants.id").Filterable()
//
// The target is free text. Resolving it to a real table would require exactly
// the dependency this exists to avoid, so it is recorded for the manifest and
// for whoever reads the schema, and not checked.
//
// Such a reference cannot be Expandable: expanding it would join a table this
// module does not own. Fetch the other side through that module's own API.
//
// That refusal is a rule about the coupling, not about the syntax, so it applies
// to the other way of writing the same join: a [Computed] column whose [FromSQL]
// expression names the other module's table couples every read of this table to
// that module's presence, and nothing refuses it because nothing resolves a
// table name out of raw SQL. FromSQL's doc comment has the case that proved it.
//
// # relation, not column
//
// The first argument is the *relation* — the column is named after it, with
// "_id" appended. So ExternalRef("org", …) declares a column called org_id, and
// ExternalRef("org_id", …) declares one called org_id_id. Use [Field.Named] if
// the column is spelled some other way.
func ExternalRef(relation, target string) *Field {
	f := newField(relation+"_id", TypeUUID)
	f.d.Ref = &Reference{Name: relation, Target: target, External: true}
	return f
}

// Enforced makes an external reference emit a real FOREIGN KEY.
//
//	schema.ExternalRef("org", "organizations.id").Enforced().Filterable()
//
// It is for the case an incremental adoption always reaches: the database has a
// live, enforced foreign key, and the table it points at has not been declared
// yet. Neither existing spelling covers that — [Ref] needs the target's
// *TableDef, and a plain [ExternalRef] emits no constraint, so a
// schema-vs-database diff reports the live one as something to drop and, if
// sqlb owned the DDL, would propose actually dropping it (issue #55).
//
// The target is still not resolved: it is a name, and the constraint is emitted
// against that name. Three forms are accepted — "organizations.id" names the
// table and the column, a bare "organizations" means its "id", and
// "auth.users.id" names a table in another schema of the same database. A
// module-qualified target ("platform/users.users.id") cannot be enforced,
// because a constraint has to name a table in this database.
//
// The schema-qualified form is for an application that shares its database
// with a platform's own schemas and does not own them — a Supabase project's
// public tables referencing auth.users is the case that turns up first, and
// [introspect] imports such a key back as this same spelling, so the round
// trip is a fixpoint rather than a constraint the next diff proposes dropping.
// It obliges every database the DDL reaches to already have that schema in it,
// including the scratch database `sqlb migrate` replays the history into:
// nothing here creates it, and the replay fails on the migration that first
// names it. docs/supabase.md is the arrangement that keeps the two alike.
//
// # What this gives up
//
// Everything [ADR-0015] bought by refusing the constraint. Two modules joined
// by an enforced reference can no longer be migrated or deployed independently,
// and neither can be moved to its own database without dropping it. That is the
// right trade when both tables are in one database and the constraint is
// already there — which is exactly the adoption case — and the wrong one across
// a module boundary you intend to keep.
//
// Expansion is still refused. A real constraint says the row exists; it does
// not give this schema the target's columns, so `?expand` has nothing to build
// a join from.
//
// [introspect]: https://pkg.go.dev/github.com/mind-vm/sqlb/introspect
// [ADR-0015]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#module-isolation
func (f *Field) Enforced() *Field {
	if f.d.Ref != nil {
		f.d.Ref.Enforced = true
	}
	return f
}

// EnforcedTarget resolves an enforced external reference's target into the
// schema, table and column a FOREIGN KEY names.
//
// "organizations.id" is a table and a column; a bare "organizations" is that
// table's "id", which is the convention every other part of this DSL already
// assumes; and "auth.users.id" names the schema the table lives in as well.
// schemaName is empty for the two unqualified forms, which leaves the target to
// the search path exactly as every other name this package renders does.
//
// Anything else — a module-qualified target, an empty one, more than two dots —
// reports false, and Validate turns that into an error naming the forms rather
// than emitting a constraint against a guess.
func (r *Reference) EnforcedTarget() (schemaName, table, column string, ok bool) {
	if r == nil || !r.Enforced {
		return "", "", "", false
	}
	target := strings.TrimSpace(r.Target)
	if target == "" || strings.Contains(target, "/") {
		return "", "", "", false
	}
	switch parts := strings.Split(target, "."); len(parts) {
	case 1:
		table, column = parts[0], "id"
	case 2:
		table, column = parts[0], parts[1]
	case 3:
		schemaName, table, column = parts[0], parts[1], parts[2]
	default:
		return "", "", "", false
	}
	if !isIdent(table) || !isIdent(column) {
		return "", "", "", false
	}
	if schemaName != "" && !isIdent(schemaName) {
		return "", "", "", false
	}
	return schemaName, table, column, true
}

// OfType overrides the column type, for an external reference whose target is
// not the conventional UUID.
func (f *Field) OfType(t Type) *Field {
	f.d.Type = t
	return f
}

// Array makes the column a one-dimensional Postgres array of the type the
// constructor named:
//
//	schema.Text("tags").Array().Filterable()
//	schema.Enum("labels", "red", "green").Array()
//
// The Go field is the plain slice — []string, not a named wrapper — so a model
// described over an existing sqlc struct can carry one (ADR-0033). Nullable
// still refers to the column: a NULL array and an empty array are different
// values, and the Go side spells them nil and []string{}.
//
// Only the scalar element types are permitted, and only one dimension. An
// array column may not be Sortable or Searchable, and a Filterable one must
// carry a GIN index; Validate reports each of those.
func (f *Field) Array() *Field {
	f.d.Array = true
	return f
}

// Timestamps is the created_at / updated_at pair, both defaulting to now().
func Timestamps() Group {
	return Group{
		Timestamp("created_at").Default(Now()).ReadOnly().Sortable(),
		Timestamp("updated_at").Default(Now()).ReadOnly().Sortable(),
	}
}

// SoftDelete adds a nullable deleted_at column, and nothing else. Nothing on
// the request path reads the column: the name is not load-bearing anywhere
// below this line, and declaring the group changes no query.
//
// What it does do is oblige the table to have a BeforeQuery hook. A table that
// declares a soft delete and filters nothing returns deleted rows from every
// list endpoint, so [rest.Resource] refuses to mount one whose reads no hook
// constrains ([ADR-0030]). The refusal is at startup and it checks only that a
// hook exists — writing the predicate is still the caller's, exactly as below.
//
// Filtering the deleted rows out is a BeforeQuery registration, which is the
// seam that reaches generated REST handlers as well as queries written by hand
// ([ADR-0008]):
//
//	sqlb.On[Post](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Post]) error {
//	    q.Where(sqlb.F("deleted_at").IsNull())
//	    return nil
//	})
//
// Serving DELETE as an update to the column is the caller's too, and BeforeDelete
// cannot do it — that hook receives a *Delete and can abort or amend the
// statement, not turn it into an UPDATE. A table that means deletes to be soft
// should leave OpDelete out of its Expose and route the endpoint itself.
//
// [ADR-0008]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#hooks-as-domain-seam
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required
func SoftDelete() Group {
	f := Timestamp("deleted_at").Nullable().ReadOnly()
	f.d.SoftDelete = true
	return Group{f}
}

// Chainable configuration.

// PrimaryKey marks the column as the table's primary key. Primary keys are
// implicitly read-only and filterable.
func (f *Field) PrimaryKey() *Field {
	f.d.PrimaryKey = true
	f.d.ReadOnly = true
	f.d.Filterable = true
	return f
}

// Named overrides the column name.
//
// Most columns are named where they are declared, so this is only needed where
// the name was derived: Ref("org", Org) produces "org_id", and a database that
// calls it "organisation_uuid" needs
//
//	schema.Ref("org", Org).Named("organisation_uuid")
//
// The relation keeps its own name, so ?expand=org still works.
func (f *Field) Named(column string) *Field {
	f.d.Name = column
	return f
}

// ConstraintNamed pins the name of the constraint this column declares. Use it
// when adopting an existing database whose constraint names do not match the
// ones this package would generate.
func (f *Field) ConstraintNamed(name string) *Field {
	f.d.ConstraintName = name
	return f
}

// CheckNamed pins the name of the CHECK an enum column emits.
//
//	schema.Enum("plan", "free", "pro").CheckNamed("chk_org_plan")
//
// It is a second constraint on the same column, so it has a name of its own
// rather than sharing ConstraintNamed with a unique constraint or a foreign
// key. Introspection sets it when the database's name is not the one this
// package would generate; declaring it by hand is for the same case, reached
// from the other direction.
func (f *Field) CheckNamed(name string) *Field {
	f.d.CheckName = name
	return f
}

// RenamedFrom declares that this column used to be called old, so that a
// generated migration renames it rather than dropping it and adding a new one:
//
//	schema.Text("email_address").RenamedFrom("email")
//
// A rename is indistinguishable from a drop and an add when only the before and
// after states are known, and inferring one from a similar name and type would
// destroy data whenever the inference was wrong. So it is declared, never
// inferred (ADR-0014).
//
// The hint is needed for exactly one release: the migration it produces is
// generated once, and after that the old name is gone from every database the
// migration has been applied to. A hint whose old column no longer exists is
// ignored, so leaving one behind is harmless — but delete it at the next edit,
// because a stale hint reads as a claim about the current schema that is no
// longer true.
func (f *Field) RenamedFrom(old string) *Field {
	f.d.RenamedFrom = old
	return f
}

// Nullable allows SQL NULL. Codegen emits the Go field as a pointer.
func (f *Field) Nullable() *Field {
	f.d.Nullable = true
	return f
}

// NotNull is the opposite claim, and the one [Computed] needs.
//
// A stored column is not null unless it says otherwise, so writing this on one
// restates the default and is harmless. A computed column defaults the other
// way — an expression can be NULL and Postgres has no NOT NULL to read it from
// — so this is where the author says the expression cannot produce one, and it
// is a claim rather than a check: nothing parses the SQL (#147).
//
// Worth it when the expression is a `count(*)`, an `EXISTS`, or a comparison
// already guarded against its own nulls, because those are the ones where a
// pointer in the generated model is noise.
func (f *Field) NotNull() *Field {
	f.d.Nullable = false
	return f
}

// Unique adds a single-column unique constraint.
func (f *Field) Unique() *Field {
	f.d.Unique = true
	return f
}

// Indexed adds a single-column btree index, named the way the table-level form
// names it:
//
//	schema.Ref("parent", Category).Nullable().Indexed()
//
// It is the column-level spelling of Table.Index("parent_id"), and it is there
// for the case that reads worst at the table level: a foreign key, where the
// index belongs to the reference rather than to the table's own access
// patterns. Anything with a method, an order, a predicate or a second column is
// [Index] through [TableDef.AddIndex]; this word only says "and index it".
//
// It is skipped when the column already leads an index, is unique, or is the
// primary key — Postgres can seek on those already, and a second index would be
// storage that answers no query the first does not.
//
// Nothing implies it. A reference used to carry one on the strength of being a
// reference, which was a rule the declaration did not state and a registry read
// out of a database could not know had not been followed: introspect imports a
// self-referencing key as an [ExternalRef], the implication put an index on it,
// and every subsequent migration proposed dropping an index that never existed
// (#259). An index is DDL, and DDL is declared here or it is not there.
func (f *Field) Indexed() *Field {
	f.d.indexWanted = true
	return f
}

// Deferred makes the column's unique constraint DEFERRABLE INITIALLY DEFERRED,
// so it holds over the committed state rather than at each statement.
//
//	schema.Int("position").Unique().Deferred()
//
// The case is a set of rows rewritten together — reordering a list by shifting
// every position — where each intermediate state violates a rule the final one
// satisfies. It is refused without [Field.Unique], since there is no other
// constraint on the column for it to defer. For the table-level form, and for
// the argument about what deferral buys, see [Unique.Deferrable].
func (f *Field) Deferred() *Field {
	f.d.UniqueDeferrable = DeferredCheck
	return f
}

// Pattern constrains a text value to a regular expression, checked when a
// request carries it.
//
//	schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`)
//
// The expression reaches the generated request bodies as Huma's `pattern` tag
// and from there the OpenAPI document, so a caller with no compile step can
// discover the rule instead of learning it from a rejected request. That is the
// whole reason it is declared rather than checked in the func: a regexp in Go
// code reaches no emitter, and what the emitters cannot see they cannot
// describe (#311).
//
// It is RE2, the syntax Go's regexp package accepts, and [Registry.Validate]
// refuses a schema whose pattern does not compile. JSON Schema specifies
// ECMA-262 instead, so the two disagree on a handful of constructs — RE2 has no
// backreferences or lookaround, which ECMA-262 does; conversely `(?i)` is RE2
// spelling that a browser will not read. Staying inside the common subset is
// what makes the server and a generated client agree about the same string.
//
// This validates a request and writes no DDL. A row inserted by a migration, a
// seed or a job is not checked against it — see [TableDef.Check] for the rule a
// database holds.
func (f *Field) Pattern(expr string) *Field {
	f.d.Pattern = expr
	return f
}

// Min bounds a numeric value from below, inclusively.
//
//	schema.Int("quantity").Min(1)
//
// It is Huma's `minimum` tag on the generated request bodies, and JSON Schema's
// `minimum` in the document. See [Field.Pattern] for why this is declared
// rather than checked in application code, and for what it does not do.
func (f *Field) Min(v float64) *Field {
	f.d.Min = &v
	return f
}

// Max bounds a numeric value from above, inclusively. It is [Field.Min]'s
// counterpart and everything said there applies.
//
//	schema.Int("quantity").Min(1).Max(999)
func (f *Field) Max(v float64) *Field {
	f.d.Max = &v
	return f
}

// Default sets the column default.
//
// A default also makes the column optional in a generated create body, and
// makes an [sqlb.Insert] omit it while its Go value is the zero value — so the
// database supplies it rather than a zero overwriting it. That reads a zero as
// "not set", which is what the caller meant whenever the default agrees with
// the zero: a generated id, a timestamp, a defaulted string.
//
//	schema.Bool("active").Default(schema.Value(true))
//
// is the one common declaration where they disagree, and where "not set" and
// "set to false" are therefore the same struct. A generated create resolves it
// from the request body, which knows which fields it carried. A caller writing
// the insert itself says so with [sqlb.Insert.Explicit] (#304, #314).
func (f *Field) Default(d *Default) *Field {
	f.d.Default = d
	return f
}

// Identity makes an integer column GENERATED BY DEFAULT AS IDENTITY: the
// database fills it in, and an INSERT may still name it.
//
//	schema.BigInt("id").PrimaryKey().Identity()
//
// This is what Postgres recommends in place of [Serial], and it is the cheaper
// of the two to own: an identity has no separate sequence object to name, own
// or drop, which is why Postgres introduced it. Declare a serial instead when
// the database already has one.
//
// Only an integer column may carry it, it cannot be nullable, and it cannot
// also have a [Field.Default] — the sequence is the default. Each is refused
// when the registry is validated rather than discovered as rejected DDL.
func (f *Field) Identity() *Field {
	f.d.Auto = AutoIdentity
	return f
}

// IdentityAlways makes an integer column GENERATED ALWAYS AS IDENTITY: the
// sequence is the only writer, and an INSERT naming the column is an error
// rather than an override.
//
// It marks the column ReadOnly, for the reason [Computed] does — nothing can
// write it, and saying so here is what keeps the generated create and update
// bodies correct without every write path having to know this exists.
//
// The stricter of the two, and the right one when a key should never be chosen
// by a caller. [Field.Identity] is the one to reach for when a data import or a
// backfill has to supply ids.
func (f *Field) IdentityAlways() *Field {
	f.d.Auto = AutoIdentityAlways
	f.d.ReadOnly = true
	return f
}

// Comment attaches a description, emitted into DDL and the OpenAPI document.
func (f *Field) Comment(s string) *Field {
	f.d.Comment = s
	return f
}

// Filterable allows the column to be used in REST filter expressions.
func (f *Field) Filterable() *Field {
	f.d.Filterable = true
	return f
}

// Sortable allows the column to appear in ?sort.
//
// An optional [Nulls] fixes where NULLs sit whenever this column is sorted on,
// in either direction:
//
//	Timestamp("published_at").Sortable(schema.NullsLast)
//
// Without it the placement is Postgres's default, which follows the direction —
// NULLS LAST ascending, NULLS FIRST descending. That default is right for a
// column whose NULLs are incidental and wrong for one whose NULLs mean
// something: a NULL `published_at` means "not published", which belongs at the
// bottom of the feed and not at the top of it, and `?sort=-published_at` puts
// it at the top (#88).
//
// It is declared here rather than spelled per request because it is a property
// of what the column *means*, not of what a particular caller wants — which is
// also why the generated clients need no new syntax to get it right.
func (f *Field) Sortable(nulls ...Nulls) *Field {
	f.d.Sortable = true
	switch len(nulls) {
	case 0:
	case 1:
		switch nulls[0] {
		case NullsDefault, NullsFirst, NullsLast:
			f.d.SortNulls = nulls[0]
		default:
			panic(fmt.Sprintf("sqlb/schema: Sortable(%q): unknown null placement %q, expected schema.NullsFirst or schema.NullsLast",
				f.d.Name, string(nulls[0])))
		}
	default:
		// Panic for the reason Numeric does: a schema is Go, this runs at init,
		// and a wrong declaration should not compile into a process that then
		// serves ORDER BY from it.
		panic(fmt.Sprintf("sqlb/schema: Sortable(%q) takes at most one null placement, got %d",
			f.d.Name, len(nulls)))
	}
	return f
}

// Searchable includes the column in the ?search fan-out. Implies Filterable,
// since search is a filter over the same column.
func (f *Field) Searchable() *Field {
	f.d.Searchable = true
	f.d.Filterable = true
	return f
}

// Expandable allows a reference to be resolved inline via ?expand.
func (f *Field) Expandable() *Field {
	f.d.Expandable = true
	return f
}

// Inverse names the relation from the target's side: the name an author knows
// its posts by. Declaring it is what makes the reverse relation exist.
//
//	schema.Ref("list", List).Expandable().Inverse("tasks").InverseExpandable()
//
// Read as: a task has a list; a list has tasks; both directions may be
// expanded. Absent Inverse there is no reverse relation, which is not an error
// — most references never need one.
//
// One side declares, as it already does for the column, the constraint and the
// delete action. What the target does gain is a field on its generated struct,
// because the expanded rows need somewhere to land.
func (f *Field) Inverse(name string) *Field {
	if f.d.Ref != nil {
		f.d.Ref.Inverse = name
	}
	return f
}

// InverseExpandable exposes the reverse relation through ?expand on the
// target's endpoint, and takes the options that decide which children a capped
// expansion returns:
//
//	schema.Ref("list", List).
//	    Expandable().
//	    Inverse("tasks").
//	    InverseExpandable(schema.ExpandOrder("-created_at"), schema.ExpandLimit(20))
//
// It requires Inverse: a relation with no name cannot be asked for. Exposure is
// a separate decision from Expandable in the forward direction, because the two
// are about different endpoints.
func (f *Field) InverseExpandable(opts ...InverseOption) *Field {
	if f.d.Ref != nil {
		f.d.Ref.InverseExpandable = true
		for _, opt := range opts {
			opt(f.d.Ref)
		}
	}
	return f
}

// InverseOption adjusts an expanded collection.
type InverseOption func(*Reference)

// ExpandOrder orders an expanded collection by a column of the referencing
// table, with a leading "-" for descending — the spelling ?sort already uses.
// The primary key is appended as a tiebreaker, because under a cap a non-total
// order decides which children the caller never sees.
func ExpandOrder(column string) InverseOption {
	return func(r *Reference) { r.InverseOrder = column }
}

// ExpandLimit caps how many children an expansion returns; the default is 50.
// Past the cap the response reports has_more and the caller follows the child's
// own endpoint, filtered by this foreign key — which is why that column wants
// to be Filterable, and why Lint says so when it is not.
func ExpandLimit(n int) InverseOption {
	return func(r *Reference) { r.InverseLimit = n }
}

// ReadOnly makes the column unwritable through REST.
func (f *Field) ReadOnly() *Field {
	f.d.ReadOnly = true
	return f
}

// Immutable makes the column writable through REST at create time only.
//
// It names its boundary for the reason [Field.ReadOnly] does, and the boundary
// is the same one: the create body carries the column, the generated patch body
// omits it, and the update path refuses it if a request names it anyway.
// Nothing outside REST is policed — [sqlb.UpdateRows] from application code
// writes it, as does a hook, as does an action's write-back — because the
// engine does not stand between an application and its own tables.
//
// So this is a convention in [domain logic]'s sense of the word, and it closes
// the door the generated client opens. A column that must never change after
// insert, wherever the write comes from, wants the guarantee underneath it too:
// a BEFORE UPDATE trigger, which is the layer that sees the old row and the new
// one at once.
//
// [domain logic]: https://github.com/mind-vm/sqlb/blob/main/docs/concepts/domain-logic.md
func (f *Field) Immutable() *Field {
	f.d.Immutable = true
	return f
}

// Hidden omits the column from every REST response. Use it for password
// hashes and similar values that must never leave the process.
//
// It also removes the column from the generated typed-column facade, so
// `SessionCols.TokenHash` does not exist and a predicate against a hidden column
// does not compile. That is the right default — for a password hash,
// `WHERE password_hash = $1` is a sign something has gone wrong — but it is a
// second property, and for the other members of "and similar values" the two
// come apart. Declare [Field.LookupKey] beside it for a credential that is found
// *by* its stored value.
func (f *Field) Hidden() *Field {
	f.d.Hidden = true
	return f
}

// WriteOnly omits the column from every REST response, the same guarantee
// Hidden makes — but leaves it settable through the generated create and
// update bodies, and leaves it in the generated typed-column facade, which
// Hidden removes.
//
// Hidden conflates two questions that come apart for a secret one specific
// caller has to write and nobody ever reads back: a password hash answers
// "never" to both, but an authored answer key — a QuizOption.is_correct only
// the authoring coach should set, and the grading code needs to read back
// through the query engine to compare against a submission — answers "never"
// to serving it and "yes" to writing it (#195). Before this, that shape had
// no generated create or update at all: Hidden blocked the write along with
// the read, and the only way out was hand-writing the endpoint from scratch.
//
//	schema.Bool("is_correct").WriteOnly()
//
// It is refused together with Hidden, ReadOnly, Filterable and Sortable on
// the same column — the first two are contradictory (a column cannot be both
// always-absent-from-writes and only-ever-written, or both never-served and
// differently-never-served), and the second two are the oracle Hidden's own
// Filterable refusal exists to close: a client that cannot read a value back
// can still learn it by watching which rows a `?is_correct=eq.true` filter
// returns, or which rows a `?sort=is_correct` puts first.
func (f *Field) WriteOnly() *Field {
	f.d.WriteOnly = true
	return f
}

// LookupKey keeps a [Field.Hidden] column in the generated typed-column facade,
// for a secret whose stored form is the thing you look the row up by.
//
//	schema.Text("token_hash").Hidden().LookupKey()
//
// Hidden names one property — the value must never be serialised — and the
// facade's omission asserts a second: it must never be predicated on. For a
// password hash the two coincide, since a user is found by email and the hash is
// compared in Go. For session tokens, API keys, password-reset and verification
// tokens, webhook secrets keyed by fingerprint and idempotency keys they do not:
// the presented secret is hashed and the hash *is* the lookup key. Every one of
// those must never leave the process, and every one is found by equality on the
// stored value, so Hidden alone took away the operation the column exists for
// (#155).
//
// Nothing about the REST surface changes, and that is the point of it being a
// separate word. A hidden column has no capability, so the filter grammar still
// refuses `?token_hash=eq.…` with a 400 naming what would have been accepted —
// a client that can probe a credential column by equality has an oracle, and
// that refusal is what capabilities exist for. This is a declaration about Go,
// on the writer's side of the boundary, where `sqlb.F` already grants the same
// reach untyped.
//
// It is refused on a column that is not Hidden, where the facade carries the
// column anyway and the word would be a claim with no effect.
func (f *Field) LookupKey() *Field {
	f.d.LookupKey = true
	return f
}

// Scoped declares that this column confines the table's rows to one tenant,
// and that every operation the table exposes must be constrained by a hook.
//
//	schema.Ref("workspace", Workspace).Filterable().ReadOnly().Scoped()
//
// Like [SoftDelete], it writes no predicate and changes no query. What it
// changes is what happens when the predicate is missing: [rest.Resource]
// refuses to mount the resource at startup rather than serving every tenant's
// rows with a 200 next to them ([ADR-0030]).
//
// The obligation follows the operations the table exposes, because a
// BeforeQuery hook constrains what a request can see and says nothing about
// what it can overwrite by id — a list needs BeforeQuery, an update needs
// BeforeUpdate, a delete needs BeforeDelete, and a create needs BeforeCreate
// when the column is ReadOnly and so has no other source than the hook.
//
// The row itself is the tenant on the table the others point at, so there the
// declaration goes on the primary key:
//
//	schema.UUIDv7("id").PrimaryKey().Scoped()
//
// A table may declare one scope column. Where the confinement cannot be
// written as a column of this table at all — a membership join, say — declare
// it on the column the hook does constrain, which is the key it narrows.
//
// # A scope inherited from a parent row
//
// That last sentence invites a declaration this package accepts and then cannot
// carry the whole way, so it is worth stating what you get. A child table scoped
// through its parent — `cart_lines` belongs to a `carts` row, and the cart
// belongs to a session — declares `Scoped` on the foreign key, and the
// obligation lands correctly on every resource that exposes the table. The hook
// it obliges has to confine with a subquery:
//
//	cart_id IN (SELECT id FROM carts WHERE session_id = $1)
//
// which is a [sqlb.RawPred], because the predicate vocabulary is value
// comparison and column-to-column and has no spelling for a subquery. That works
// for every direct query of the table, and it forecloses `?expand` onto it
// permanently: opaque text cannot be requalified onto a join alias with
// certainty, so the expansion is refused rather than silently resolving the
// predicate against the wrong table.
//
// The refusal is loud and lands at the right moment, so this is a foreclosure
// rather than a hazard. But it is worth deciding up front, because the other way
// out is to store the scope column on the child as well — a second copy of the
// one column whose wrongness is a data leak. Usually the right answer is neither:
// leave the child unexposed and reach its rows through the parent's endpoint,
// which is where the confinement already holds (#158).
//
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required
// name optionally says *which* scope this column carries, for the columns whose
// name does not already say it. Codegen groups the confining hooks it emits by
// that answer — sixteen tables carrying `workspace_id` are one question and get
// one func to answer it — and the column name is a good enough key whenever the
// column is a reference to the tenant table.
//
// It is not good enough for a table scoped on its own primary key. A workspace
// confined to `id = the caller's workspace` and a user confined to `id = …`
// are two different questions under one column name, and grouping them by that
// name would hand both to one func. So a scoped primary key must name its
// scope, and codegen refuses the schema until it does (#274). Naming the same
// scope on both a primary key and a reference is how a table that *is* the
// tenant and a table that points at it come to share one answer:
//
//	var Workspace = schema.Table("workspaces",
//	    schema.UUIDv7("id").PrimaryKey().Scoped("workspace"),
//	)
//	var Task = schema.Table("tasks",
//	    schema.Ref("workspace", Workspace).ReadOnly().Scoped("workspace"),
//	)
//
// The name is about grouping columns, and is unrelated to the releasable name
// passed to [sqlb.Hooks.Scope] at registration, which groups hooks so a handle
// can be released from them.
func (f *Field) Scoped(name ...string) *Field {
	f.d.Scoped = true
	if len(name) > 0 {
		f.d.ScopeName = name[0]
	}
	return f
}

// OnDelete sets the foreign key delete action. It panics if the field is not a
// reference: that is a schema authoring bug, and failing at init is more useful
// than failing at request time.
func (f *Field) OnDelete(a RefAction) *Field {
	if f.d.Ref == nil {
		panic(fmt.Sprintf("sqlb/schema: OnDelete on non-reference column %q", f.d.Name))
	}
	f.d.Ref.OnDelete = a
	return f
}

// OnUpdate sets the foreign key update action.
func (f *Field) OnUpdate(a RefAction) *Field {
	if f.d.Ref == nil {
		panic(fmt.Sprintf("sqlb/schema: OnUpdate on non-reference column %q", f.d.Name))
	}
	f.d.Ref.OnUpdate = a
	return f
}

// GoType is the Go type codegen emits for this column.
//
// An array is the plain slice of its element type, nullable or not: a nil slice
// already says NULL and an empty one says {}, so a pointer would add a third
// spelling for a distinction that only has two.
//
// A nullable bytea is the same argument and stays []byte: nil is already how a
// slice says NULL, and a pointer would add a second spelling for it.
//
// jsonb used to be excluded alongside it, on the strength of the resemblance —
// json.RawMessage is a slice of bytes too. The resemblance is where it ends.
// []byte says NULL by being nil because that is what it *is*; json.RawMessage
// is a document type whose nullability the model otherwise never states, which
// left a nullable jsonb as the one column whose generated type did not say it
// could be NULL.
//
// It was also, until sqlb took pgx as a dependency (ADR-0040), unreadable.
// database/sql's convertAssign resolves a scan destination by concrete type: it
// carries a `case *[]byte` that stores NULL as a nil slice, and json.RawMessage
// is a named type over []byte that matches neither that case nor any other, so
// a NULL fell out the bottom as "unsupported Scan, storing driver.Value type
// <nil>". pgx has no such gap and scans NULL into a bare json.RawMessage as
// nil, so on sqlb's own path this is now consistency rather than a repair — but
// it is consistency the generated struct keeps when it is read by anything
// else, database/sql included.
func (d *FieldDesc) GoType() string {
	base := d.Type.GoType()
	if d.Array {
		return "[]" + base
	}
	if d.Nullable && base != "[]byte" {
		return "*" + base
	}
	return base
}

// IsArrayElement reports whether t may be the element type of an array column.
//
// jsonb and bytea are excluded: both already hold a composite value, and an
// array of either is a shape no generated client can narrow past `unknown`.
func IsArrayElement(t Type) bool {
	switch t {
	case TypeText, TypeVarchar, TypeEnum, TypeSmallInt, TypeInt, TypeBigInt,
		TypeReal, TypeFloat, TypeNumeric, TypeBool, TypeUUID, TypeTimestamp,
		TypeDate, TypeTime:
		return true
	}
	return false
}

// DatabaseSupplied reports whether the database fills this column in when an
// INSERT does not name it — because it has a default, or because a sequence or
// an identity supplies it.
//
// The distinction between those matters exactly once, when DDL is rendered.
// Everywhere after that — whether a create body may omit the column, whether
// the engine writes a zero or lets the database decide, whether the OpenAPI
// property is required — the question is only whether the caller has to say,
// and the answer is the same for all three. Asking it in one place is what let
// [Serial] arrive without a write path having to learn what a sequence is.
func (d *FieldDesc) DatabaseSupplied() bool {
	return d.Default != nil || d.Auto != NotAuto
}

// Capabilities renders the column's capabilities as the comma-separated body
// of the `sqlb` struct tag that the runtime engine reads back.
func (d *FieldDesc) Capabilities() string {
	var out []string
	add := func(cond bool, s string) {
		if cond {
			out = append(out, s)
		}
	}
	add(d.PrimaryKey, "pk")
	add(d.DatabaseSupplied(), "default")
	add(d.Filterable, "filter")
	// The null placement rides on the capability that carries it rather than
	// becoming a capability of its own: it has no meaning without `sort`, and a
	// separate token could be written without one.
	switch {
	case d.Sortable && d.SortNulls == NullsFirst:
		add(true, "sort:nullsfirst")
	case d.Sortable && d.SortNulls == NullsLast:
		add(true, "sort:nullslast")
	default:
		add(d.Sortable, "sort")
	}
	add(d.Searchable, "search")
	add(d.Expandable, "expand")
	add(d.ReadOnly, "readonly")
	add(d.Immutable, "immutable")
	add(d.Hidden, "hidden")
	add(d.WriteOnly, "writeonly")
	add(d.Scoped, "scope")
	add(d.SoftDelete, "softdelete")
	return strings.Join(out, ",")
}

// IndexWanted reports whether the column declared [Field.Indexed].
func (d *FieldDesc) IndexWanted() bool { return d.indexWanted }

// Computed reports whether the column is an expression rather than storage.
// The DDL emitters read it to skip the column, and Diff reads it to not see it
// at all.
func (d *FieldDesc) Computed() bool { return d.Expr != "" }

// Placeholders counts the binds a computed expression takes, treating `??` as
// the escaped literal that Raw does.
func (d *FieldDesc) Placeholders() int {
	n := 0
	for i := 0; i < len(d.Expr); i++ {
		if d.Expr[i] != '?' {
			continue
		}
		if i+1 < len(d.Expr) && d.Expr[i+1] == '?' {
			i++
			continue
		}
		n++
	}
	return n
}

// volatileMarkers are the expressions whose value changes between two readings
// within one query's lifetime. A computed column sorted on one of them cannot
// carry a keyset: the boundary a cursor recorded is compared against a
// different number on the next page (ADR-0027).
var volatileMarkers = []string{
	"now(", "current_date", "current_time", "current_timestamp", "localtime",
	"localtimestamp", "clock_timestamp(", "statement_timestamp(", "timeofday(",
	"random(",
}

// Volatile reports whether a computed expression reads something that does not
// hold still.
func (d *FieldDesc) Volatile() bool {
	lower := strings.ToLower(d.Expr)
	for _, marker := range volatileMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
