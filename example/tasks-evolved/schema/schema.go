// Package schema is not the schema of tasks-evolved — it is seven of them.
//
// example/tasks grew by addition only: every migration it has ever produced
// adds a table or a column, and nothing declared here has to reckon with what
// came before it meaning something different. This module asks the other
// question. A schema in its second year renames things, narrows things, splits
// a column into a table, and drops a column something else still names. Each
// of those needs a different answer from migrate.Diff than "add it", and this
// package is the sequence that provokes each answer in turn.
//
// Each Vn function below returns a brand new *schema.Registry, built from
// scratch with its own schema.NewRegistry() and its own reg.Table(...) calls.
// There is no shared package-level registry for the steps to collide over —
// schema.Registry already is the unit of isolation the DSL offers (see
// schema/registry.go's own doc comment), and a registry that outlived one step
// would make the "previous" and "next" schemas the same Go value, which is not
// what a real migration ever diffs. The repetition between neighbouring Vn
// functions is deliberate: it is what a second migration actually looks like
// next to the first one, not a refactoring opportunity.
//
// The naming: V0 is the baseline every history in example/tasks already
// covers. V1Bad exists only to be diffed and never applied — it is the trap.
// V1 through V6 are the six non-additive steps the module's tests walk,
// applied in order against one running database so that data really does
// carry from step to step.
package schema

import "github.com/mind-vm/sqlb/schema"

// labelsGIN is schema.Index{Columns: []string{"labels"}, Method: "gin"},
// factored out because every Vn function that still carries the labels array
// column has to declare it: Registry.Validate refuses a Filterable array
// column with no GIN index behind it (schema/registry.go's validateArray),
// on the same argument ADR-0026 makes for vectors — a filter with no index
// still returns the right rows, silently, by scanning the table for them.
func labelsGIN() schema.Index {
	return schema.Index{Columns: []string{"labels"}, Method: "gin"}
}

// addUsers declares the one table that never changes across every step: the
// users a task can be assigned to. It exists so that every Vn function below
// can build a fresh registry and still get back the same *schema.TableDef a
// Ref needs — a foreign key target has to live in the registry doing the
// pointing, so there is no way to share it across registries even though its
// shape never moves.
func addUsers(reg *schema.Registry) *schema.TableDef {
	return reg.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
}

// V0 is the baseline: two tables, the shape docs/special-cases.md's census
// describes as "generated once with -force and never changed". Everything
// after this is the second year.
func V0() *schema.Registry {
	reg := schema.NewRegistry()
	addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("status", "todo", "doing", "done").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V1Bad renames tasks.status to tasks.state the way a first attempt reaches
// for: change the name, nothing else. It is never applied — Diff(V0, V1Bad)
// is inspected and discarded, because what it proposes is a DROP COLUMN status
// and an ADD COLUMN state, and applying that to a table with rows would lose
// every one of them. This function is the trap the rest of step 1 exists to
// avoid.
func V1Bad() *schema.Registry {
	reg := schema.NewRegistry()
	addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V1 is the fix: the same rename, with RenamedFrom telling Diff it is the
// same column wearing a new name. Diff(V0, V1) proposes ALTER TABLE tasks
// RENAME COLUMN status TO state instead, and every row's value survives it.
//
// RenamedFrom is needed for exactly one release (its own doc comment says
// so), which is why V2 below does not carry it forward: by V2 the rename has
// already been generated and applied, and a stale hint would claim a rename
// that is no longer true.
func V1() *schema.Registry {
	reg := schema.NewRegistry()
	addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done").
			RenamedFrom("status").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V2 widens the state enum by one value: "blocked" joins the three V1
// already had. This rewrites the CHECK constraint behind the enum (Diff
// drops and re-adds it, since Postgres has no ALTER CONSTRAINT for what a
// CHECK permits) but is not Destructive — nothing already stored stops being
// valid, so no row can violate the new, larger set. migrate/diff.go's own
// package doc explains why the comparison that decides this reads Postgres's
// own rendered spelling of the constraint rather than the declared Go string:
// a CHECK comes back from Postgres as a parse tree, not the text it was
// declared with, so comparing anything else proposes rebuilding every check
// on every diff, forever (issue #24).
func V2() *schema.Registry {
	reg := schema.NewRegistry()
	addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V3Direct adds assignee_id the way a schema with no existing rows could get
// away with: UUID, Ref(users), NOT NULL, no default, in one step. Diffed
// against V2 it is never applied to the live database in this test — it
// exists to be inspected and, once, actually executed against a table that
// already has rows, to see what Postgres itself does when a Destructive
// change is run anyway. (Spoiler: it refuses, cleanly, in one statement.)
func V3Direct() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V3Nullable is the honest first half of adding a required reference to a
// table with rows in it: the column arrives nullable, so ADD COLUMN is a
// catalog write nobody has to review. Diff(V2, V3Nullable) is not
// Destructive.
func V3Nullable() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Ref("assignee", users).Nullable().Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V3NotNull is the second half, reached only after every row has been
// hand-backfilled (see evolve_test.go's step 3 — that backfill is DML, and
// migrate renders DDL only, which is the asymmetry step 3 is about).
// Diff(V3Nullable, V3NotNull) proposes ALTER COLUMN assignee_id SET NOT NULL,
// and Diff marks it Destructive regardless of what the live data actually
// looks like — a pure function over two registries has no way to know a
// backfill already ran. Applying it here succeeds anyway, because by the
// time it runs, it is true.
func V3NotNull() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	return reg
}

// V4WithJoinTable adds task_labels(id, task_id, label) alongside the labels
// array column, which is still declared here — both exist at once so that the
// hand-written DML in evolve_test.go's step 4 has an array column to read
// from and a table to copy into before the array column goes away. This is
// the ADR-0033 array-column shape, reversed: that record chose the array
// specifically to avoid the join table this step reintroduces.
func V4WithJoinTable() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	tasks := reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Text("labels").Array().Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	).AddIndex(labelsGIN())
	reg.Table("task_labels",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("task", tasks).Filterable(),
		schema.Text("label").Filterable(),
	).Index("task_id")
	return reg
}

// V4Final drops the labels array column now that task_labels holds the same
// data. Diff(V4WithJoinTable, V4Final) proposes DROP COLUMN labels, which is
// Destructive — Diff cannot see that evolve_test.go already copied every
// element into task_labels first. The GIN index that labels needed goes with
// it: it is not declared here, and Diff proposes dropping it in the same
// migration, before the column drop (DROP INDEX outranks DROP COLUMN in
// migrate/diff.go's ordering, precisely so an index is never left pointing at
// a column that is about to disappear).
func V4Final() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	tasks := reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Int("priority").Default(schema.Value(0)).Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	)
	reg.Table("task_labels",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("task", tasks).Filterable(),
		schema.Text("label").Filterable(),
	).Index("task_id")
	return reg
}

// V5 drops priority. The full docs/special-cases.md entry imagines this as
// "a client generated one commit ago" still selecting the dropped column;
// this lean module has no client-codegen harness, so the step is narrowed to
// what Diff itself does: mark the drop Destructive, name why, and — the part
// worth showing — render it commented out by default rather than live.
func V5() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	tasks := reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	)
	reg.Table("task_labels",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("task", tasks).Filterable(),
		schema.Text("label").Filterable(),
	).Index("task_id")
	return reg
}

// V6 adds a partial unique index: one task in progress ("doing") per
// assignee. Diff renders the CREATE UNIQUE INDEX exactly as declared — it
// does not check it against live data, because Diff is a pure function over
// two registries and never sees a row. evolve_test.go's step 6 seeds two
// "doing" rows sharing an assignee before this diff runs, so applying the
// index Diff proposes fails at the database, not at diff time.
func V6() *schema.Registry {
	reg := schema.NewRegistry()
	users := addUsers(reg)
	tasks := reg.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("state", "todo", "doing", "done", "blocked").
			Default(schema.Value("todo")).
			Filterable(),
		schema.Ref("assignee", users).Filterable(),
		schema.Timestamps(),
	)
	tasks.AddIndex(schema.Index{
		Unique:  true,
		Columns: []string{"assignee_id"},
		Where:   "state = 'doing'",
	})
	reg.Table("task_labels",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("task", tasks).Filterable(),
		schema.Text("label").Filterable(),
	).Index("task_id")
	return reg
}
