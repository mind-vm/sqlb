package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// The workspace boundary.
//
// This file is the reason the example exists. Everything a request may read or
// write is confined to one workspace, and the confinement is expressed once per
// model here rather than once per handler, once per query, and once per
// background job — which is the arrangement that eventually leaks, because it
// only takes one call site written on a Friday.
//
// It works because BeforeQuery is handed the *query*. A hook that could only
// veto would have to be given the rows, which means fetching them first; a hook
// that is given the builder can add a predicate, and the database never sees
// the other workspace's rows at all. That is also why the generated REST
// handlers need to know nothing: they call sqlb.Query[T], and so does everyone
// else.
//
// # Fail closed
//
// The schema says the same thing from the other side. Every model below
// declares Scoped on the column this file constrains, which means none of
// these resources mount until the registrations their operations need are
// here — so the file that would otherwise have to be remembered is the one the
// server refuses to start without (ADR-0030). Nothing in it changed to satisfy
// that; the check was shaped around what was already written.
//
// Every hook here returns an error when the context carries no claims. It never
// falls back to "no restriction", which is the shape of most tenancy bugs: the
// unauthenticated path is the one nobody tests, and an unscoped query there
// returns every tenant's rows with a 200 next to it. Refusing means the worst
// case is a broken endpoint, which is noticed.

// Register installs the hooks into a registry and returns it.
//
// A registry is the only way to register — there is no process-wide default
// (ADR-0047) — and this example is part of why. Two servers in one test binary
// sharing registrations meant the second one's Register stacked a duplicate
// set of predicates onto the first, so naming a registry was already the right
// answer here before it became the only one. Handing it to the handle with
// WithHooks keeps each server's rules its own.
func Register(log *slog.Logger) *sqlb.Registry {
	reg := sqlb.NewRegistry()

	// Reads. Every workspace-scoped model gets the same treatment, and the
	// models with a deleted_at get the soft-delete predicate too.
	//
	// Nothing in sqlb applies a soft-delete filter on its own — schema.SoftDelete
	// adds the column and stops there — so a model that declares one and does
	// not filter it returns deleted rows from every list endpoint. Doing it here
	// means it is done once, for reads issued by anyone.
	//
	// The workspace predicate is registered under the name "tenant" — see
	// [Hooks.Scope] — and the soft-delete predicate deliberately is not: they
	// are two different questions ("which tenant" and "which lifecycle state"),
	// and app/admin.go releases only the first. A platform-admin token sees
	// every workspace's rows; it does not, by that alone, see rows a user
	// deleted. scopeWrites has the same split for BeforeUpdate/BeforeDelete.
	scopeReads[tasks.List](reg, softDeleted)
	scopeReads[tasks.Task](reg, softDeleted)
	scopeReads[tasks.Comment](reg, softDeleted)
	scopeReads[tasks.Membership](reg, hardDeleted)

	// Workspaces are scoped by identity rather than by a workspace_id column:
	// the row *is* the tenant. Without this, GET /workspaces would list every
	// tenant in the installation, which is the kind of hole that a schema-level
	// convention silently leaves behind when one table does not follow it.
	sqlb.On[tasks.Workspace](reg).Scope("tenant").BeforeQuery(func(ctx context.Context, q *sqlb.Builder[tasks.Workspace]) error {
		workspace, err := workspaceOf(ctx)
		if err != nil {
			return err
		}
		q.Where(sqlb.F("id").Eq(workspace))
		return nil
	})
	// Workspace.id is Scoped in the schema (taskschema/schema.go) and this was
	// the only BeforeQuery hook confining it; releasing "tenant" for the admin
	// mount would leave rest.Resource nothing to find and it would refuse to
	// mount ADR-0030's obligation check "asks that a hook exists, not that it
	// does anything" (rest/scope.go) — which is exactly what this satisfies,
	// permanently and un-releasably, once for the model rather than at every
	// mount that wants to see every workspace.
	noopQuery[tasks.Workspace](reg)

	// Users are global — one account reaches every workspace it is a member of
	// — so "which users may I see" is a question about memberships, and the
	// predicate has to reach another table.
	//
	// RawPred is the escape hatch for what the builder cannot model, and this is
	// what it is for. The identifiers are literals in this file rather than
	// anything derived from a request, and the one value is a bind parameter, so
	// the usual objection to hand-written SQL does not apply. It is the honest
	// version of the alternative — fetching the member ids first and passing
	// them to In() — which is the same query split in two with a race in the
	// middle.
	//
	// "users"."id" is qualified rather than bare "id", for the reason
	// TestExpandDoesNotMakeTheOtherParametersAmbiguous (app/expand_test.go)
	// documents for every other request-supplied parameter: GET
	// /users?expand=profile joins in profiles, which has an "id" column of its
	// own, and Postgres refuses a bare "id" the moment a second table in the
	// statement has one too (SQLSTATE 42702). This predicate is hand-written
	// SQL, so it does not get that qualification for free the way the
	// builder's own columns do — it has to say so itself.
	sqlb.On[tasks.User](reg).Scope("tenant").BeforeQuery(func(ctx context.Context, q *sqlb.Builder[tasks.User]) error {
		workspace, err := workspaceOf(ctx)
		if err != nil {
			return err
		}
		q.Where(sqlb.RawPred(
			`"users"."id" IN (SELECT "user_id" FROM "memberships" WHERE "workspace_id" = ?)`,
			workspace))
		return nil
	})
	// Same reason as Workspace's: User.id is Scoped and this was its only
	// confining hook.
	noopQuery[tasks.User](reg)

	// Writes. An UPDATE or DELETE gets the same workspace predicate, which is a
	// separate registration because it is a separate statement: a BeforeQuery
	// hook constrains what a request can *see*, and says nothing about what it
	// can overwrite by id.
	scopeWrites[tasks.List](reg)
	scopeWrites[tasks.Task](reg)
	scopeWrites[tasks.Comment](reg)
	scopeWrites[tasks.Membership](reg)
	// app/admin.go exposes Update for lists and tasks, and Delete for
	// memberships — the same "tenant" release, same reason noopQuery exists
	// for the read side.
	noopUpdate[tasks.List](reg)
	noopUpdate[tasks.Task](reg)
	noopDelete[tasks.Membership](reg)

	// Creates. The columns a client is not allowed to assert are stamped from
	// the token. They are ReadOnly in the schema, so they are absent from the
	// generated request bodies — the hook is not overriding a value the caller
	// sent, it is supplying the only value there is.
	sqlb.On[tasks.List](reg).BeforeCreate(func(ctx context.Context, l *tasks.List) error {
		c, err := claimsOrError(ctx)
		if err != nil {
			return err
		}
		l.WorkspaceID = c.Workspace
		return nil
	})

	sqlb.On[tasks.Task](reg).BeforeCreate(func(ctx context.Context, t *tasks.Task) error {
		c, err := claimsOrError(ctx)
		if err != nil {
			return err
		}
		t.WorkspaceID = c.Workspace
		t.AuthorID = c.Subject
		return nil
	})

	// Comments carry an invariant the other models do not: the task's
	// comment_count has to move with them. Both halves live here, in hooks, and
	// that is only possible because rest wraps a generated write in a
	// transaction — so the context a hook receives carries one, and TxFrom finds
	// it. Before that change this had to be a hand-written endpoint.
	comments := sqlb.On[tasks.Comment](reg)

	comments.BeforeCreate(func(ctx context.Context, cm *tasks.Comment) error {
		c, err := claimsOrError(ctx)
		if err != nil {
			return err
		}
		cm.WorkspaceID = c.Workspace
		cm.AuthorID = c.Subject

		// Check the task exists in this workspace, rather than letting the
		// composite foreign key catch it.
		//
		// Both refuse the write; they differ in what the caller is told. The
		// constraint raises a Postgres error rest cannot classify, so a client
		// naming a task in another workspace would get a 500 for what is
		// squarely its own mistake. Reading first turns that into the 404 it
		// should have been — the same answer an id that never existed gets.
		//
		// The read runs on the transaction, so it sees the same snapshot the
		// insert will, and the BeforeQuery hook scopes it to the workspace.
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			// Fail closed. Without a transaction the counter below cannot move
			// atomically with the insert, and a comment that silently does not
			// count is worse than one that is refused.
			return fmt.Errorf("app: a comment must be created inside a transaction")
		}
		switch _, err := sqlb.Query[tasks.Task]().
			Where(tasks.TaskCols.ID.Eq(cm.TaskID)).
			One(ctx, tx); {
		case errors.Is(err, sqlb.ErrNotFound):
			return huma.Error404NotFound("no task matched")
		case err != nil:
			return fmt.Errorf("reading the task: %w", err)
		}
		return nil
	})

	comments.AfterCreate(func(ctx context.Context, cm *tasks.Comment) error {
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return fmt.Errorf("app: a comment must be created inside a transaction")
		}

		// comment_count + 1 computed by the database, not read into Go and
		// written back: two comments posted in the same second both count.
		if _, err := tasks.UpdateTask().
			AddCommentCount(1).
			Where(tasks.TaskCols.ID.Eq(cm.TaskID)).
			Stmt().
			Exec(ctx, tx); err != nil {
			return fmt.Errorf("bumping the comment count: %w", err)
		}

		// The side effect goes after the commit. Announcing it from here would
		// announce a comment a later rollback un-writes; doing it in the handler
		// would announce one that was never committed.
		//
		// At-most-once and in-process: if the machine dies between the commit
		// and the callback, nothing records that the callback was owed. That is
		// AfterCommit's documented limit and the reason a durable change feed
		// wants an outbox row written in this same transaction instead.
		id, task, workspace := cm.ID, cm.TaskID, cm.WorkspaceID
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			log.Info("comment posted",
				"comment_id", id, "task_id", task, "workspace_id", workspace)
			return nil
		})
	})

	// profiles has no workspace_id and no BeforeQuery "tenant" hook — see the
	// note on taskschema.Profile for why: it is 1:1 with the global User, and
	// the membership-subquery answer User's own hook uses cannot be reused
	// here, because that hook is RawPred and a RawPred BeforeQuery hook on an
	// expansion target cannot be requalified onto a join alias
	// (sqlb/qualify.go) — registering it on Profile would make every GET
	// /users?expand=profile fail at request time.
	//
	// So the write side is checked row by row instead of by a query-level
	// predicate: tasks.User is already correctly scoped by its own "tenant"
	// hook, so a user this query cannot find is either nonexistent or not
	// this caller's to see, and either way the same 404 is the right and
	// honest answer — the same choice comments.BeforeCreate makes above for a
	// task another workspace's caller named. Without this, POST /profiles
	// would let any authenticated caller plant a profile for a user_id
	// belonging to a workspace they are not a member of.
	sqlb.On[tasks.Profile](reg).BeforeCreate(func(ctx context.Context, p *tasks.Profile) error {
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return fmt.Errorf("app: a profile must be created inside a transaction")
		}
		switch _, err := sqlb.Query[tasks.User]().
			Where(tasks.UserCols.ID.Eq(p.UserID)).
			One(ctx, tx); {
		case errors.Is(err, sqlb.ErrNotFound):
			return huma.Error404NotFound("no user matched")
		case err != nil:
			return fmt.Errorf("reading the user: %w", err)
		}
		return nil
	})

	sqlb.On[tasks.Membership](reg).BeforeCreate(func(ctx context.Context, m *tasks.Membership) error {
		c, err := claimsOrError(ctx)
		if err != nil {
			return err
		}
		// Inviting somebody is an administrative act, so this is the one place
		// the role in the token is read for a decision rather than carried
		// along. A member may list the workspace's memberships and may not add
		// to them.
		if !c.AtLeast(auth.RoleAdmin) {
			return errForbidden("adding a member needs the admin role")
		}
		m.WorkspaceID = c.Workspace
		return nil
	})

	return reg
}

// softDeleted and hardDeleted name the two shapes scopeReads handles, because
// scopeReads(reg, true) at the call site says nothing about what is true.
const (
	softDeleted = true
	hardDeleted = false
)

// scopeReads confines every SELECT against T to the caller's workspace, under
// the releasable name "tenant" (see the note above this function's call
// site). soft additionally filters deleted_at, as a second, permanently
// unreleased hook — a platform-admin token releases "tenant" and still does
// not see a row a user deleted.
//
// It is generic over the model because the predicate is the same for all of
// them — the column is called workspace_id everywhere, which is a convention
// this schema keeps deliberately so that the boundary can be one function
// instead of four near-copies.
func scopeReads[T any](reg *sqlb.Registry, soft bool) {
	sqlb.On[T](reg).Scope("tenant").BeforeQuery(func(ctx context.Context, q *sqlb.Builder[T]) error {
		workspace, err := workspaceOf(ctx)
		if err != nil {
			return err
		}
		q.Where(sqlb.F("workspace_id").Eq(workspace))
		return nil
	})
	if soft {
		sqlb.On[T](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[T]) error {
			q.Where(sqlb.F("deleted_at").IsNull())
			return nil
		})
	} else {
		// No soft-delete hook to fall back on once "tenant" is released — see
		// noopQuery's doc comment.
		noopQuery[T](reg)
	}
}

// noopQuery, noopUpdate and noopDelete register a hook that does nothing, so
// that a model whose only confining hook is named "tenant" still has *a*
// BeforeQuery/BeforeUpdate/BeforeDelete hook once app/admin.go releases that
// name — rest.Resource's obligation check (ADR-0030) asks only that a hook
// exists, not that it restricts anything ("A BeforeQuery hook that logs and
// returns nil satisfies it," rest/scope.go) — and without one of these, the
// admin mount for that model and operation would refuse to start rather than
// serve every workspace's rows, which is the point of mounting it.
func noopQuery[T any](reg *sqlb.Registry) {
	sqlb.On[T](reg).BeforeQuery(func(context.Context, *sqlb.Builder[T]) error { return nil })
}

func noopUpdate[T any](reg *sqlb.Registry) {
	sqlb.On[T](reg).BeforeUpdate(func(context.Context, *sqlb.Update[T]) error { return nil })
}

func noopDelete[T any](reg *sqlb.Registry) {
	sqlb.On[T](reg).BeforeDelete(func(context.Context, *sqlb.Delete[T]) error { return nil })
}

// scopeWrites confines every UPDATE and DELETE against T to the caller's
// workspace, under the releasable name "tenant".
//
// The predicate is added rather than checked, so a PATCH naming a task id in
// another workspace matches no rows and comes back 404 — the same answer the
// caller would get for an id that does not exist, which is the answer they
// should get. A check would have to read the row first, and would then have to
// decide between 403 and 404; adding the predicate makes the question not
// arise.
//
// This also satisfies sqlb's guard against unscoped mutations: an UPDATE with
// no WHERE is refused rather than executed, so a handler that forgets its own
// predicate is stopped by this one rather than rewriting the table.
func scopeWrites[T any](reg *sqlb.Registry) {
	hooks := sqlb.On[T](reg).Scope("tenant")

	hooks.BeforeUpdate(func(ctx context.Context, u *sqlb.Update[T]) error {
		workspace, err := workspaceOf(ctx)
		if err != nil {
			return err
		}
		u.Where(sqlb.F("workspace_id").Eq(workspace))
		return nil
	})

	hooks.BeforeDelete(func(ctx context.Context, d *sqlb.Delete[T]) error {
		workspace, err := workspaceOf(ctx)
		if err != nil {
			return err
		}
		d.Where(sqlb.F("workspace_id").Eq(workspace))
		return nil
	})
}
