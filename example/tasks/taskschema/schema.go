// Package taskschema is the schema definition for the task-manager example:
// the single source of truth that an author, or an agent, edits.
//
// It lives in its own package because the declarations here and the model
// structs generated from them share names — taskschema.Task is the table
// declaration, tasks.Task is the row struct. Keeping them apart is what lets
// both be called Task.
//
// # What this example is for
//
// example/blog shows the shortest path from a schema to a server. This one
// shows what the same machinery looks like once the application has a real
// shape: six tables, a tenant boundary that must hold, and an authentication
// story. The parts worth reading for are the ones blog cannot demonstrate —
//
//   - a workspace boundary enforced by one BeforeQuery registration per model
//     rather than by every handler and every call site remembering;
//   - JWT claims arriving from HTTP middleware and reaching the query layer
//     through the context, which is the only channel a hook has;
//   - hand-written endpoints (register, login) alongside generated CRUD, on
//     the same router and in the same OpenAPI document;
//   - a migration history generated from this file and applied by goose.
package taskschema

import "github.com/mind-vm/sqlb/schema"

// Workspace is the tenant. Every other table except User is scoped to one, and
// the scoping is enforced in hooks rather than in handlers.
var Workspace = schema.Table("workspaces",
	// Scoped on the key, because on this table the row *is* the tenant. There
	// is no workspace_id to point at, and a convention that only covers the
	// tables carrying the column would leave GET /workspaces listing every
	// tenant in the installation — which is the hole a schema-level convention
	// silently leaves behind when one table does not follow it.
	schema.UUIDv7("id").PrimaryKey().Scoped(),
	schema.Text("name").Searchable().Sortable(),
	schema.Text("slug").Unique().Filterable(),
	schema.Timestamps(),
).
	Describe("A tenant. Lists, tasks and comments all belong to exactly one.").
	Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

// User is a person. Users are global rather than per-workspace — one login
// reaches every workspace the user is a member of — so this is the one table
// the workspace hook does not scope.
var User = schema.Table("users",
	// The one table with no workspace column, and still scoped: which users a
	// caller may see is a question about memberships, so the hook narrows the
	// key with a subquery rather than comparing a column. The declaration goes
	// where the predicate lands, which is here.
	schema.UUIDv7("id").PrimaryKey().Scoped(),
	schema.Text("email").Unique().Searchable(),
	schema.Text("name").Searchable().Sortable(),

	// Never leaves the process. Hidden also means not filterable: a filterable
	// secret can be recovered a character at a time by probing, which is why
	// the schema validator rejects a column that declares both.
	schema.Text("password_hash").Hidden(),

	schema.Timestamps(),
).
	// No OpCreate: accounts are made by POST /auth/register, which also creates
	// the first workspace and hashes the password. Generated CRUD would let a
	// caller write password_hash directly — except that it cannot, because the
	// column is Hidden, which would instead produce an account nobody can log
	// in to.
	//
	// Half of that is now expressible: schema.REST's CreateInput would declare
	// a `password` property that is not a column, and a BeforeCreate hook would
	// hash it into password_hash (#309). What still keeps this one hand-written
	// is the other half — registering also makes the first workspace and its
	// membership, and the response is a session rather than a row.
	Expose(schema.REST{Ops: schema.OpRead | schema.OpList, MaxPageSize: 100})

// Profile is a user's extended profile, split into a table of its own rather
// than columns on User because it exists for one reason: user_id carries a
// single-column Unique constraint, which is what makes the relation
// structurally one-to-one and is the fixture the reverse side proves itself
// against end to end. GET /users?expand=profile resolves through the Inverse
// declared below to the row or null — never the {items, has_more} envelope
// every other Inverse relation in this schema returns, because at most one
// profile can ever point back at a given user.
//
// Only OpCreate is exposed — no GET /profiles, no GET /profiles/{id} — and
// that is a tenancy constraint, not an oversight. profiles has no
// workspace_id: a profile is 1:1 with a User, and User is the one table in
// this schema that is *global* rather than workspace-owned (a login reaches
// every workspace it is a member of), so there is no single workspace a
// profile could carry either. That leaves this schema no way to declare a
// Scoped column for it — every other exposed table's tenant boundary is
// either a workspace_id (Scoped, ReadOnly, filtered by app/hooks.go's
// scopeReads) or, for User itself, a membership subquery. The subquery answer
// does not carry over: app/hooks.go's User hook needs RawPred (a membership
// test is not expressible with F() and comparison operators), and a RawPred
// BeforeQuery hook cannot be requalified onto a join alias (sqlb/qualify.go)
// — so registering the same hook here would make every GET
// /users?expand=profile fail at request time, breaking the one endpoint this
// table exists to prove. Direct listing is left unexposed rather than
// shipped unscoped; app/hooks.go's Profile BeforeCreate hook closes the
// write-side version of the same gap by checking user_id against the
// caller's own already-scoped membership, one row at a time, rather than
// with a query-level predicate.
var Profile = schema.Table("profiles",
	schema.UUIDv7("id").PrimaryKey(),
	// Unique is what makes this one-to-one (no separate schema verb for it —
	// the constraint already says so). Inverse names the reverse relation, and
	// InverseExpandable is the separate decision that exposes it through
	// ?expand on /users; declaring one without the other would leave "profile"
	// in the manifest with no endpoint that can ever return it.
	//
	// Not Filterable: this table has no GET at all (see the Ops below), so a
	// filter capability here would buy nothing and only generate a misleading
	// `?user_id=eq.…` example and a ProfileWhere/ProfileColumn client type for
	// a query parameter no route ever reads.
	schema.Ref("user", User).OnDelete(schema.Cascade).Unique().
		Inverse("profile").InverseExpandable(),
	schema.Text("bio").Nullable(),
	schema.Timestamps(),
).
	Describe("A user's extended profile. One-to-one with users: user_id is unique.").
	Expose(schema.REST{
		Path: "/profiles",
		Ops:  schema.OpCreate,
	})

// Membership is what makes a user part of a workspace, and carries the role
// that authorisation reads. It is also the table the login endpoint consults to
// decide which workspaces a token may be issued for.
var Membership = schema.Table("memberships",
	schema.UUIDv7("id").PrimaryKey(),

	// ReadOnly on every workspace_id in this schema, and it is the single most
	// load-bearing word in the file.
	//
	// ReadOnly keeps a column out of the generated create and patch bodies, so
	// no request can name the workspace it is writing into — and leaves the
	// BeforeCreate hook free to supply it from the verified token. The column
	// appears nowhere in the OpenAPI document as an input, and a client that
	// sends one is not silently overruled, because there is nothing to send.
	//
	// The alternative, Immutable, keeps it out of the patch body only. That
	// closes the worse hole (a task moved between workspaces after the fact)
	// and leaves a required create field the server ignores.
	schema.Ref("workspace", Workspace).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),

	schema.Ref("user", User).OnDelete(schema.Cascade).Filterable(),
	schema.Enum("role", "owner", "admin", "member").
		Default(schema.Value("member")).
		Filterable().
		Sortable(),
	schema.Timestamps(),
).
	// One membership per user per workspace. This is the constraint the
	// register and invite paths both rely on rather than checking first.
	UniqueIndex("workspace_id", "user_id").
	Describe("A user's membership of a workspace, and their role in it.").
	Expose(schema.REST{
		Path:            "/memberships",
		Ops:             schema.OpRead | schema.OpList | schema.OpCreate | schema.OpDelete,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})

// List is a task list — the "different task lists" a workspace organises work
// into. Archiving is a flag rather than a delete so that a list's tasks keep
// their home.
var List = schema.Table("lists",
	schema.UUIDv7("id").PrimaryKey(),
	// ReadOnly, supplied by the BeforeCreate hook; see the note on Membership.
	schema.Ref("workspace", Workspace).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),

	schema.Text("name").Searchable().Sortable(),
	schema.Text("description").Searchable(),
	schema.Text("color").Default(schema.Value("#6b7280")).Filterable(),
	schema.Int("position").Default(schema.Value(0)).Sortable(),
	schema.Bool("archived").Default(schema.Value(false)).Filterable().Sortable(),

	schema.Timestamps(),
	schema.SoftDelete(),
).
	Index("workspace_id", "archived").
	// Two lists in one workspace may not share a name. Across workspaces they
	// may, which is why the index is composite rather than on name alone.
	UniqueIndex("workspace_id", "name").
	// Redundant on its own — id is already unique — and declared anyway,
	// because a composite FOREIGN KEY needs a unique constraint covering
	// exactly the columns it references. cmd/migrate adds
	// tasks (workspace_id, list_id) → lists (workspace_id, id), which is what
	// makes it impossible for a task to point at a list in another workspace.
	// The DSL cannot express a two-column foreign key; it can express the index
	// one needs, so half of this lives here and half in the migration.
	UniqueIndex("workspace_id", "id").
	Describe("A named list of tasks within a workspace.").
	// No OpDelete on any table that declares SoftDelete, here or below.
	//
	// schema.SoftDelete adds a deleted_at column and nothing else: the generated
	// DELETE handler issues a real DELETE, and no part of sqlb filters the
	// column back out of reads. A table that declared a soft delete and exposed
	// the generated one would therefore hard-delete through an endpoint whose
	// schema says otherwise, which is worse than not having the feature.
	//
	// So the two halves are supplied here instead: the BeforeQuery hooks in
	// app/hooks.go filter deleted_at, and app/deletes.go serves DELETE as an
	// UPDATE. Both are a few lines, and both are visible.
	Expose(schema.REST{
		Path:            "/lists",
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})

// Task is the table the dynamic data views are built over, and the reason the
// filter grammar exists: filterable by list, assignee, status, priority and due
// date, sortable by most of the same, and searchable over title and
// description.
var Task = schema.Table("tasks",
	schema.UUIDv7("id").PrimaryKey(),
	// ReadOnly, supplied by the BeforeCreate hook; see the note on Membership.
	schema.Ref("workspace", Workspace).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),
	// Expandable in both directions, which are two decisions about two
	// endpoints: ?expand=list on a task pulls in the one list it belongs to,
	// and ?expand=tasks on a list pulls in the tasks that point back at it.
	//
	// The reverse is declared here, on the side that already owns the column
	// and the constraint, and it is capped: a list with two hundred tasks
	// returns twenty and says has_more, and a caller wanting the rest follows
	// /tasks?list_id=eq.<id>, which is the endpoint that already pages and
	// filters. Ordering by position is what the screen wants, and
	// Index("list_id", "position") below is what makes it cheap. ADR-0022.
	schema.Ref("list", List).OnDelete(schema.Cascade).Filterable().Expandable().
		Inverse("tasks").
		InverseExpandable(schema.ExpandOrder("position"), schema.ExpandLimit(20)),

	// Nullable: an unassigned task is the normal state of a new one. The column
	// is typed *string on the model and Col[string] on the typed facade, so
	// "unassigned" is written ?assignee_id=isnull rather than by comparing
	// against a null pointer.
	schema.Ref("assignee", User).OnDelete(schema.SetNull).Nullable().Filterable(),

	// Who filed it. ReadOnly for the same reason workspace_id is: authorship is
	// the token's subject, not something a request gets to assert, so it belongs
	// in neither the create body nor the patch body.
	schema.Ref("author", User).OnDelete(schema.Restrict).ReadOnly(),

	schema.Text("title").Searchable().Sortable(),
	schema.Text("description").Searchable(),

	// The array column, and the one place in this schema where a capability
	// costs an index rather than nothing.
	//
	// Filterable on an array means ?labels=has.urgent, which without a GIN
	// index is a sequential scan that returns the right rows — so nothing
	// reports it and only a plan would show it. schema.Validate refuses the
	// pairing rather than letting that happen, which is why AddIndex below is
	// not optional (ADR-0033).
	//
	// Not Searchable and not Sortable, and neither is an oversight: search is a
	// text operation, and a sortable array would have to be encoded into the
	// keyset cursor, which is wire format.
	// The default is the empty array rather than NULL, and it is what makes
	// adding this column to a populated table a non-destructive migration. It
	// also settles the question a nullable array would keep asking: "no labels"
	// has one spelling here, not two.
	schema.Text("labels").Array().Filterable().
		Default(schema.Value("{}")).
		Comment("Free-form labels. Filter with has, hasany or hasall."),

	schema.Enum("status", "todo", "in_progress", "blocked", "done").
		Default(schema.Value("todo")).
		Filterable().
		Sortable(),
	schema.Enum("priority", "low", "medium", "high", "urgent").
		Default(schema.Value("medium")).
		Filterable().
		Sortable(),

	schema.Timestamp("due_at").Nullable().Filterable().Sortable(),

	// Owned by the BeforeUpdate hook that watches status, not by the client:
	// a request that could set status=done and completed_at=null independently
	// would be able to write a state the check constraint below forbids.
	schema.Timestamp("completed_at").Nullable().ReadOnly().Filterable().Sortable(),

	schema.Int("position").Default(schema.Value(0)).Sortable(),

	// Incremented by AddCommentCount in task_ext.go, never by a client, which
	// is what ReadOnly says: there is no SetCommentCount on the patch body.
	schema.Int("comment_count").Default(schema.Value(0)).Filterable().Sortable().ReadOnly(),

	schema.Timestamps(),
	schema.SoftDelete(),
).
	Index("workspace_id", "status").
	Index("list_id", "position").
	Index("assignee_id").
	Index("due_at").
	// The index Filterable() on an array column obliges. array_ops is the GIN
	// default for a text[], so the opclass gap in the index DSL does not bite
	// here.
	AddIndex(schema.Index{Columns: []string{"labels"}, Method: "gin"}).
	// The other half of a tenant-safe composite reference, for comments. See
	// the note on List.
	UniqueIndex("workspace_id", "id").
	// The invariant the hook maintains, stated where the database can enforce
	// it too. A hook is a convention; a check constraint is a guarantee, and
	// the two disagreeing is exactly what a demo should not hide.
	Check("done_tasks_have_a_completion_time",
		"status <> 'done' OR completed_at IS NOT NULL").
	Describe("A unit of work, belonging to one list.").
	Expose(schema.REST{
		Path:            "/tasks",
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 20,
		MaxPageSize:     100,
		MaxFilters:      12,
	}).
	// The transition PATCH cannot express, and the reason declared actions
	// exist (ADR-0043).
	//
	// Completing a task is not "set status to done". It is a rule — a task
	// already done cannot be completed again — plus two columns that have to
	// move together, because the check constraint above forbids the state where
	// only one of them did. Today a client says it by PATCHing status and
	// trusting a hook to fill completed_at, which works and is invisible: the
	// TypeScript client, the Dart client and the CLI see a status column, not a
	// transition, and every one of them has to be told by hand.
	//
	// Writes names completed_at even though the column is ReadOnly, and the two
	// do not disagree. ReadOnly says no *request* may set it; the envelope
	// writes it from the row the verb mutated, on the server, which is the same
	// standing the BeforeUpdate hook has.
	//
	// Touches is the other half, and this verb is why it exists: a note in the
	// body becomes a comment row through sqlb.TxFrom, and the write set above
	// cannot say so. Two columns on one row is what the envelope persists, not
	// what the route reaches (#149).
	AddAction(schema.Action{
		Name: "complete",
		Body: schema.Body(
			schema.Text("note").Nullable().
				Comment("Recorded as a comment on the task, in the same transaction."),
		),
		Writes:      []string{"status", "completed_at"},
		Touches:     []string{"comments"},
		Description: "Marks the task done and stamps its completion time. A task that is already done is refused with a 409.",
	})

// Comment is a note on a task. It exists mainly to give the demo a second
// write path into the same transaction: creating one bumps the task's
// comment_count, and both must land together or not at all.
var Comment = schema.Table("comments",
	schema.UUIDv7("id").PrimaryKey(),
	// ReadOnly, supplied by the BeforeCreate hook; see the note on Membership.
	schema.Ref("workspace", Workspace).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),
	schema.Ref("task", Task).OnDelete(schema.Cascade).Filterable().Immutable(),
	schema.Ref("author", User).OnDelete(schema.Restrict).Filterable().ReadOnly(),

	schema.Text("body").Searchable(),

	schema.Timestamps(),
	schema.SoftDelete(),
).
	Index("task_id", "created_at").
	Describe("A comment on a task.").
	// OpCreate is exposed, and it was not always.
	//
	// Creating a comment has an invariant attached: the task's comment_count
	// must move with it, and the two writes must land together or not at all.
	// While generated writes ran under autocommit a generated handler could not
	// carry that, so the create was a hand-written endpoint and this table was
	// deliberately not exposed for it — exposing both would have left two ways
	// to create a comment, one of which quietly left the counter wrong.
	//
	// `rest` now wraps a generated write in a transaction, which changes where
	// the invariant can live rather than merely making the old endpoint
	// shorter. A hook receives a context carrying that transaction, so
	// `BeforeCreate` can check the task and `AfterCreate` can move the counter,
	// both inside the unit of work that writes the row. The invariant belongs to
	// the *model* now, not to one route — so every path that creates a comment
	// maintains it, including one written later by someone who has not read
	// this comment. That is a stronger guarantee than the endpoint ever gave,
	// and app/comments.go is gone.
	//
	// Still no OpUpdate: editing a comment is a domain question this example
	// does not answer, not a mechanical one.
	Expose(schema.REST{
		Path:            "/comments",
		Ops:             schema.OpCreate | schema.OpRead | schema.OpList,
		DefaultPageSize: 50,
		MaxPageSize:     100,
	})
