package migrate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// Diff computes the changes that take current to target.
//
// It is a pure function over two registries rather than a comparison between a
// registry and a live database, which is the point: introspection produces the
// same *schema.Registry the DSL produces, so the same machinery generates a
// migration forwards and an import backwards, and the whole engine is testable
// without a database (ADR-0014).
//
// # One case where the two registries are not comparable as they stand
//
// Constraints are compared by their definition text, and for a CHECK that text
// is not the same on both sides. Postgres stores a check as a parse tree and
// hands back a canonical spelling — fully parenthesised, with explicit casts on
// literals — so `status <> 'done'` comes back from introspect as
// `(status <> 'done'::text)`. Diffing a declared registry against an
// introspected one therefore proposes dropping and re-adding every check they
// have in common, forever (issue #24).
//
// The same is true of a partial index's WHERE, which Postgres stores the same
// way — a declared `latitude IS NOT NULL` comes back as
// `(latitude IS NOT NULL)`, and the diff proposes creating an index that is
// already there with DDL that reads identically (issue #63).
//
// Call shadow.Normalize on the declared registry first, which puts both through
// the same normalisation by asking a Postgres. That is a
// separate call rather than something Diff does, because doing it here would
// mean taking a database — and being a pure function over two registries is
// what the paragraph above is about. `sqlb migrate` makes the call; anything
// diffing against introspect.Registry output should too.
//
// A caller that skips it gets a diff whose statements look identical to what
// the database already has, which is the part of both reports that cost the
// most to work out. So a rebuilt index and a replaced CHECK whose expression
// differs only in formatting say so in their Comment — an explanation, never a
// decision: see onlyThePredicateFormattingDiffers for why that distinction is
// the whole of its safety.
//
// Enums are unaffected: an enum is text plus a CHECK (ADR-0017), and introspect
// reads the values back out of the normalised form rather than comparing it.
//
// # What Destructive means here
//
// A change is marked Destructive when applying it can lose data that cannot be
// recovered by reversing it: dropping a table or column, or a type change that
// is not a widening. Adding NOT NULL to an existing column is included,
// because the fix for a failure is a backfill rather than a retry. Changes
// that merely fail loudly — adding a CHECK that existing rows violate,
// removing a value from an enum — are not destructive, since nothing is lost;
// they carry a Comment saying what to check instead.
//
// # What Lock means here
//
// Most DDL is a catalog write that nobody notices. A few statements hold their
// lock for a time proportional to the number of rows — because they rewrite the
// table, scan it, or build an index over it — and those are the ones that turn
// a routine migration into an outage. A change that does one carries Lock and
// Hazard, naming the lock and the sequence to use instead on a table too large
// to hold it.
//
// It is a note rather than a refusal, and unlike a destructive change it is not
// commented out. Whether a full scan matters depends on how many rows the table
// holds, which is not in the schema and never will be. Commenting out every
// SET NOT NULL would make the generator useless for the ordinary case and train
// people to uncomment without reading — which is how the destructive guard
// would stop working too. Migration.Blocking is the hook for a project that
// does know which of its tables are big.
//
// For the changes whose remedy is a fixed rewrite — adding a CHECK, a FOREIGN
// KEY or a UNIQUE, requiring a column — Unblock performs it, moving the scan or
// the index build out from under the lock. It is called rather than applied
// automatically, for the same reason the hazard is a note: the sequence is
// longer, splits the migration across files, and buys nothing on a table small
// enough that the scan is instant. What is left after Unblock is the type
// change, which has no mechanical alternative at all.
//
// # What waits for what
//
// A commented-out change is a statement that will not run, so anything needing
// what it would have created cannot run either. Adding a column NOT NULL with
// no default is destructive and comes out commented out; the UNIQUE, the CHECK
// and the index over that column would then fail with "column does not exist",
// turning a file that was meant to be a reviewable no-op into a migration that
// dies halfway. Those changes carry DependsOn and are commented out with it, so
// that one decision uncomments the whole set. See markDependents.
//
// # What is not inferred
//
// A rename is indistinguishable from a drop and an add when only the before
// and after states are known, so it has to be declared: schema.RenamedFrom
// says a column or a table used to be called something else, and the diff
// emits ALTER TABLE … RENAME. Without the hint a rename is a drop and an add,
// which is correct, lossy, and never silently wrong. Inferring one from a
// similar name and type is the tempting alternative and is rejected on
// consequence asymmetry: a wrong inference destroys a column of production
// data, a missing one costs a hint (ADR-0014).
//
// # Ordering
//
// Changes are ordered so that each one's dependencies already exist and
// nothing is dropped out from under something that still refers to it:
//
//  0. CREATE EXTENSION, before anything that declares a column of its type
//  1. CREATE TABLE for new tables
//  2. DROP INDEX for removed and changed indexes — before the columns they
//     cover can disappear
//  3. DROP CONSTRAINT, foreign keys first, since a foreign key depends on the
//     unique or primary key constraint it points at
//  4. RENAME, of tables, columns, constraints and indexes
//  5. ADD COLUMN and ALTER COLUMN
//  6. DROP COLUMN
//  7. ADD CONSTRAINT, foreign keys last, once every table and column exists
//  8. CREATE INDEX
//  9. DROP TABLE
//
// Rendering reverses this list for the Down section, which is exactly the
// mirror of it, so reversibility falls out of the ordering rather than being
// arranged separately.
//
// The renames sit where they do because that is the only place both sides work
// out. Everything before them is expressed in the old names and everything
// after in the new ones — and because the Down runs the list backwards, each
// half is reversed while the names it was written against are the ones in
// effect. Putting the renames first instead would leave every drop's Down
// re-adding a constraint against a column that no longer answers to that name.
func Diff(current, target *schema.Registry, opts ...Option) ([]Change, error) {
	if current == nil {
		current = schema.NewRegistry()
	}
	if target == nil {
		target = schema.NewRegistry()
	}
	// A registry that does not validate would produce DDL for a schema that
	// cannot exist — a reference to an unregistered table, a duplicate column.
	// Failing here beats failing halfway through a migration.
	if err := current.Validate(); err != nil {
		return nil, fmt.Errorf("migrate: current schema is not valid: %w", err)
	}
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("migrate: target schema is not valid: %w", err)
	}

	var cfg diffOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	d := &differ{current: current, target: target, opts: cfg}
	if err := d.run(); err != nil {
		return nil, err
	}
	return d.changes(), nil
}

// extensionsNeeded emits the CREATE EXTENSION statements the target schema
// requires and the current one does not already have.
//
// pgvector and btree_gist so far. Each is emitted when its trigger first
// appears and not on every migration afterwards, which is what makes it a
// change rather than a preamble — the statement is idempotent, so repeating
// it would be harmless and would also be noise in every file forever.
//
// The collision argument that keeps CREATE TYPE out of this generator does not
// apply. An extension is one global name, owned by nobody, and installing it
// twice is defined to do nothing; a type name is a thing two schemas can each
// believe they own (ADR-0026).
//
// This runs in the same diff — and so the same migration file — as the table
// or constraint that needs it (see phase ordering in changes()), which is
// what closes the ordering hazard issue #194 named: a schema-first bootstrap
// that put AddExclude's inline EXCLUDE in migration one and a hand-written
// CREATE EXTENSION btree_gist in migration two failed outright on a fresh
// database, because the extension has to exist before that first CREATE
// TABLE runs, not merely before the app starts.
//
// There is no Down. Dropping the extension would remove it for every table in
// the database that depends on it, including those belonging to schemas this
// migration has never heard of, and an extension nobody is using costs
// nothing to leave installed. An empty Down renders as a note saying it is not
// automatically reversible, which is the honest answer.
func (d *differ) extensionsNeeded() {
	for _, ext := range []struct {
		name string
		used func(*schema.Registry) bool
	}{
		{"vector", usesVector},
		{"btree_gist", usesBtreeGist},
	} {
		if !ext.used(d.target) || ext.used(d.current) {
			continue
		}
		d.extensions = append(d.extensions, Change{
			Comment: ext.name + " extension, required by a column type or constraint declared below",
			Up:      "CREATE EXTENSION IF NOT EXISTS " + quoteIdent(ext.name) + ";",
			Hazard: "CREATE EXTENSION usually needs privileges the migration role does not have. " +
				"If this fails it fails on the first statement, which is the best place for it to: " +
				"install the extension as a superuser once, and this statement becomes a no-op.",
		})
	}
}

// usesVector reports whether any table in the registry declares a vector
// column.
func usesVector(r *schema.Registry) bool {
	if r == nil {
		return false
	}
	for _, t := range r.Tables() {
		for _, f := range t.Fields() {
			if f.Desc().Type == schema.TypeVector {
				return true
			}
		}
	}
	return false
}

// usesBtreeGist reports whether any table declares a gist EXCLUDE constraint
// over an equality operator. A gist index natively covers ranges and
// geometric types; comparing a scalar with = inside one — the common case,
// pairing "coach_id WITH =" against "tstzrange(...) WITH &&" to scope an
// overlap check per coach — needs the operator classes btree_gist adds. This
// is a heuristic over the constraint's free-form Elements SQL (schema.Table's
// own [schema.Exclusion] doc comment says why Elements stays a string rather
// than a structured form), not a parse: it can miss an equality spelled with
// unusual whitespace, and it says nothing about a gist exclusion that needs
// no extension at all (ranges and geometric types alone). Both are false
// negatives, not false positives — the failure mode this exists to prevent
// (a migration that 500s on a fresh database) is not made worse by it.
func usesBtreeGist(r *schema.Registry) bool {
	if r == nil {
		return false
	}
	for _, t := range r.Tables() {
		for _, e := range t.Exclusions() {
			if strings.EqualFold(e.Using, "gist") && strings.Contains(e.Elements, "WITH =") {
				return true
			}
		}
	}
	return false
}

// differ accumulates changes into ordered phases. Phases exist because the
// correct order is not the order the comparison discovers things in: a column
// added to one table and an index dropped from another are found together and
// must be applied apart.
type differ struct {
	current, target *schema.Registry

	// opts is what the caller said about where this migration will run.
	// It reaches the DDL layer and nothing else.
	opts diffOptions

	// tableRenames maps an old storage name to its new one. A foreign key
	// names the table it points at, so a reference to a renamed table has to
	// be recognised as unchanged rather than dropped and re-added.
	tableRenames map[string]string

	// pendingColumns are the columns this diff adds with a change that renders
	// commented out, keyed as quoted table."column". Until somebody uncomments
	// that change the column does not exist, so nothing naming one of these can
	// run either. See markDependents.
	pendingColumns map[string]bool

	// extensions run before everything, because a table declaring a column of
	// an extension type cannot be created until the extension is.
	extensions     []Change
	createTables   []Change
	dropIndexes    []Change
	dropForeignKey []Change
	dropOther      []Change
	renames        []Change
	alterColumns   []Change
	dropColumns    []Change
	addOther       []Change
	addForeignKey  []Change
	createIndexes  []Change
	dropTables     []Change
}

func (d *differ) changes() []Change {
	var out []Change
	for _, phase := range [][]Change{
		d.extensions,
		d.createTables,
		d.dropIndexes,
		d.dropForeignKey,
		d.dropOther,
		d.renames,
		d.alterColumns,
		d.dropColumns,
		d.addOther,
		d.addForeignKey,
		d.createIndexes,
		d.dropTables,
	} {
		out = append(out, phase...)
	}
	d.markDependents(out)
	return out
}

// markDependents marks the changes that cannot run because a change they
// depend on renders commented out.
//
// A destructive change is emitted commented out, to be reviewed and uncommented
// before it is applied (ADR-0014). Until that happens the column an ADD COLUMN
// would have introduced does not exist — so a UNIQUE, a CHECK, a foreign key or
// an index naming it must be commented out as well. Emitting one live is how
// the generated file stops being a reviewable no-op and becomes a migration
// that fails partway through, leaving the schema in neither state.
//
// It runs over the finished list rather than at the point each change is built,
// for two reasons. It reads the Destructive flag that was actually set, so it
// cannot drift away from the rule that sets it — a guard recomputing the same
// condition somewhere else is a guard that can silently stop matching
// (ADR-0016). And it does not depend on the order the comparison happens to
// discover things in, which is a separate concern already stated once, in the
// phase list.
func (d *differ) markDependents(changes []Change) {
	if len(d.pendingColumns) == 0 {
		return
	}
	for i := range changes {
		c := &changes[i]
		if c.Destructive || c.DependsOn != "" {
			// Already commented out, and for a reason of its own that says
			// more than this one would.
			continue
		}
		c.DependsOn = d.dependencyOf(*c)
		if c.DependsOn == "" {
			continue
		}
		// Every step of an unblocked sequence inherits it, for the reason
		// notNullSequence gives: a sequence with half its statements commented
		// out is worse than either whole.
		for j := range c.unblocked {
			c.unblocked[j].DependsOn = c.DependsOn
		}
	}
}

// dependencyOf explains what a change is waiting for, or returns "" if nothing
// it names is pending.
//
// The two halves answer the same question with different certainty. A column
// list this package built is exact, and the note names the column outright. A
// hand-written CHECK expression or a partial index predicate is arbitrary SQL,
// and which columns it names cannot be known without parsing it — so it waits
// whenever *any* column of its own table is pending, and the note says so
// rather than claiming a match it did not make. Waiting unnecessarily costs a
// reviewer one more uncomment in a file they are already reading; guessing at
// the expression would put a wrong statement in the output, which ADR-0014
// refuses everywhere else for the same reason.
func (d *differ) dependencyOf(c Change) string {
	var named []string
	for _, key := range c.needsColumns {
		if d.pendingColumns[key] {
			named = append(named, key)
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		return joinWithAnd(named) + ", which an earlier change in this migration " +
			"adds and is commented out as destructive. Uncomment that first; " +
			"this statement fails without it"
	}
	if c.needsTable == "" {
		return ""
	}
	if pending := d.pendingIn(c.needsTable); len(pending) > 0 {
		return "possibly " + joinWithAnd(pending) + ", which an earlier change in " +
			"this migration adds and is commented out as destructive. This one is " +
			"hand-written SQL and is not read here, so whether it names that column " +
			"is for you to say — uncomment it alongside if it does"
	}
	return ""
}

// pendingIn returns the pending columns of one table, in a fixed order so that
// the note reads the same on every run.
func (d *differ) pendingIn(table string) []string {
	prefix := quoteIdent(table) + "."
	var out []string
	for key := range d.pendingColumns {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// columnKey identifies a column across tables, spelled the way the generated
// SQL spells it, so that matching one and naming it in a comment need no second
// representation.
func columnKey(table, column string) string {
	return quoteIdent(table) + "." + quoteIdent(column)
}

func (d *differ) run() error {
	d.tableRenames = map[string]string{}
	d.pendingColumns = map[string]bool{}
	d.extensionsNeeded()
	claimed := map[string]bool{}
	for _, t := range d.target.Tables() {
		cur := d.currentFor(t)
		if cur == nil {
			continue // created below, once the rename map is complete
		}
		claimed[cur.Name()] = true
		if cur.Name() != t.Name() {
			d.tableRenames[cur.Name()] = t.Name()
		}
	}

	for _, t := range d.target.Tables() {
		cur := d.currentFor(t)
		if cur == nil {
			if err := d.tableCreated(t); err != nil {
				return err
			}
			continue
		}
		if cur.Name() != t.Name() {
			d.renames = append(d.renames, Change{
				Comment: "rename table " + cur.Name() + " to " + t.Name(),
				Up:      renameTable(cur.Name(), t.Name()),
				Down:    renameTable(t.Name(), cur.Name()),
			})
		}
		if err := d.tableAltered(cur, t); err != nil {
			return err
		}
	}
	for _, t := range d.current.Tables() {
		if claimed[t.Name()] {
			continue
		}
		if err := d.tableDropped(t); err != nil {
			return err
		}
	}
	return nil
}

// currentFor returns the table in the current registry that t describes, or
// nil if t is new.
//
// A table matching by name wins over a rename hint, which is what makes a hint
// left behind after its migration was generated harmless: by then the table
// answers to its new name, the old one is gone, and the hint resolves to
// nothing. Deleting it is still the right thing to do — it is a claim about the
// schema that has stopped being true — but forgetting to does not produce a
// second rename.
func (d *differ) currentFor(t *schema.TableDef) *schema.TableDef {
	if cur := d.current.Get(t.Name()); cur != nil {
		return cur
	}
	if old := t.RenamedFromName(); old != "" {
		return d.current.Get(old)
	}
	return nil
}

// tableCreated emits the table and everything that comes with it.
func (d *differ) tableCreated(t *schema.TableDef) error {
	up, err := createTable(t, d.opts)
	if err != nil {
		return err
	}
	d.createTables = append(d.createTables, Change{
		Comment: "create table " + t.Name(),
		Up:      up,
		Down:    dropTable(t),
	})

	for _, c := range constraints(t) {
		if !c.fk {
			continue // already inline in CREATE TABLE
		}
		// No needsColumns: both ends are columns that arrive with a table
		// rather than by an ALTER — this table's own, and the primary key of
		// the one it points at — so neither can be pending. See
		// constraintNeeds.
		d.addForeignKey = append(d.addForeignKey, Change{
			Comment: "add foreign key " + c.name,
			Up:      addConstraint(t.Name(), c),
			Down:    dropConstraint(t.Name(), c.name),
		})
	}

	// The table is empty by construction, so its indexes cannot contend with
	// anything and do not need CONCURRENTLY — which keeps them in the same
	// file as the table instead of forcing a split.
	for _, idx := range sortedIndexes(t) {
		d.createIndexes = append(d.createIndexes, Change{
			Comment: "index " + idx.Name,
			Up:      createIndex(t, idx, false),
			Down:    dropIndex(idx.Name, false),
		})
	}
	return nil
}

func (d *differ) tableDropped(t *schema.TableDef) error {
	// DROP TABLE takes the table's own indexes and constraints with it, so
	// none of them need emitting separately.
	//
	// The Down recreates the table and its inline constraints, and stops
	// there. Its indexes and references are not restored: a reference may
	// point at another table dropped in the same migration, and getting that
	// order right in reverse is work in service of a rollback that cannot
	// bring the rows back anyway. The Reason says so rather than the Down
	// implying otherwise.
	down, err := createTable(t, d.opts)
	if err != nil {
		return err
	}
	d.dropTables = append(d.dropTables, Change{
		Comment:     "drop table " + t.Name(),
		Up:          dropTable(t),
		Down:        down,
		Destructive: true,
		Reason: "dropping table " + t.Name() + " deletes every row in it. " +
			"The Down recreates the table and its constraints, but not the data, the indexes or the references to it",
	})
	return nil
}

func (d *differ) tableAltered(cur, tgt *schema.TableDef) error {
	cols := columnRenames(cur, tgt)
	for _, old := range sortedKeys(cols) {
		d.renames = append(d.renames, Change{
			Comment: "rename " + tgt.Name() + "." + old + " to " + cols[old],
			Up:      renameColumn(tgt.Name(), old, cols[old]),
			Down:    renameColumn(tgt.Name(), cols[old], old),
		})
	}
	if err := d.columns(cur, tgt, cols); err != nil {
		return err
	}
	d.tableComment(cur, tgt)
	d.constraints(cur, tgt, cols)
	d.indexes(cur, tgt, cols)
	return nil
}

// columnRenames resolves a table's rename hints into old name → new name.
//
// A hint counts only when the old column is present and the new one is not:
// anything else means the rename has already been generated and applied, and
// the leftover hint describes a schema that no longer exists. See currentFor
// for why that is a no-op rather than an error.
func columnRenames(cur, tgt *schema.TableDef) map[string]string {
	out := map[string]string{}
	for _, f := range tgt.StoredFields() {
		td := f.Desc()
		old := td.RenamedFrom
		if old == "" || cur.Field(old) == nil || cur.Field(td.Name) != nil {
			continue
		}
		out[old] = td.Name
	}
	return out
}

func (d *differ) columns(cur, tgt *schema.TableDef, cols map[string]string) error {
	renamedFrom := make(map[string]string, len(cols))
	for old, name := range cols {
		renamedFrom[name] = old
	}

	// A computed column is not storage, so the diff does not see one: it has no
	// type to create, no default to alter, and a database that does not have it
	// is not behind.
	for _, f := range tgt.StoredFields() {
		td := f.Desc()
		existing := cur.Field(td.Name)
		if existing == nil {
			// A renamed column is not new: the rename is already emitted, and
			// what remains is whatever else changed about it at the same time.
			if old, renamed := renamedFrom[td.Name]; renamed {
				existing = cur.Field(old)
			}
		}
		if existing == nil {
			if err := d.columnAdded(tgt, td); err != nil {
				return err
			}
			continue
		}
		if err := d.columnAltered(tgt, existing.Desc(), td); err != nil {
			return err
		}
	}
	for _, f := range cur.Fields() {
		cd := f.Desc()
		// StoredField rather than Field: a column the target now computes is a
		// column the database should stop holding, so the drop is proposed.
		if tgt.StoredField(cd.Name) != nil || cols[cd.Name] != "" {
			continue
		}
		// The column drop runs after the renames, so it and its Down are both
		// written against the table's new name.
		d.dropColumns = append(d.dropColumns, Change{
			Comment:     "drop column " + tgt.Name() + "." + cd.Name,
			Up:          dropColumn(tgt.Name(), cd.Name),
			Down:        mustAddColumnDown(tgt.Name(), cd, d.opts),
			Destructive: true,
			Reason:      "dropping " + tgt.Name() + "." + cd.Name + " deletes its contents. The Down restores the column but not the values",
		})
	}
	return nil
}

// mustAddColumnDown renders the ADD COLUMN that reverses a drop. The column
// came from a registry that already rendered, so a failure here is impossible;
// an empty Down would render as "not reversible", which is honest either way.
func mustAddColumnDown(table string, d *schema.FieldDesc, opts diffOptions) string {
	sql, err := addColumn(table, d, opts)
	if err != nil {
		return ""
	}
	return sql
}

func (d *differ) columnAdded(t *schema.TableDef, td *schema.FieldDesc) error {
	up, err := addColumn(t.Name(), td, d.opts)
	if err != nil {
		return err
	}
	if td.Comment != "" {
		up += "\n" + commentOnColumn(t.Name(), td.Name, td.Comment)
	}
	c := Change{
		Comment: "add column " + t.Name() + "." + td.Name,
		Up:      up,
		Down:    dropColumn(t.Name(), td.Name),
	}
	// Postgres can add a NOT NULL column with a default without rewriting the
	// table, but a NOT NULL column with no default is simply rejected on any
	// table that has rows in it.
	if !td.Nullable && td.Default == nil {
		c.Destructive = true
		c.Reason = "adding NOT NULL column " + t.Name() + "." + td.Name +
			" with no default fails on a table that already has rows. Give it a default or backfill it"
		// Commented out means the column does not exist until somebody
		// uncomments this, and neither can anything that names it. Recorded
		// here, in the same branch that decides it, so the two cannot disagree.
		d.pendingColumns[columnKey(t.Name(), td.Name)] = true
	}
	d.alterColumns = append(d.alterColumns, c)
	return nil
}

func (d *differ) columnAltered(t *schema.TableDef, cd, td *schema.FieldDesc) error {
	curType, err := sqlType(cd)
	if err != nil {
		return err
	}
	tgtType, err := sqlType(td)
	if err != nil {
		return err
	}

	if curType != tgtType {
		c := Change{
			Comment: "change type of " + t.Name() + "." + td.Name + " from " + curType + " to " + tgtType,
			Up:      alterColumn(t.Name(), td.Name, "TYPE "+tgtType),
			Down:    alterColumn(t.Name(), td.Name, "TYPE "+curType),
		}
		c.Lock, c.Hazard = typeChangeHazard(t.Name(), cd, td)
		if c.Lock == "" && rewrites(td, cd) {
			// Widening a varchar is free and narrowing it is not, so the Up
			// costs nothing and the rollback rewrites the table. Worth saying
			// here, since the place it would otherwise be discovered is
			// halfway through an incident.
			c.Comment += " (free in this direction; reversing it rewrites the table)"
		}
		if !widens(cd, td) {
			c.Destructive = true
			c.Reason = "converting " + t.Name() + "." + td.Name + " from " + curType +
				" to " + tgtType + " is not a widening: values that do not fit are truncated or rejected"
			if cd.Type == schema.TypeVector && td.Type == schema.TypeVector {
				// Not truncation. Postgres refuses the cast outright, and the
				// deeper problem is that every stored vector was produced by an
				// embedder of the old width and means nothing at the new one.
				c.Reason = "changing " + t.Name() + "." + td.Name + " from " + curType + " to " + tgtType +
					" invalidates every stored embedding: they were produced by a different model and " +
					"cannot be converted, only recomputed. Re-embed the rows, and expect to do it in " +
					"batches rather than in this migration"
			}
			// No USING clause is generated. Postgres refuses a conversion it
			// cannot make implicitly, and refusing is what should happen: a
			// generated USING would pick a cast nobody reviewed, and casting
			// to a narrower type truncates silently.
			c.Comment += " (add a USING clause by hand if Postgres refuses the cast)"
		}
		d.alterColumns = append(d.alterColumns, c)
	}

	switch {
	case cd.Nullable && !td.Nullable:
		// Two separate problems, so two separate flags: rows holding NULL make
		// this fail, and proving that none do makes it slow.
		lock, hazard := setNotNullHazard(t.Name(), td.Name)
		c := Change{
			Comment:     "require " + t.Name() + "." + td.Name,
			Up:          alterColumn(t.Name(), td.Name, "SET NOT NULL"),
			Down:        alterColumn(t.Name(), td.Name, "DROP NOT NULL"),
			Destructive: true,
			Reason: "rows with NULL in " + t.Name() + "." + td.Name +
				" will fail this constraint. Backfill them before applying",
			Lock:   lock,
			Hazard: hazard,
		}
		c.unblocked = notNullSequence(t.Name(), td.Name, c)
		d.alterColumns = append(d.alterColumns, c)
	case !cd.Nullable && td.Nullable:
		// The Up is a catalog write, so this is not flagged — but reversing it
		// is the scan above, which is a surprising way to find that out during
		// a rollback.
		d.alterColumns = append(d.alterColumns, Change{
			Comment: "allow NULL in " + t.Name() + "." + td.Name +
				" (reversing this scans the table to prove no row took the opportunity)",
			Up:   alterColumn(t.Name(), td.Name, "DROP NOT NULL"),
			Down: alterColumn(t.Name(), td.Name, "SET NOT NULL"),
		})
	}

	curDefault, err := renderDefault(cd.Default, d.opts)
	if err != nil {
		return err
	}
	tgtDefault, err := renderDefault(td.Default, d.opts)
	if err != nil {
		return err
	}
	if !sameDefault(td.Type, curDefault, tgtDefault) {
		up, down := "DROP DEFAULT", "DROP DEFAULT"
		if tgtDefault != "" {
			up = "SET DEFAULT " + tgtDefault
		}
		if curDefault != "" {
			down = "SET DEFAULT " + curDefault
		}
		d.alterColumns = append(d.alterColumns, Change{
			Comment: "default of " + t.Name() + "." + td.Name +
				" (existing rows are not backfilled; a default applies to new rows only)",
			Up:   alterColumn(t.Name(), td.Name, up),
			Down: alterColumn(t.Name(), td.Name, down),
		})
	}

	if cd.Auto != td.Auto {
		d.alterColumns = append(d.alterColumns, autoChanges(t.Name(), cd, td)...)
	}

	if cd.Comment != td.Comment {
		d.alterColumns = append(d.alterColumns, Change{
			Comment: "describe " + t.Name() + "." + td.Name,
			Up:      commentOnColumn(t.Name(), td.Name, td.Comment),
			Down:    commentOnColumn(t.Name(), td.Name, cd.Comment),
		})
	}
	return nil
}

// autoChanges is the DDL for a column that starts or stops being supplied by a
// sequence, or that swaps one spelling of that for the other.
//
// The two spellings are not symmetric and the asymmetry is Postgres's. An
// identity is an attribute of the column, so it is added and dropped with one
// ALTER each and rolls back exactly. A serial is three objects wearing one
// name, and only the CREATE TABLE spelling assembles them — so building one on
// a column that already exists means creating the sequence, pointing the column
// at it, and giving it to the column to own, which is what the statements below
// do in that order.
//
// What none of this does is set the sequence past the rows already there.
// Nothing here knows how many that is, and a generated `setval` reading the
// column's own max is a full scan written into a migration by a tool that
// cannot see the table's size. It is named in the hazard instead, because
// getting it wrong is not a slow migration but a duplicate key on the first
// insert after it.
func autoChanges(table string, cd, td *schema.FieldDesc) []Change {
	var out []Change
	col := table + "." + td.Name

	// The old arrangement goes first, so a serial→identity swap does not leave
	// a nextval default sitting under an identity Postgres would refuse.
	switch {
	case cd.Auto.Identity():
		out = append(out, Change{
			Comment: "drop identity on " + col,
			Up:      alterColumn(table, td.Name, "DROP IDENTITY"),
			Down:    alterColumn(table, td.Name, "ADD "+identityClause(cd.Auto)),
		})
	case cd.Auto == schema.AutoSerial:
		out = append(out, Change{
			Comment: "stop " + col + " drawing from its sequence",
			Up:      alterColumn(table, td.Name, "DROP DEFAULT"),
			Down:    alterColumn(table, td.Name, "SET DEFAULT nextval("+sqlString(sequenceName(table, td.Name))+")"),
			Hazard: "the sequence " + sequenceName(table, td.Name) + " is left behind rather than dropped. " +
				"It is owned by the column, so it goes when the column does — drop it by hand if this column is staying",
		})
	}

	switch {
	case td.Auto.Identity():
		out = append(out, Change{
			Comment: "make " + col + " an identity column",
			Up:      alterColumn(table, td.Name, "ADD "+identityClause(td.Auto)),
			Down:    alterColumn(table, td.Name, "DROP IDENTITY"),
			Hazard: "the identity starts at 1. On a table that already has rows, set it past the " +
				"highest value first — ALTER TABLE " + table + " ALTER COLUMN " + td.Name +
				" RESTART WITH <max+1> — or the first insert collides with a row already there",
		})
	case td.Auto == schema.AutoSerial:
		seq := sequenceName(table, td.Name)
		out = append(out,
			Change{
				Comment: "create the sequence behind " + col,
				Up:      "CREATE SEQUENCE " + quoteIdent(seq) + " OWNED BY " + quoteIdent(table) + "." + quoteIdent(td.Name) + ";",
				Down:    "DROP SEQUENCE IF EXISTS " + quoteIdent(seq) + ";",
			},
			Change{
				Comment: "draw " + col + " from " + seq,
				Up:      alterColumn(table, td.Name, "SET DEFAULT nextval("+sqlString(seq)+")"),
				Down:    alterColumn(table, td.Name, "DROP DEFAULT"),
				Hazard: "the sequence starts at 1. On a table that already has rows, SELECT setval(" +
					sqlString(seq) + ", max(" + quoteIdent(td.Name) + ")) FROM " + quoteIdent(table) +
					" first, or the first insert collides with a row already there",
			},
		)
	}
	return out
}

// sequenceName is what Postgres would have called the sequence had the column
// been declared `bigserial` in the first place.
//
// The name is derived rather than declared, and that is a real limit worth
// stating: a database whose sequence is called something else round-trips as a
// serial — the declaration records that a sequence supplies the column, not
// which one — so a schema rebuilt from scratch gets this name instead. Nothing
// diffs against it, so no gate goes red over it; what changes is the name in a
// database built by `Diff` rather than by the original migration.
func sequenceName(table, column string) string {
	return table + "_" + column + "_seq"
}

func (d *differ) tableComment(cur, tgt *schema.TableDef) {
	if cur.Comment() == tgt.Comment() {
		return
	}
	d.alterColumns = append(d.alterColumns, Change{
		Comment: "describe " + tgt.Name(),
		Up:      commentOnTable(tgt.Name(), tgt.Comment()),
		Down:    commentOnTable(tgt.Name(), cur.Comment()),
	})
}

// constraints emits the changes between a table's current and target
// constraints.
//
// The current side is compared in its post-rename form, so that a constraint
// which merely follows a renamed column or table matches its target and
// produces nothing. It is *dropped* in its original form: a drop is ordered
// before the renames, so its Down runs after they have been reversed and the
// old names are the ones in effect.
func (d *differ) constraints(cur, tgt *schema.TableDef, cols map[string]string) {
	orig := byName(constraints(cur))
	curCons := make(map[string]constraint, len(orig))
	for name, c := range orig {
		curCons[name] = c.renamed(cols, d.tableRenames)
	}
	tgtCons := byName(constraints(tgt))
	usedCur, usedTgt := map[string]bool{}, map[string]bool{}

	// Same name: either unchanged, or replaced in place.
	for _, name := range sortedKeys(curCons) {
		t, kept := tgtCons[name]
		if !kept {
			continue
		}
		usedCur[name], usedTgt[name] = true, true
		if t.def == curCons[name].def {
			continue
		}
		prev := curCons[name]
		d.dropConstraintChange(cur.Name(), orig[name])
		d.addConstraintChange(tgt.Name(), t, &prev)
	}

	// Same definition, different name. Constraint names are derived from the
	// table and column they cover, so a rename changes them — and Postgres
	// does not rename a constraint when the thing it is named after is
	// renamed. Renaming it is a catalog write; dropping and re-adding it
	// revalidates every row in the table, and for a foreign key every row in
	// the table it points at as well.
	for _, name := range sortedKeys(curCons) {
		if usedCur[name] {
			continue
		}
		for _, want := range sortedKeys(tgtCons) {
			if usedTgt[want] || tgtCons[want].def != curCons[name].def {
				continue
			}
			usedCur[name], usedTgt[want] = true, true
			d.renames = append(d.renames, Change{
				Comment: "rename constraint " + name + " to " + want,
				Up:      renameConstraint(tgt.Name(), name, want),
				Down:    renameConstraint(tgt.Name(), want, name),
			})
			break
		}
	}

	// Whatever is left is genuinely gone, or genuinely new.
	for _, name := range sortedKeys(curCons) {
		if !usedCur[name] {
			d.dropConstraintChange(cur.Name(), orig[name])
		}
	}
	for _, name := range sortedKeys(tgtCons) {
		if !usedTgt[name] {
			d.addConstraintChange(tgt.Name(), tgtCons[name], nil)
		}
	}
}

func (d *differ) dropConstraintChange(table string, c constraint) {
	ch := Change{
		Comment: "drop constraint " + c.name,
		Up:      dropConstraint(table, c.name),
		Down:    addConstraint(table, c),
	}
	if c.fk {
		d.dropForeignKey = append(d.dropForeignKey, ch)
		return
	}
	d.dropOther = append(d.dropOther, ch)
}

func (d *differ) addConstraintChange(table string, c constraint, prev *constraint) {
	// The Down is just a drop. When this add replaces an earlier definition,
	// the drop of that definition was emitted in an earlier phase and its own
	// Down restores it, so reversing the whole list restores the old
	// constraint without this change having to know about it.
	// Every caller of this is altering a table that already exists and may hold
	// rows. A constraint on a table created by the same migration is emitted by
	// tableCreated, which does not come through here, because a table nothing
	// has inserted into yet costs nothing to constrain.
	lock, hazard := constraintHazard(table, c)
	needs, needsTable := constraintNeeds(table, c)
	ch := Change{
		Comment:      "add constraint " + c.name,
		Up:           addConstraint(table, c),
		Down:         dropConstraint(table, c.name),
		Lock:         lock,
		Hazard:       hazard,
		needsColumns: needs,
		needsTable:   needsTable,
	}
	switch removed := removedEnumValues(prev, c); {
	case len(removed) > 0:
		ch.Comment += " (no longer permits " + joinQuoted(removed) +
			"; rows still holding one will fail — migrate them first)"
	case prev != nil && onlyTheDefinitionFormattingDiffers(*prev, c):
		// Replacing a CHECK with one that reads the same is issue #24 seen from
		// the consumer's side, and this is where it gets to say so. Below the
		// enum clause, which is about a change in what the constraint permits
		// and outranks a remark about how it is spelled.
		ch.Comment += formattingNote("expression")
	case prev == nil:
		ch.Comment += " (existing rows must already satisfy it or this fails)"
	}
	if lock != "" {
		// A unique constraint has no NOT VALID form — there is no way to build
		// an index without reading every row — but the index can be built
		// beforehand under a weaker lock and then adopted.
		if c.unique {
			ch.unblocked = usingIndexSequence(table, c, ch)
		} else {
			ch.unblocked = notValidSequence(table, c, ch)
		}
	}
	if c.fk {
		d.addForeignKey = append(d.addForeignKey, ch)
		return
	}
	d.addOther = append(d.addOther, ch)
}

// constraintNeeds reports the columns an ADD CONSTRAINT cannot run without,
// which are the ones it covers in its own table. A hand-written CHECK reports
// its table instead; see dependencyOf.
//
// The far side of a foreign key is deliberately not included. schema.Ref points
// at the target table's primary key, which arrives with the table rather than
// by an ALTER, so it can never be a column this migration is still waiting to
// add. A reference to some other unique column — which the DSL cannot express
// today — would break that, and would have to name the referenced column here.
func constraintNeeds(table string, c constraint) (cols []string, wholeTable string) {
	for _, col := range c.covers {
		cols = append(cols, columnKey(table, col))
	}
	if c.handWritten {
		wholeTable = table
	}
	return cols, wholeTable
}

// indexNeeds reports the columns a CREATE INDEX cannot run without. A partial
// index's predicate is hand-written SQL over the same table, so it is treated
// the way a hand-written CHECK is.
func indexNeeds(table string, idx schema.Index) (cols []string, wholeTable string) {
	for _, col := range idx.Columns {
		cols = append(cols, columnKey(table, col))
	}
	if idx.Where != "" {
		wholeTable = table
	}
	return cols, wholeTable
}

// The lock-brief sequences.
//
// Each replaces one statement that scans a table under a lock nothing else can
// work through, with two or more that do the scanning under a lock everything
// can. They are built here, where the table and the constraint are still
// known, and substituted by Unblock — which is the caller's decision, since
// the reason to prefer them is a row count this package cannot see.
//
// The rule they all follow: the statement that scans must not share a
// transaction with one that took a strong lock, because a lock is held until
// the transaction commits and not until the statement ends. That is what
// StageValidate is for.

// notValidSequence is the two-part form of an ADD CONSTRAINT: add it without
// checking the rows already there, then prove them separately.
func notValidSequence(table string, c constraint, orig Change) []Change {
	return []Change{{
		Comment: orig.Comment + " — NOT VALID, so it binds new and updated rows " +
			"from here while the rows already there are proven separately",
		Up:   addConstraintNotValid(table, c),
		Down: dropConstraint(table, c.name),
	}, {
		Stage:   StageValidate,
		Comment: validateComment(table, c.name),
		Up:      validateConstraint(table, c.name),
		Down:    nothingToUndo(c.name),
	}}
}

// usingIndexSequence is the two-part form of adding a unique constraint: build
// the index it will be enforced by under a lock that lets writes through, then
// adopt the finished index as the constraint.
//
// Adoption reads nothing — the index is already there and already unique — so
// the ACCESS EXCLUSIVE it takes is held for a catalog write rather than for an
// index build.
func usingIndexSequence(table string, c constraint, orig Change) []Change {
	return []Change{{
		Stage: StageConcurrent,
		Comment: orig.Comment + " — built concurrently and adopted next. " +
			"If it fails on a duplicate, an invalid index is left behind and has " +
			"to be dropped before this can be retried; a plain ADD CONSTRAINT " +
			"would have left nothing. Reversing this drops the index only if the " +
			"constraint has not already taken it",
		Up:   createUniqueIndexConcurrently(table, c.name, c.cols),
		Down: dropIndexConcurrentlyIfExists(c.name),
	}, {
		Stage:   StageAdopt,
		Comment: "adopt " + c.name + " as the constraint it enforces (a catalog write; the index is already built)",
		Up:      addConstraintUsingIndex(table, c),
		// Dropping a unique constraint drops the index enforcing it, which is
		// why the build above reverses with IF EXISTS.
		Down: dropConstraint(table, c.name),
	}}
}

// notNullSequence is the lock-brief form of SET NOT NULL. Postgres 12 and later
// accept a validated CHECK as proof that a column holds no NULLs, so the scan
// can be done under a weak lock and the statement that takes the strong one has
// nothing left to look at.
//
// The check is dropped again at the end, so the table finishes exactly as the
// single statement would have left it. Anything else would be a constraint of
// this package's invention sitting in a schema that does not declare it, which
// the next diff would propose dropping.
//
// On a Postgres older than 12 every statement here still does the right thing;
// the SET NOT NULL simply scans again, and the sequence is a slower way to
// reach the same place rather than a broken one.
func notNullSequence(table, column string, orig Change) []Change {
	c := notNullCheck(table, column)
	out := []Change{{
		Comment: "prove " + table + "." + column + " holds no NULL before requiring it, " +
			"so that the requirement itself takes no scan — NOT VALID here, validated next",
		Up:   addConstraintNotValid(table, c),
		Down: dropConstraint(table, c.name),
	}, {
		Stage:   StageValidate,
		Comment: validateComment(table, c.name),
		Up:      validateConstraint(table, c.name),
		Down:    nothingToUndo(c.name),
	}, {
		// StageFinish, not StageValidate: this takes ACCESS EXCLUSIVE and holds
		// it until the transaction commits, so anything still scanning would
		// end up doing it underneath.
		Stage: StageFinish,
		Comment: "require " + table + "." + column +
			" (instant: Postgres finds the validated check and skips its own scan)",
		Up:   alterColumn(table, column, "SET NOT NULL"),
		Down: alterColumn(table, column, "DROP NOT NULL"),
	}, {
		Stage: StageFinish,
		Comment: "drop the check now the column carries the requirement itself, " +
			"leaving the table exactly as a plain SET NOT NULL would have",
		Up: dropConstraint(table, c.name),
		// Re-added NOT VALID rather than validated: on the way back the check
		// exists only so the migration that created it has something to drop,
		// and proving it again would be the scan this sequence avoided.
		Down: addConstraintNotValid(table, c),
	}}

	// Every step inherits the reason the original needed review. A sequence
	// with half its statements commented out is worse than either whole.
	for i := range out {
		out[i].Destructive = orig.Destructive
		out[i].Reason = orig.Reason
	}
	return out
}

func validateComment(table, name string) string {
	return "prove the rows already in " + table + " satisfy " + name +
		" — scans the table, under a lock readers and writers pass through"
}

// nothingToUndo is the Down of a validation. There is no statement that
// un-proves a constraint, and none is needed: the migration that added it drops
// it, and this says so rather than rendering as an unexplained gap.
func nothingToUndo(name string) string {
	return "-- a validation cannot be undone, and needs no undoing: " + name +
		" is dropped by the migration that added it"
}

// removedEnumValues reports which permitted values a replaced enum CHECK no
// longer allows. Removing one cannot lose data — Postgres rejects the whole
// statement — but it is the case worth naming, because the fix is in the data
// rather than in the schema.
func removedEnumValues(prev *constraint, next constraint) []string {
	if prev == nil || prev.enum == nil || next.enum == nil {
		return nil
	}
	keep := make(map[string]bool, len(next.enum))
	for _, v := range next.enum {
		keep[v] = true
	}
	var removed []string
	for _, v := range prev.enum {
		if !keep[v] {
			removed = append(removed, v)
		}
	}
	return removed
}

// indexes emits the changes between a table's current and target indexes. The
// current side is compared post-rename and dropped in its original form, for
// the reasons given on constraints.
func (d *differ) indexes(cur, tgt *schema.TableDef, cols map[string]string) {
	orig := indexesByName(cur)
	curIdx := make(map[string]schema.Index, len(orig))
	for name, idx := range orig {
		curIdx[name] = renamedIndex(idx, cols)
	}
	tgtIdx := indexesByName(tgt)
	usedCur, usedTgt := map[string]bool{}, map[string]bool{}

	for _, name := range sortedKeys(curIdx) {
		t, kept := tgtIdx[name]
		if !kept {
			continue
		}
		usedCur[name], usedTgt[name] = true, true
		if indexDef(t) == indexDef(curIdx[name]) {
			continue
		}
		d.indexDropped(cur, tgt, orig[name], curIdx[name])
		// A rebuild whose statement looks identical to the index already there
		// is what issue #63 was reported as, and the difference it does not
		// show is whitespace. Normalising the declared side is the fix and it
		// is a call the caller makes; this is for the caller who did not.
		var note string
		if onlyThePredicateFormattingDiffers(t, curIdx[name]) {
			note = formattingNote("predicate")
		}
		d.indexCreated(tgt, t, note)
	}

	// Same definition, different name: rebuilding an index to change its name
	// is the most expensive way to do nothing. ALTER INDEX … RENAME touches
	// the catalog and takes no lock worth naming.
	for _, name := range sortedKeys(curIdx) {
		if usedCur[name] {
			continue
		}
		for _, want := range sortedKeys(tgtIdx) {
			if usedTgt[want] || indexDef(tgtIdx[want]) != indexDef(curIdx[name]) {
				continue
			}
			usedCur[name], usedTgt[want] = true, true
			d.renames = append(d.renames, Change{
				Comment: indexRenameComment(name, want, curIdx[name].Unique),
				Up:      renameIndex(name, want),
				Down:    renameIndex(want, name),
			})
			break
		}
	}

	for _, name := range sortedKeys(curIdx) {
		if !usedCur[name] {
			d.indexDropped(cur, tgt, orig[name], curIdx[name])
		}
	}
	for _, name := range sortedKeys(tgtIdx) {
		if !usedTgt[name] {
			d.indexCreated(tgt, tgtIdx[name], "")
		}
	}
}

// indexDropped emits the drop of an index. orig is the index as it exists now,
// which is what the Down recreates; renamed is the same index with any column
// rename applied, which is what decides whether it is about to lose a column.
func (d *differ) indexDropped(cur, tgt *schema.TableDef, orig, renamed schema.Index) {
	// An index over a column that is going away is dropped without
	// CONCURRENTLY, which is what keeps it in the same file as the column
	// drop and therefore ordered before it — a concurrent one is split
	// into a file that runs afterwards, by which time Postgres has already
	// dropped the index along with the column and the statement fails.
	//
	// Nothing is lost by giving up CONCURRENTLY here: DROP COLUMN takes an
	// ACCESS EXCLUSIVE lock on the same table moments later, so the brief
	// lock this takes is one the migration was going to take anyway.
	concurrent := !coversDroppedColumn(renamed, tgt)
	d.dropIndexes = append(d.dropIndexes, Change{
		Comment:    "drop index " + orig.Name,
		Up:         dropIndex(orig.Name, concurrent),
		Down:       createIndex(cur, orig, concurrent),
		Stage:      concurrentStage(concurrent),
		dropsIndex: orig.Name,
	})
}

// indexCreated emits the creation of an index. note is appended to the comment
// and is usually empty; it carries the one thing the statement cannot say about
// itself, which is that it may be rebuilding an index that is already right.
func (d *differ) indexCreated(tgt *schema.TableDef, idx schema.Index, note string) {
	// The table already holds rows, so building the index without
	// CONCURRENTLY would lock it against writes for the duration.
	needs, needsTable := indexNeeds(tgt.Name(), idx)
	d.createIndexes = append(d.createIndexes, Change{
		Comment:      "index " + idx.Name + note + concurrentIndexRetryNote(),
		Up:           createIndex(tgt, idx, true),
		Down:         dropIndex(idx.Name, true),
		Stage:        StageConcurrent,
		needsColumns: needs,
		needsTable:   needsTable,
	})
}

// concurrentIndexRetryNote warns about the failure mode CONCURRENTLY has and
// a plain CREATE INDEX does not: it builds outside the migration's own
// transaction, so a build that fails — most commonly a unique or partial
// unique index meeting a row that already violates it, which Diff has no way
// to see coming since it never touches the database — does not roll back.
// Postgres leaves the catalog entry in place, marked invalid, instead of
// removing it. Reissuing the identical Up then fails with "already exists",
// an error that no longer names the real problem, unless the invalid index
// is dropped first — which is exactly what this change's own Down does.
func concurrentIndexRetryNote() string {
	return " (builds outside this migration's transaction: a failed build " +
		"leaves an invalid index behind rather than rolling back — run this " +
		"change's Down before retrying, or the retry fails with a confusing " +
		`"already exists" instead of the real error)`
}

// coversDroppedColumn reports whether the index spans a column the target no
// longer declares. Postgres drops the whole index when any indexed column
// goes, even a trailing one.
func coversDroppedColumn(idx schema.Index, tgt *schema.TableDef) bool {
	for _, col := range idx.Columns {
		if tgt.StoredField(col) == nil {
			return true
		}
	}
	return false
}

func byName(cs []constraint) map[string]constraint {
	m := make(map[string]constraint, len(cs))
	for _, c := range cs {
		m[c.name] = c
	}
	return m
}

func indexesByName(t *schema.TableDef) map[string]schema.Index {
	m := make(map[string]schema.Index, len(t.Indexes()))
	for _, idx := range t.Indexes() {
		m[idx.Name] = idx
	}
	return m
}

func sortedIndexes(t *schema.TableDef) []schema.Index {
	out := append([]schema.Index(nil), t.Indexes()...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedKeys keeps output deterministic across runs, which matters more here
// than usual: a migration that reorders itself between runs is a diff nobody
// can review.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinQuoted(vs []string) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = sqlString(v)
	}
	return joinWithAnd(out)
}

func joinWithAnd(vs []string) string {
	switch len(vs) {
	case 0:
		return ""
	case 1:
		return vs[0]
	}
	s := ""
	for i, v := range vs[:len(vs)-1] {
		if i > 0 {
			s += ", "
		}
		s += v
	}
	return s + " and " + vs[len(vs)-1]
}

// concurrentStage maps the decision an index change already made — whether to
// use CONCURRENTLY — onto the file it therefore belongs in.
func concurrentStage(concurrent bool) Stage {
	if concurrent {
		return StageConcurrent
	}
	return StageMain
}

// indexRenameComment says what a rename costs, which is nothing in the database
// and occasionally something in the application.
//
// Postgres reports a violated constraint by name, and matching that name is the
// standard way to tell one unique violation from another — so renaming a unique
// index can turn a handled collision into an unhandled 500 without touching the
// code that handled it. The declaration can say what the name is
// (schema.TableDef.UniqueIndexNamed), and this is where a reader finds that out
// before applying the migration rather than after (issue #57).
func indexRenameComment(from, to string, unique bool) string {
	c := "rename index " + from + " to " + to
	if unique {
		c += " (a unique index's name reaches the application: Postgres reports it in a " +
			"23505 error, so code matching " + from + " by name stops matching. " +
			"UniqueIndexNamed keeps the old name if something does)"
	}
	return c
}

// sameDefault reports whether two rendered defaults mean the same thing.
//
// Textual equality answers it for almost every type, and for those this is that
// comparison. The exception is jsonb, where the same default has two spellings
// and each side of a database diff picks a different one: introspect normalises
// a literal default into schema.Value, which renders as `'[]'`, while a
// declaration reaching for schema.Expr — the natural choice, since it is what
// the raw DDL says — renders as `'[]'::jsonb`. Those two strings are not equal
// and the defaults are, so a diff proposed SET DEFAULT on every run, forever
// (issue #56).
//
// The comparison is semantic rather than textual: both sides are unwrapped to
// their JSON text and compared as documents, so `'{"a":1,"b":2}'::jsonb` and
// `'{"b": 2, "a": 1}'` are one default — which is what Postgres itself thinks,
// since jsonb stores a parsed value and not the text it arrived as.
//
// Only for a jsonb column. Doing it for text would make `'{"a":1}'` and
// `'{ "a" : 1 }'` compare equal on a column where they are genuinely different
// strings.
func sameDefault(t schema.Type, cur, tgt string) bool {
	if cur == tgt {
		return true
	}
	if t != schema.TypeJSON {
		return false
	}
	curDoc, ok := jsonDefaultValue(cur)
	if !ok {
		return false
	}
	tgtDoc, ok := jsonDefaultValue(tgt)
	if !ok {
		return false
	}
	return jsonEqual(curDoc, tgtDoc)
}

// jsonDefaultValue extracts the JSON text from a rendered default, unwrapping
// the cast and the quoting that the two spellings differ in.
//
// It reports false for anything that is not a literal — `now()`, a function
// call, an expression over other columns — because those are not documents to
// compare and their text is the only thing that can be.
func jsonDefaultValue(rendered string) (string, bool) {
	s := strings.TrimSpace(rendered)
	// The cast is what a hand-written default carries and a normalised one does
	// not: '[]'::jsonb and '[]' are the same default.
	for _, cast := range []string{"::jsonb", "::json", "::JSONB", "::JSON"} {
		s = strings.TrimSuffix(strings.TrimSpace(s), cast)
	}
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	// Postgres doubles an embedded quote inside a string literal, which is the
	// only escape in one.
	return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), true
}

// jsonEqual compares two JSON documents by value, so that key order and
// whitespace do not decide whether a migration is generated.
func jsonEqual(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
