// Package tasks2schema is the schema for the tasks2 example: a from-scratch
// rebuild of example/tasks with the auth and multi-tenancy stripped out, so
// that what is left is only what defining a schema, a declared action and a
// declared query actually costs.
//
// example/tasks answers "what does a real application look like". This one
// answers a narrower question: starting from nothing, how many decisions does
// declaring a table, a declared action (in both its item and collection
// forms) and a declared query (schema.Query, the ADR-0043 read-side
// prototype) actually take.
package tasks2schema

import "github.com/mind-vm/sqlb/schema"

// List groups tasks. There is exactly one tenant in this example — the whole
// database — so unlike example/tasks's Workspace there is nothing to scope by
// and nothing here declares Scoped.
var List = schema.Table("lists",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("name").Searchable().Sortable(),
	schema.Timestamps(),
).
	Describe("A group of tasks.").
	Expose(schema.REST{
		Path:            "/lists",
		Ops:             schema.CRUD | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})

// Task is a unit of work in a list.
var Task = schema.Table("tasks",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Ref("list", List).OnDelete(schema.Cascade).Filterable(),
	schema.Text("title").Searchable().Sortable(),
	schema.Enum("status", "todo", "done").
		Default(schema.Value("todo")).
		Filterable().
		Sortable(),
	schema.Timestamp("due_at").Nullable().Filterable().Sortable(),
	// Owned by the "complete" mutation below, not by the client — the same
	// standing completed_at has in example/tasks and for the same reason: a
	// request that could set status=done and completed_at=null independently
	// would write a state the check constraint forbids.
	schema.Timestamp("completed_at").Nullable().ReadOnly().Filterable().Sortable(),
	schema.Timestamps(),
).
	Index("list_id").
	Index("due_at").
	Check("done_tasks_have_a_completion_time",
		"status <> 'done' OR completed_at IS NOT NULL").
	Describe("A unit of work, belonging to one list.").
	Expose(schema.REST{
		Path:            "/tasks",
		Ops:             schema.CRUD | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
		MaxFilters:      8,
	}).
	// The item-form action: row-scoped by having "{id}" in its path (the
	// default), which a transition PATCH cannot express — two columns have to
	// move together and a task already done must be refused rather than
	// silently re-completed. See mutations.go for CompleteTask. (ADR-0057
	// tried a separate schema.Mutation type for this shape and retired it the
	// same day: an item-form Action was always the same envelope.)
	AddAction(schema.Action{
		Name: "complete",
		Body: schema.Body(
			schema.Text("note").Nullable().Comment("Ignored in this example; kept to show a body works."),
		),
		Writes:      []string{"status", "completed_at"},
		Description: "Marks the task done and stamps its completion time. A task that is already done is refused with a 409.",
	}).
	// The collection-form action: no {id} in the path, so there is no row to
	// fetch and none of the item form's envelope applies — no BeforeQuery, no
	// lock, no Writes to persist afterward. The transaction is still there
	// (this runs inside the same write() the item form does), but confining
	// what it touches is entirely the func's own job, the position
	// sqlb.Query in application code is already in. See mutations.go for
	// ClearCompleted.
	AddAction(schema.Action{
		Name: "clear-completed",
		Path: "/clear-completed",
		Body: schema.Body(
			schema.UUID("list_id").Nullable().Comment("Only clear completed tasks in this list; omit to clear every list."),
		),
		Description: "Deletes every task whose status is done, optionally scoped to one list.",
	}).
	// The query: a read the filter grammar cannot express as one call, because
	// "not done and due before X" is fine as a filter (?status=neq.done&due_at=lt.…)
	// but the point of building this table is to prove the *shape* — Params
	// declared, Do bound at registration, result type whatever Do returns —
	// works end to end. See app/queries.go for overdueTasks.
	AddQuery(schema.Query{
		Name: "overdue",
		Params: schema.Body(
			schema.Timestamp("as_of").Comment("Tasks due before this time are overdue."),
		),
		// No Reads: the table this query is declared on — tasks — is implicit,
		// and it reads nothing else.
		Description: "Tasks that are not done and were due before the given time.",
	})
