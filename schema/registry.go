package schema

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds a set of table declarations.
//
// A registry is also the unit of module isolation. Independent modules — fx
// modules, or any other arrangement where one package must not import another —
// each declare into their own registry, so two modules may both own a table
// called "events" without colliding.
type Registry struct {
	mu     sync.RWMutex
	module string
	wire   WireCase
	tables []*TableDef
	byName map[string]*TableDef
}

// NewRegistry returns an empty registry. Most schemas use the default one via
// the package-level functions.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]*TableDef)}
}

// NewModule returns a registry whose tables are all prefixed with the module
// name, so that table ownership is visible in the database and cannot be
// forgotten:
//
//	var Billing = schema.NewModule("billing")
//	var Invoice = Billing.Table("invoices", …)   // → billing_invoices
//
// The prefix is applied by the registry rather than written into each
// declaration, which is the point: a convention that has to be repeated at
// every call site is a convention that drifts.
//
// Declarations still use the local name, so a table moving between modules
// changes one line.
func NewModule(name string) *Registry {
	if !isIdent(name) {
		panic(fmt.Sprintf("sqlb/schema: module name %q is not a valid SQL identifier prefix", name))
	}
	r := NewRegistry()
	r.module = name
	return r
}

// Module returns the module name, or "" for a registry that is not a module.
func (r *Registry) Module() string { return r.module }

// Qualify renders a local table name as this registry would store it.
func (r *Registry) Qualify(local string) string {
	if r.module == "" {
		return local
	}
	return r.module + "_" + local
}

var defaultRegistry = NewRegistry()

// DefaultRegistry returns the registry that Table populates.
func DefaultRegistry() *Registry { return defaultRegistry }

// Register adds a table to the default registry. Table calls this for you.
func Register(t *TableDef) { defaultRegistry.Add(t) }

// Add registers a table. A duplicate name panics: two tables with the same
// name is an authoring error that would otherwise surface as confusing DDL.
func (r *Registry) Add(t *TableDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[t.name]; dup {
		panic(fmt.Sprintf("sqlb/schema: table %q declared twice", t.name))
	}
	r.byName[t.name] = t
	r.tables = append(r.tables, t)
}

// Tables returns every registered table, sorted by name so that generated
// output is deterministic across runs.
func (r *Registry) Tables() []*TableDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*TableDef, len(r.tables))
	copy(out, r.tables)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Get returns the named table, or nil.
func (r *Registry) Get(name string) *TableDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}

// Exposed returns the tables with a REST surface, sorted by name.
func (r *Registry) Exposed() []*TableDef {
	var out []*TableDef
	for _, t := range r.Tables() {
		if t.rest != nil {
			out = append(out, t)
		}
	}
	return out
}

// Error is a single schema validation failure, located at a table and
// optionally a column.
type Error struct {
	Table  string
	Column string
	Msg    string
}

func (e Error) Error() string {
	if e.Column != "" {
		return fmt.Sprintf("%s.%s: %s", e.Table, e.Column, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Table, e.Msg)
}

// Validate checks the registry for authoring mistakes and returns every
// problem it finds, joined into a single error. Reporting all of them at once
// rather than stopping at the first keeps the edit-generate loop short.
func (r *Registry) Validate() error {
	var errs []error
	report := func(table, column, format string, args ...any) {
		errs = append(errs, Error{Table: table, Column: column, Msg: fmt.Sprintf(format, args...)})
	}

	// A rename hint claims that a name is gone, so two tables cannot claim the
	// same one and no table may claim a name that is still declared.
	renamedTables := make(map[string]string)

	// Inverse names are claimed on the *target's* endpoint, so the collision
	// this catches is between two references declared in different tables.
	// Keyed by target table and name; the value is where it was claimed from.
	inverses := make(map[string]string)

	// SharedAs names are claimed across the whole registry — that is the point
	// of the declaration — so the first column to use one fixes what every
	// later column claiming it must match. Keyed by the SharedAs name.
	sharedEnums := make(map[string]sharedEnumClaim)

	// Before anything else: if the schema's wire case cannot spell one of its
	// own columns reversibly, nothing generated from it is trustworthy.
	r.validateWireNames(report)

	for _, t := range r.Tables() {
		if !isIdent(t.name) {
			report(t.name, "", "table name is not a valid SQL identifier")
		}
		if old := t.oldName; old != "" {
			switch {
			case !isIdent(old):
				report(t.name, "", "RenamedFrom %q is not a valid SQL identifier", old)
			case old == t.name:
				report(t.name, "", "RenamedFrom names the table itself")
			case r.Get(old) != nil:
				report(t.name, "", "RenamedFrom %q is still declared as a table of its own; a rename means the old name is gone", old)
			}
			if prev, dup := renamedTables[old]; dup {
				report(t.name, "", "RenamedFrom %q is also claimed by table %q", old, prev)
			}
			renamedTables[old] = t.name
		}

		seen := make(map[string]bool, len(t.fields))
		renamedCols := make(map[string]string)
		pks := 0
		scoped := 0
		for _, f := range t.fields {
			d := f.Desc()
			if !isIdent(d.Name) {
				report(t.name, d.Name, "column name is not a valid SQL identifier")
			}
			if seen[d.Name] {
				report(t.name, d.Name, "column declared twice")
			}
			seen[d.Name] = true

			if old := d.RenamedFrom; old != "" {
				switch {
				case !isIdent(old):
					report(t.name, d.Name, "RenamedFrom %q is not a valid SQL identifier", old)
				case old == d.Name:
					report(t.name, d.Name, "RenamedFrom names the column itself")
				case t.Field(old) != nil:
					// Either the hint is wrong, or the two columns are being
					// swapped — which Postgres cannot do in one statement
					// either, and which a generator should not attempt.
					report(t.name, d.Name, "RenamedFrom %q is still declared as a column of its own; a rename means the old name is gone", old)
				}
				if prev, dup := renamedCols[old]; dup {
					report(t.name, d.Name, "RenamedFrom %q is also claimed by column %q", old, prev)
				}
				renamedCols[old] = d.Name
			}

			if d.PrimaryKey {
				pks++
				if d.Nullable {
					report(t.name, d.Name, "primary key cannot be Nullable")
				}
				if d.Hidden {
					report(t.name, d.Name, "primary key cannot be Hidden: REST responses need it to address the row")
				}
				if d.WriteOnly {
					report(t.name, d.Name, "primary key cannot be WriteOnly: REST responses need it to address the row")
				}
			}
			if d.Scoped {
				scoped++
				// A tenant column a request may write is not a tenant column:
				// the create body would carry it, and the caller would choose
				// which tenant to write into. ReadOnly keeps it out of the
				// generated bodies entirely, which leaves the BeforeCreate
				// hook as the only thing that can supply it. Immutable is not
				// enough — it closes the update and leaves the create open.
				if !d.ReadOnly {
					report(t.name, d.Name, "Scoped column must be ReadOnly, or a create request gets to name the tenant it writes into")
				}
				// A tenant column that may be NULL is scoped by a predicate
				// that cannot match it, so those rows are visible to nobody
				// and, on the day someone writes IS NULL OR = $1, to everybody.
				if d.Nullable {
					report(t.name, d.Name, "Scoped column cannot be Nullable: a row whose tenant is NULL is outside every tenant's predicate")
				}
			}
			if d.Expandable && d.Ref == nil {
				report(t.name, d.Name, "Expandable is only meaningful on a Ref column")
			}
			if d.Computed() || len(d.Needs) > 0 {
				r.validateComputed(t, d, report)
			}
			if d.Searchable && (!isTextual(d.Type) || d.Array) {
				report(t.name, d.Name, "Searchable requires a text column, got %s", describeType(d))
			}
			if d.Array {
				r.validateArray(t, d, report)
			}
			if d.Type == TypeVector {
				r.validateVector(t, d, report)
			}
			if d.Auto != NotAuto {
				r.validateAuto(t, d, report)
			}
			if d.Type == TypeEnum && len(d.EnumValues) == 0 {
				report(t.name, d.Name, "Enum declares no values")
			}
			if d.SharedAs != "" {
				r.validateSharedEnum(t, d, sharedEnums, report)
			}
			if d.Hidden && d.Filterable {
				report(t.name, d.Name, "column is both Hidden and Filterable, which leaks its contents through filter probing")
			}
			if d.WriteOnly {
				if d.Hidden {
					report(t.name, d.Name, "column is both Hidden and WriteOnly; they say the same thing about reads and disagree about writes — pick one")
				}
				if d.ReadOnly {
					report(t.name, d.Name, "column is both WriteOnly and ReadOnly: never settable and only ever settable")
				}
				if d.Filterable {
					report(t.name, d.Name, "column is both WriteOnly and Filterable, which leaks its contents through filter probing")
				}
				if d.Sortable {
					report(t.name, d.Name, "column is both WriteOnly and Sortable, which leaks its contents through the order it puts rows in")
				}
			}
			// LookupKey only ever *keeps* a facade entry Hidden would have
			// removed. On a visible column the entry is there either way, so
			// the word would read as a declaration and mean nothing — and the
			// generated comment saying which kind of secret a column is would
			// be saying it about one that is not a secret at all.
			if !d.UniqueDeferrable.Valid() {
				report(t.name, d.Name, "unknown Deferrable %q", d.UniqueDeferrable)
			}
			// Deferral is a property of a constraint, and this one has none of
			// its own to defer. A primary key is not it either: PRIMARY KEY is
			// rendered from the table's key columns, and deferring it is not
			// something this DSL can say.
			if d.UniqueDeferrable != NotDeferrable && !d.Unique {
				report(t.name, d.Name, "Deferred applies to a column's own unique constraint; add Unique(), or declare the constraint with AddUnique")
			}
			if d.LookupKey && !d.Hidden {
				report(t.name, d.Name, "LookupKey applies to a Hidden column; the typed column is already there without it")
			}
			if d.Ref != nil && d.Ref.External {
				if d.Expandable {
					report(t.name, d.Name, "a reference across a module boundary cannot be Expandable: expanding it would join a table this module does not own")
				}
				if d.Ref.Inverse != "" {
					report(t.name, d.Name, "a reference across a module boundary cannot declare an Inverse: nothing about the other side is resolvable, in either direction")
				}
				if d.Ref.Target == "" {
					report(t.name, d.Name, "ExternalRef declares no target")
				}
				// An enforced reference emits a constraint naming a table, so
				// unlike the unenforced form its target has to be one. A
				// module-qualified name is not: the FOREIGN KEY would be
				// written against a table nothing in this database is called.
				if _, _, ok := d.Ref.EnforcedTarget(); d.Ref.Enforced && !ok {
					report(t.name, d.Name,
						"Enforced needs a target naming a table in this database, and %q is not one; "+
							"write it as \"organizations.id\" or \"organizations\" — a module-qualified target "+
							"cannot carry a real foreign key, which is the whole of what a module boundary means",
						d.Ref.Target)
				}
			}
			if d.Ref != nil && !d.Ref.External && d.Ref.Enforced {
				// A real reference is always enforced. Reading .Enforced() on
				// one would suggest the others are not.
				report(t.name, d.Name, "Enforced applies to an ExternalRef; a Ref already emits a foreign key")
			}
			if d.Ref != nil && !d.Ref.External {
				switch {
				case d.Ref.Table == nil:
					report(t.name, d.Name, "Ref target is nil (declaration order: the target table var must be initialised first)")
				case r.Get(d.Ref.Table.name) == nil:
					report(t.name, d.Name, "Ref target %q is not registered", d.Ref.Table.name)
				case d.Ref.Table.PrimaryKey() == nil && len(d.Ref.Table.CompositeKey()) > 0:
					// Named apart from "no primary key", because the fix is a
					// different one: the target has a key and a reference here
					// is single-column, so the answer is a surrogate on the
					// target or no reference at all — not "give it a key".
					report(t.name, d.Name, "Ref target %q has a composite primary key (%s), and a "+
						"reference is single-column; give the target a surrogate key to point at, "+
						"or leave the relationship undeclared",
						d.Ref.Table.name, strings.Join(d.Ref.Table.CompositeKey(), ", "))
				case d.Ref.Table.PrimaryKey() == nil:
					report(t.name, d.Name, "Ref target %q has no primary key", d.Ref.Table.name)
				}
			}
			if d.Ref != nil {
				r.validateInverse(t, d, inverses, report)
			}
		}

		if pks > 0 && len(t.pkCols) > 0 {
			report(t.name, "", "the table declares both a column primary key and PrimaryKeyColumns(%s); "+
				"one table has one primary key, so use the table-level form for all of its columns "+
				"or the column form for the single one",
				strings.Join(t.pkCols, ", "))
		}
		if len(t.pkCols) == 1 {
			// Legal Postgres and a spelling with two meanings in one schema.
			// Refused rather than accepted quietly, because the two forms differ in
			// what the rest of the DSL will then allow — a single-column key
			// declared this way would silently lose Ref, REST and actions.
			report(t.name, "", "PrimaryKeyColumns(%q) names one column, which is Field.PrimaryKey()'s job; "+
				"the table-level form is for a key of two or more columns, and using it for one "+
				"would silently give up Ref, REST exposure and row actions",
				t.pkCols[0])
		}
		if pks > 1 {
			// The reason rather than only the workaround: one column is what
			// spells a row in a URL, a cursor and a generated cache key, and
			// each of those is a wire format (ADR-0034).
			report(t.name, "", "%d primary keys declared, expected at most one; one column is what addresses a row in its URL and its cursor, so declare the composite key with UniqueIndex, and add a surrogate key beside it if the table is exposed", pks)
		}
		// Two scope columns would name one hook twice and say nothing more.
		// There is no matching check for soft delete: the group always
		// declares deleted_at, so a second one is already a duplicate column.
		if scoped > 1 {
			report(t.name, "", "%d Scoped columns declared, expected at most one", scoped)
		}

		for _, idx := range t.Indexes() {
			if len(idx.Columns) == 0 {
				report(t.name, "", "index %q covers no columns", idx.Name)
			}
			for _, c := range idx.Columns {
				if !seen[c] {
					report(t.name, "", "index %q references unknown column %q", idx.Name, c)
				}
			}
			// A derived index name concatenates the table and every column it
			// covers, so a prefixed table with a composite index passes 63
			// bytes without anything looking long. Postgres then truncates
			// silently — even quoted — so the name in the schema and the name
			// in the database differ, and every later diff proposes renaming
			// one to the other forever.
			if len(idx.Name) > maxIdentBytes {
				report(t.name, "", "index name %q is %d bytes; Postgres truncates at %d, "+
					"so give it a shorter Name explicitly", idx.Name, len(idx.Name), maxIdentBytes)
			}
		}

		for _, u := range t.Uniques() {
			if len(u.Columns) == 0 {
				report(t.name, "", "unique constraint %q covers no columns", u.Name)
			}
			for _, c := range u.Columns {
				if !seen[c] {
					report(t.name, "", "unique constraint %q references unknown column %q", u.Name, c)
				}
			}
			// The derived name concatenates the table and every column, so it
			// runs long sooner than an index name does — and a truncated
			// constraint name leaves every later diff proposing to rename one
			// spelling to the other forever. UniqueNamed is the way out.
			if len(u.Name) > maxIdentBytes {
				report(t.name, "", "unique constraint name %q is %d bytes; Postgres truncates at %d, "+
					"so give it a shorter name with UniqueNamed", u.Name, len(u.Name), maxIdentBytes)
			}
			if !u.Deferrable.Valid() {
				report(t.name, "", "unique constraint %q has an unknown Deferrable %q", u.Name, u.Deferrable)
			}
		}

		if t.rest != nil && len(t.pkCols) > 0 {
			// A composite key is declarable precisely so that association
			// tables and natural-key caches can be *gated* without being
			// resources. Refused at declaration rather than at mount, and named
			// as the reason rather than reported as "no primary key", which is
			// what every other consumer will see and is misleading here: one
			// column is what addresses a row in a URL, a cursor and a generated
			// cache key, and each of those is a wire format (ADR-0034).
			report(t.name, "", "has a composite primary key (%s) and is exposed over REST; "+
				"/{id} addresses one column, so a table with this key cannot be a resource — "+
				"add a surrogate key beside it if it needs to be one",
				strings.Join(t.pkCols, ", "))
		}
		if t.rest != nil {
			// A singleton addresses its row through the scope hook rather than
			// through a path segment, so none of its operations needs a key.
			needsPK := !t.rest.Ops.Has(OpSingleton) &&
				(t.rest.Ops.Has(OpRead) || t.rest.Ops.Has(OpUpdate) || t.rest.Ops.Has(OpDelete))
			if needsPK && pks == 0 {
				report(t.name, "", "exposed for %s but has no primary key to address rows by", t.rest.Ops)
			}
			if t.rest.Ops.Has(OpSingleton) {
				r.validateSingleton(t, report)
			}
			if t.rest.Ops == 0 {
				report(t.name, "", "Expose declares no operations")
			}
			if !strings.HasPrefix(t.rest.Path, "/") {
				report(t.name, "", "REST path %q must start with %q", t.rest.Path, "/")
			}
			if t.rest.MaxPageSize < 0 || t.rest.DefaultPageSize < 0 {
				report(t.name, "", "page sizes must not be negative")
			}
			if t.rest.MaxPageSize > 0 && t.rest.DefaultPageSize > t.rest.MaxPageSize {
				report(t.name, "", "DefaultPageSize %d exceeds MaxPageSize %d", t.rest.DefaultPageSize, t.rest.MaxPageSize)
			}
			// Negative is refused rather than resolved. Every ceiling treats a
			// non-positive value as "take the package default", so a negative
			// one is a declaration that reads as a tighter bound and behaves
			// as the loosest available — which is the one direction a cost
			// ceiling must not fail in.
			if t.rest.MaxFilters < 0 || t.rest.MaxSortTerms < 0 || t.rest.MaxOffset < 0 {
				report(t.name, "", "request ceilings must not be negative; leave one zero to take the package default")
			}
			// A default ordering naming a column that cannot be sorted by is a
			// resource whose every unsorted list is a 400 — answering a client
			// that sent nothing wrong. It is checkable here, so it is refused
			// here rather than at mount, where the same mistake would have
			// already shipped.
			for _, term := range t.rest.DefaultSort {
				name, _, err := SortTerm(term)
				if err != nil {
					report(t.name, "", "DefaultSort %q: %s", term, err)
					continue
				}
				f := t.Field(name)
				switch {
				case f == nil:
					report(t.name, "", "DefaultSort %q names no column of this table", term)
				case f.Desc().Hidden:
					report(t.name, name, "DefaultSort %q names a Hidden column; a resource cannot order by a column it never serves", term)
				case f.Desc().WriteOnly:
					report(t.name, name, "DefaultSort %q names a WriteOnly column; a resource cannot order by a column it never serves", term)
				case !f.Desc().Sortable:
					report(t.name, name, "DefaultSort %q names a column that is not Sortable; "+
						"capabilities are opt-in, so an ordering nothing declared is one no ?sort could ask for either", term)
				}
			}
			// A declared soft delete and a generated hard DELETE are a
			// contradiction the runtime cannot resolve: nothing reads
			// deleted_at, so the generated handler removes the row and the
			// column that was supposed to record its removal stays NULL
			// forever. Every other disagreement between a schema and its
			// behaviour in this package is loud; this one silently did the
			// opposite of what the table declares.
			r.validateCreateInput(t, report)
			if seen["deleted_at"] && t.rest.Ops.Has(OpDelete) {
				report(t.name, "deleted_at",
					"declares a soft delete but exposes OpDelete, which hard-deletes the row; "+
						"drop OpDelete from Expose and route DELETE to an update of deleted_at")
			}
		}

		r.validateActions(t, report)
		r.validateQueries(t, report)
	}

	// Duplicate REST paths would make routing order-dependent.
	paths := make(map[string]string)
	for _, t := range r.Exposed() {
		if prev, dup := paths[t.rest.Path]; dup {
			report(t.name, "", "REST path %q already used by table %q", t.rest.Path, prev)
			continue
		}
		paths[t.rest.Path] = t.name
	}

	return errors.Join(errs...)
}

// validateSingleton checks the two things that make [OpSingleton] safe.
//
// Both are refusals rather than warnings, and both are here rather than at
// mount because the declaration is where the mistake is written. A singleton's
// row is whatever the scope hook leaves — there is no key in the path and no
// predicate in the statement — so on a table nobody declared confined, the read
// answers an arbitrary row and PATCH reaches every row in the table. That is
// the default-open outcome [ADR-0030] exists to close, arriving through a
// different door.
func (r *Registry) validateSingleton(t *TableDef, report func(string, string, string, ...any)) {
	var scope string
	for _, f := range t.fields {
		if f.Desc().Scoped {
			scope = f.Desc().Name
			break
		}
	}
	if scope == "" {
		report(t.name, "", "exposes OpSingleton but declares no Scoped column; "+
			"a singleton addresses the caller's row through the scope hook and nothing else, "+
			"so on an unconfined table the read answers an arbitrary row and a write reaches every row — "+
			"declare Scoped on the tenant column, or expose OpRead and OpList instead")
	}
	// OpList is a route collision — both are GET on the collection path — and
	// OpRead is the id-shaped question a singleton exists to delete. Named
	// separately because the fix differs: one is a choice between two shapes,
	// the other is a leftover.
	if t.rest.Ops.Has(OpList) {
		report(t.name, "", "exposes both OpSingleton and OpList, which are the same route: "+
			"GET %s cannot be the caller's row and the collection at once", t.rest.Path)
	}
	if t.rest.Ops.Has(OpRead) {
		report(t.name, "", "exposes both OpSingleton and OpRead; OpSingleton removes the {id} segment "+
			"from this resource, so a read by id is the question it exists to delete — drop OpRead")
	}
}

// Validate checks the default registry.
func Validate() error { return defaultRegistry.Validate() }

// Tables returns every table in the default registry.
func Tables() []*TableDef { return defaultRegistry.Tables() }

// Get returns the named table from the default registry.
func Get(name string) *TableDef { return defaultRegistry.Get(name) }

func isTextual(t Type) bool {
	switch t {
	case TypeText, TypeVarchar, TypeEnum:
		return true
	}
	return false
}

// isIdent reports whether s is a safe unquoted SQL identifier. The generator
// and the filter parser both rely on this: an identifier that passes here can
// be interpolated into SQL without further escaping.
func isIdent(s string) bool { return CheckIdent(s) == nil }

// SortTerm splits a sort term into the column it names and its direction.
//
// Both spellings the request grammar accepts are read here — `-published_at`
// and `published_at.desc` — so a declared default and a `?sort` that means the
// same thing are written the same way. It is exported because codegen resolves
// a declared ordering at generation time rather than emitting a second parser
// into the exit.
//
// This duplicates [filter.SortTerm] on purpose: nothing on the request path may
// import this package, which is what keeps the runtime usable without the DSL.
// The grammar is four lines and is pinned on both sides by tests.
func SortTerm(term string) (name string, desc bool, err error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return "", false, errors.New("a sort term cannot be empty")
	}
	if name, found := strings.CutPrefix(term, "-"); found {
		return name, true, nil
	}
	if name, dir, found := strings.Cut(term, "."); found {
		switch strings.ToLower(dir) {
		case "asc":
			return name, false, nil
		case "desc":
			return name, true, nil
		default:
			return "", false, fmt.Errorf("%q is not a sort direction; write asc, desc, or a leading -", dir)
		}
	}
	return term, false, nil
}

// CheckIdent reports why the DSL cannot declare a table or column called name,
// or nil when it can.
//
// It is exported for introspection, which reads names a database already has
// rather than names an author chose. Those two are not the same set — a
// camelCase column is legal in Postgres and undeclarable here — and an importer
// needs to say which construct it had to skip, not fail the whole import with a
// message about what the DSL considers impossible.
func CheckIdent(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if len(name) > maxIdentBytes {
		return fmt.Errorf("name is %d bytes; Postgres truncates identifiers at %d, "+
			"so the declared name and the real one would differ", len(name), maxIdentBytes)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_':
		case i > 0 && (r >= '0' && r <= '9'):
		case r >= 'A' && r <= 'Z':
			return fmt.Errorf("name contains an upper-case letter, which Postgres only " +
				"preserves for a quoted identifier; the DSL declares unquoted names only")
		default:
			return fmt.Errorf("name contains %q, which is not allowed in an unquoted "+
				"identifier", r)
		}
	}
	return nil
}

// maxIdentBytes is Postgres's NAMEDATALEN-1. An identifier longer than this is
// silently truncated, even when quoted, so a longer declared name would not be
// the name the database ends up holding.
const maxIdentBytes = 63

// validateInverse checks a declared reverse relation.
//
// The collision case is the one this exists for, and it is the reason the name
// cannot be derived: posts.author_id and posts.reviewer_id both point at
// authors, so a derived reverse would call both of them "posts" and an author's
// posts are not the posts an author reviewed. Two references claiming one name
// on one target is therefore an error rather than a last-writer-wins. ADR-0022.
// validateAuto checks what Postgres would refuse about a serial or an identity
// column, here rather than as rejected DDL halfway through a migration.
//
// Every one of these is a combination Postgres has no reading for, not a taste
// this package is expressing. A serial over a uuid is not a narrower serial, it
// is an error; a nullable identity is not an identity whose value may be
// omitted, it is an error.
func (r *Registry) validateAuto(t *TableDef, d *FieldDesc, report func(string, string, string, ...any)) {
	what := "an identity column"
	if d.Auto == AutoSerial {
		what = "a serial column"
	}
	switch d.Type {
	case TypeSmallInt, TypeInt, TypeBigInt:
	default:
		report(t.name, d.Name, "%s must be smallint, int or bigint, got %s: a sequence counts, and there is nothing to count in a %s",
			what, describeType(d), d.Type)
	}
	if d.Array {
		report(t.name, d.Name, "%s cannot be an Array: a sequence supplies one value, not a list", what)
	}
	if d.Nullable {
		report(t.name, d.Name, "%s cannot be Nullable: the sequence always has a next value, and Postgres makes the column NOT NULL either way", what)
	}
	if d.Default != nil {
		report(t.name, d.Name, "%s cannot also have a Default: the sequence is the default, and Postgres refuses a column with both", what)
	}
	if d.Computed() {
		report(t.name, d.Name, "%s cannot be Computed: an expression is derived from the row, and a sequence is not", what)
	}
	if d.Auto == AutoIdentityAlways && !d.ReadOnly {
		// IdentityAlways sets ReadOnly itself, so reaching this means something
		// cleared it. Saying so beats an INSERT that Postgres refuses.
		report(t.name, d.Name, "a GENERATED ALWAYS identity column is ReadOnly: an INSERT naming it is an error rather than an override, so nothing may offer it to a caller")
	}
}

// validateArray checks the refusals ADR-0033 starts an array column from.
//
// Each of them is the cheap direction to be wrong in: allowing one of these
// later is additive, and withdrawing it once a schema declares it is not.
func (r *Registry) validateArray(t *TableDef, d *FieldDesc, report func(string, string, string, ...any)) {
	if !IsArrayElement(d.Type) {
		report(t.name, d.Name, "%s is not an array element type; arrays are one-dimensional and hold scalars", d.Type)
	}
	if d.PrimaryKey {
		report(t.name, d.Name, "an array column cannot be the primary key: one column addresses a row in its URL and its cursor, and an array has no spelling in either")
	}
	if d.Ref != nil {
		report(t.name, d.Name, "an array column cannot be a reference: Postgres has no foreign key from an array's elements")
	}
	if d.Scoped {
		report(t.name, d.Name, "an array column cannot be Scoped: a tenant predicate compares one value, not a set")
	}
	if d.SoftDelete {
		report(t.name, d.Name, "an array column cannot carry the soft-delete marker")
	}
	// Postgres orders arrays happily. The refusal is about the cursor: keyset
	// paging reads the ordering columns off the last row and encodes them into
	// a token that is wire format (ADR-0027), and an array has no spelling
	// there that a client could be asked to keep sending back.
	if d.Sortable {
		report(t.name, d.Name, "an array column cannot be Sortable: the keyset cursor encodes the ordering columns, and an array has no spelling in it")
	}
	if d.Filterable && !t.hasGINIndex(d.Name) {
		// The failure this prevents is the one that reports nothing: an array
		// filter with no GIN index still returns the right rows, by scanning
		// the table for them. ADR-0026 made the same argument for vectors.
		report(t.name, d.Name, "a Filterable array column needs a GIN index, or every filter over it is a sequential scan: add t.AddIndex(schema.Index{Columns: []string{%q}, Method: \"gin\"})", d.Name)
	}
}

// validateComputed refuses the claims a derived column cannot make.
//
// Each one is a claim about storage or about stability, and each is silent if
// it is allowed through: a defaulted computed column emits DDL for a column
// that does not exist, an indexed one names a column Postgres cannot index, and
// a volatile sort produces a keyset whose pages disagree with each other about
// the same row (ADR-0041).
func (r *Registry) validateComputed(t *TableDef, d *FieldDesc, report func(string, string, string, ...any)) {
	if !d.Computed() {
		report(t.name, d.Name, "Needs is only meaningful on a Computed column")
		return
	}
	if n := d.Placeholders(); n != len(d.Needs) {
		report(t.name, d.Name,
			"the expression takes %d bind(s) but Needs names %d (%s); each `?` takes the bind at the matching position, and `??` is a literal question mark",
			n, len(d.Needs), strings.Join(d.Needs, ", "))
	}
	for _, key := range d.Needs {
		if strings.TrimSpace(key) == "" {
			report(t.name, d.Name, "Needs contains an empty key")
		}
	}
	// Searchable is deliberately *not* refused here. The reason this check used
	// to give — "an expression has no reading there" — is a claim about type,
	// and the general rule above already enforces it: Searchable requires a
	// text column, computed or stored. What the blanket refusal added was the
	// refusal of a computed column whose declared type *is* text, which has a
	// perfectly good ILIKE reading and is the only way to search across a
	// relation (#93).
	//
	// The other half of the objection was cost — ?search over a correlated
	// subquery runs it per candidate row. That is now a decision the resource
	// makes rather than one every reader of the model inherits: a computed
	// column reaches a resource only if the resource selects it (#92).
	if d.Sortable && d.Volatile() {
		// ADR-0027 keysets on the sort column. An expression reading now() is a
		// different number on the next page, so the boundary a cursor recorded
		// no longer means what it meant — page 1 and page 50 disagree about
		// whether they have already shown a row.
		report(t.name, d.Name, "a computed column reading a volatile expression cannot be Sortable: the keyset cursor pages on the sort column, and this one does not hold still between pages")
	}
	if !d.ReadOnly {
		report(t.name, d.Name, "a computed column is ReadOnly; there is nothing to write to it")
	}
	if d.PrimaryKey {
		report(t.name, d.Name, "a computed column cannot be the primary key: a row is addressed by something the table stores")
	}
	if d.Unique {
		report(t.name, d.Name, "a computed column cannot be Unique: a unique constraint is an index, and there is no column to index")
	}
	if d.Default != nil {
		report(t.name, d.Name, "a computed column cannot have a Default: nothing writes it, so there is nothing to default")
	}
	if d.Ref != nil {
		report(t.name, d.Name, "a computed column cannot be a reference: a foreign key constrains a stored value")
	}
	if d.Scoped {
		report(t.name, d.Name, "a computed column cannot be Scoped: a tenant is a stored fact, and a scope predicate over an expression cannot be indexed")
	}
	if d.SoftDelete {
		report(t.name, d.Name, "a computed column cannot carry the soft-delete marker")
	}
	if d.Array {
		report(t.name, d.Name, "a computed column cannot be an Array: the array machinery is about a stored column's element type")
	}
	if d.Type == TypeVector {
		report(t.name, d.Name, "a computed column cannot be a vector: an embedding is stored, and a similarity search names the column it is stored in")
	}
	if d.Type == TypeEnum {
		report(t.name, d.Name, "a computed column cannot be an Enum: the value set is enforced by a CHECK constraint, and a computed column emits no DDL to carry one; declare it as Text")
	}
	if d.RenamedFrom != "" {
		report(t.name, d.Name, "a computed column cannot be RenamedFrom another: a rename is a migration, and this column was never in the database")
	}
	for _, idx := range t.indexes {
		for _, c := range idx.Columns {
			if c == d.Name {
				report(t.name, d.Name, "index %q covers a computed column; Postgres indexes stored columns, and an expression index is a different declaration this does not emit", idx.Name)
			}
		}
	}
}

// validateVector refuses the arrangements a vector column cannot be part of.
//
// Every one of these is a thing Postgres would accept and answer wrongly or
// uselessly, which is the pattern this whole file exists for. None of them is
// about the index, because there is no index kind yet — ADR-0026 stages that as
// a second decision, and an unindexed vector column is a complete
// configuration rather than a half-built one.
func (r *Registry) validateVector(t *TableDef, d *FieldDesc, report func(string, string, string, ...any)) {
	if d.Dim <= 0 {
		report(t.name, d.Name, "a vector column needs a dimension: schema.Vector(%q, n) where n is the width your embedder produces", d.Name)
	}
	// pgvector's own ceiling for a stored value. The lower limit that applies
	// to an *indexed* column is not checked here, because nothing declares an
	// index yet; when something does, that check belongs with the declaration
	// that carries the metric.
	if d.Dim > 16000 {
		report(t.name, d.Name, "a vector column is limited to 16000 dimensions, got %d", d.Dim)
	}
	if d.Array {
		report(t.name, d.Name, "a vector column cannot be an array: pgvector has no array-of-vector type, and one embedding per row is the shape every consumer of this has")
	}
	if d.PrimaryKey {
		report(t.name, d.Name, "a vector column cannot be the primary key: one column addresses a row in its URL and its cursor, and an embedding has no spelling in either")
	}
	if d.Unique {
		report(t.name, d.Name, "a vector column cannot be Unique: equality on an embedding is a comparison of floats, and two rows meaning the same thing do not produce the same vector")
	}
	if d.Ref != nil {
		report(t.name, d.Name, "a vector column cannot be a reference")
	}
	// The capabilities all fail for the same reason: the REST surface has no
	// spelling for a vector. A URL cannot carry one — 1,536 float32s is about
	// twenty kilobytes — which is why ADR-0026 makes similarity search its own
	// operation rather than a query parameter.
	if d.Filterable {
		report(t.name, d.Name, "a vector column cannot be Filterable: a filter value travels in a URL and an embedding does not fit in one. Similarity search is its own operation")
	}
	if d.Sortable {
		report(t.name, d.Name, "a vector column cannot be Sortable: ?sort names a column and a similarity ordering needs a vector to be near to")
	}
	if d.Searchable {
		report(t.name, d.Name, "a vector column cannot be Searchable: ?search is a text fan-out, and matching an embedding against a search term is the operation the embedder performs, not the database")
	}
	if !d.Hidden {
		// Reachable only by clearing it after construction, since Vector sets
		// it. Said out loud because the alternative is a REST response
		// carrying twenty kilobytes of float per row and nobody noticing until
		// the bill.
		report(t.name, d.Name, "a vector column must stay Hidden: an embedding has no use on the wire and serialising one is a mistake with no symptom except its size")
	}
	if d.Scoped {
		report(t.name, d.Name, "a vector column cannot be Scoped: a tenant predicate compares one value to a constant")
	}
	if d.SoftDelete {
		report(t.name, d.Name, "a vector column cannot carry the soft-delete marker")
	}
}

// hasGINIndex reports whether a GIN index covers the column. Leading position
// does not matter: GIN indexes every key in the row, so a column anywhere in
// the column list is searchable through it.
func (t *TableDef) hasGINIndex(column string) bool {
	for _, idx := range t.indexes {
		if !strings.EqualFold(idx.Method, "gin") {
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

// describeType spells a column's type the way an error message should, so that
// a refusal aimed at an array does not read as though it were aimed at text.
func describeType(d *FieldDesc) string {
	if d.Array {
		return string(d.Type) + "[]"
	}
	return string(d.Type)
}

func (r *Registry) validateInverse(t *TableDef, d *FieldDesc, claimed map[string]string, report func(string, string, string, ...any)) {
	ref := d.Ref
	if ref.Inverse == "" {
		if ref.InverseExpandable {
			report(t.name, d.Name, "InverseExpandable without Inverse: a relation with no name on the target cannot be asked for")
		}
		if ref.InverseOrder != "" || ref.InverseLimit != 0 {
			report(t.name, d.Name, "an expansion order or limit was declared without an Inverse to apply it to")
		}
		return
	}
	if ref.External {
		return // already reported, and nothing below can be checked
	}
	if !isIdent(ref.Inverse) {
		report(t.name, d.Name, "Inverse %q is not a valid identifier", ref.Inverse)
	}
	if ref.Table == nil {
		return // already reported
	}

	key := ref.Table.name + "." + ref.Inverse
	if prev, dup := claimed[key]; dup {
		report(t.name, d.Name,
			"Inverse %q is already claimed on %q by %s; two references to one table need two names, since the rows they collect are different sets",
			ref.Inverse, ref.Table.name, prev)
	}
	claimed[key] = t.name + "." + d.Name

	// The name lands as a field on the target, beside its columns.
	if ref.Table.Field(ref.Inverse) != nil {
		report(t.name, d.Name, "Inverse %q collides with a column of %q", ref.Inverse, ref.Table.name)
	}

	if ref.InverseLimit < 0 {
		report(t.name, d.Name, "ExpandLimit is %d, want a positive number", ref.InverseLimit)
	}
	// The order names a column of this table, because these are the rows being
	// collected — not of the target, which is the easy mistake to make.
	if col := strings.TrimPrefix(ref.InverseOrder, "-"); col != "" && t.Field(col) == nil {
		report(t.name, d.Name,
			"ExpandOrder %q is not a column of %q — an expanded collection is ordered by the rows it collects, which are this table's",
			col, t.name)
	}
	if d.Unique && (ref.InverseOrder != "" || ref.InverseLimit != 0) {
		report(t.name, d.Name,
			"ExpandOrder/ExpandLimit on Inverse %q has no effect: %s.%s is unique, "+
				"so at most one row can ever match; remove ExpandOrder/ExpandLimit",
			ref.Inverse, t.name, d.Name)
	}
}

// DefaultExpandLimit is the cap an expanded collection takes when it declares
// none. It mirrors the engine's own default, and sqlb's model test asserts the
// two agree — a schema package that disagreed with the runtime would publish a
// number the responses do not honour.
const DefaultExpandLimit = 50

// sharedEnumClaim is where a SharedAs name was first declared, so a second
// declaration under the same name can be reported against it.
type sharedEnumClaim struct {
	table, column string
	values        []string
}

// validateSharedEnum checks a column's SharedAs declaration against every
// other column already claiming the same name.
//
// The first column to use a name fixes the value set every later one must
// match — same values, in the same order. Order is part of the agreement and
// not just the set, because codegen emits the shared type and its constants
// exactly once, off the first declaration it renders; a second column with the
// same values in a different order would be asking for constants that do not
// exist under the names it expects, silently, since nothing about a mismatched
// order fails to compile.
func (r *Registry) validateSharedEnum(t *TableDef, d *FieldDesc, claimed map[string]sharedEnumClaim, report func(string, string, string, ...any)) {
	if d.Type != TypeEnum {
		report(t.name, d.Name, "SharedAs is only meaningful on an Enum column")
		return
	}
	if !isExportedGoIdent(d.SharedAs) {
		report(t.name, d.Name, "SharedAs(%q) is not an exported Go identifier; it becomes a type name in the generated package, so it needs a capital letter to start and letters, digits or underscores after that", d.SharedAs)
		return
	}

	prev, dup := claimed[d.SharedAs]
	if !dup {
		claimed[d.SharedAs] = sharedEnumClaim{table: t.name, column: d.Name, values: d.EnumValues}
		return
	}
	if !equalStrings(prev.values, d.EnumValues) {
		report(t.name, d.Name,
			"SharedAs(%q) is also declared on %s.%s, with a different value set: %s.%s has %s, %s.%s has %s — "+
				"every column sharing a SharedAs name must declare identical values in the identical order",
			d.SharedAs, prev.table, prev.column,
			prev.table, prev.column, quoteList(prev.values),
			t.name, d.Name, quoteList(d.EnumValues))
	}
}

// isExportedGoIdent reports whether s can name an exported Go type: a
// capital-letter start, and letters, digits or underscores after that.
//
// It is a small, deliberate duplicate of codegen's own identifier check rather
// than a shared one — schema cannot import codegen without a cycle, since
// codegen already imports schema, and the rule is four lines that is not
// worth inventing an import path for.
func isExportedGoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return s[0] >= 'A' && s[0] <= 'Z'
}

// equalStrings reports whether a and b hold the same values in the same
// order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// quoteList renders a value set the way an error message wants it: quoted,
// comma-separated, and in the declared order — the order is part of what a
// SharedAs mismatch is reporting.
func quoteList(vs []string) string {
	quoted := make([]string, len(vs))
	for i, v := range vs {
		quoted[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// InverseRelation is a reverse relation seen from the target's side: the rows
// of another table that point at this one, and the name this table knows them
// by.
//
// It is derived rather than declared here — the declaration lives on the
// referencing column, which is the side that already owns the constraint. What
// the target gains is a field on its generated struct and, if the reference
// exposed it, a name in its ?expand vocabulary. ADR-0022.
type InverseRelation struct {
	Name       string    // the name ?expand uses on the target
	Table      *TableDef // the table whose rows are collected
	Column     string    // that table's foreign key column
	Order      string    // ordering column, with a leading "-" for descending
	Limit      int       // cap as declared; zero means DefaultExpandLimit
	Expandable bool      // reachable through ?expand on the target
	// OneToOne reports that Column carries a single-column unique
	// constraint, so at most one row of Table can ever point back here. It
	// is derived, never declared: a unique foreign key is structurally
	// one-to-one whether or not the schema names it that way.
	OneToOne bool
}

// Cap is how many rows one expansion returns at most, with the default
// resolved. Anything published — the manifest, a generated tag — uses this
// rather than Limit, so a caller is never left to guess the number.
func (i InverseRelation) Cap() int {
	if i.Limit > 0 {
		return i.Limit
	}
	return DefaultExpandLimit
}

// Inverses returns the reverse relations pointing at t, in a deterministic
// order: by referencing table, then by declaration order within it.
func (r *Registry) Inverses(t *TableDef) []InverseRelation {
	if t == nil {
		return nil
	}
	var out []InverseRelation
	for _, src := range r.Tables() {
		for _, f := range src.Fields() {
			d := f.Desc()
			if d.Ref == nil || d.Ref.Inverse == "" || d.Ref.External || d.Ref.Table != t {
				continue
			}
			out = append(out, InverseRelation{
				Name:       d.Ref.Inverse,
				Table:      src,
				Column:     d.Name,
				Order:      d.Ref.InverseOrder,
				Limit:      d.Ref.InverseLimit,
				Expandable: d.Ref.InverseExpandable,
				OneToOne:   d.Unique,
			})
		}
	}
	return out
}
