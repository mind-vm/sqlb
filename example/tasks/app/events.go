package app

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/rest"
)

// The change feed.
//
// A workspace's clients hold `GET /events` open and are told when something they
// display has changed. They are told the *address* of the change — table and row
// id — and refetch through the endpoints they already use, so every predicate
// hooks.go adds still applies to what they end up seeing. Nothing here reads a
// row, which is why nothing here has to re-implement the workspace boundary.
//
// # The boundary, on this path
//
// It is not free. An invalidation is published by a write rather than read
// through a query, so BeforeQuery — the hook that confines everything else in
// this application — never runs on it. Without a filter, every subscriber would
// receive every workspace's events: not their contents, but their row ids and
// their timing, which is more than a tenant should learn about another.
//
// eventsFilter is what closes that. It compares the event's scope against the
// caller's workspace, and the event has a scope to compare because the schema
// declared one: `workspace_id` is `.Scoped()`, so rest.PublishChanges reads it
// off the changed row and carries it (ADR-0030 again, on a third path).
//
// # Why deletes are not in the feed's blind spot here
//
// A hard DELETE used to publish a table and no key, because sqlb's AfterDelete
// hook is handed a count — which also meant no scope, and an event this filter
// could not attribute. It never arose for the three models below: their deletes
// are soft, and a soft delete is an UPDATE (see deletes.go), so it carries the
// key and the workspace like any other change.
//
// It no longer arises anywhere. rest.PublishChanges registers
// sqlb.Hooks.AfterDeleteRows, so a hard delete returns the rows it removed and
// publishes one attributable event each (#144). What that costs is a scan of
// what the statement matched, on every delete of a published model.
//
// Memberships are the one hard delete in this application, and they stay
// unpublished — but for a smaller reason than before. The old one was that the
// event could not be attributed to a workspace at all; the remaining one is that
// nothing in the client displays a membership list, so the fan-out would buy
// nothing. Publishing them would now be correct, just not useful.

// publishChanges wires the models whose changes a client displays.
//
// Users and Workspaces are left out for the same reason they expose only reads:
// nothing in this application changes them often enough for a client to be
// watching, and every model added here is a fan-out every subscriber pays for.
func publishChanges(reg *sqlb.Registry, broker *rest.Broker) error {
	for _, wire := range []func(*sqlb.Registry, rest.Publisher) error{
		rest.PublishChanges[tasks.Task],
		rest.PublishChanges[tasks.List],
		rest.PublishChanges[tasks.Comment],
	} {
		if err := wire(reg, broker); err != nil {
			return err
		}
	}
	return nil
}

// registerEvents mounts the stream.
//
// The route is authenticated by the same middleware as everything else — it is
// not in auth.Middleware's exception list — so the context this filter reads has
// been through the token check before any event is considered.
func registerEvents(api huma.API, broker *rest.Broker) error {
	return rest.Events(api, rest.EventsOptions{
		Source:  broker,
		Tag:     "events",
		Summary: "Subscribe to changes in this workspace",
		Description: "A Server-Sent Events stream of invalidations for the caller's workspace. " +
			"Each `change` event names a table and a row id; refetch through that resource's " +
			"own endpoint, which is what keeps the workspace boundary on the data itself. " +
			"A `reset` event means the stream could not be resumed — refetch everything.\n\n" +
			"Events are held in memory by the process serving the stream, so this demo is " +
			"single-replica and at-most-once. See ADR-0012 for the durable version.",
		Filter: eventsFilter,
	})
}

// eventsFilter answers "is this event mine" for one subscriber.
//
// It fails closed. A context with no claims should be unreachable — the
// middleware rejects the request before the stream opens — but if it ever is
// reached, the answer is no events rather than all of them. That is the same
// stance hooks.go takes, and for the same reason: the unauthenticated path is
// the one nobody exercises.
//
// An event with no scope is also refused here. In this application there is no
// such event, because the published models all declare `workspace_id` and their
// deletes are soft — so this branch is a guard against a fourth model being
// added later whose changes cannot be attributed, not a case that fires today.
func eventsFilter(ctx context.Context, e rest.Event) bool {
	workspace, err := workspaceOf(ctx)
	if err != nil {
		return false
	}
	return e.Scope != "" && e.Scope == workspace
}
