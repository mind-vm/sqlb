package rest

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/mind-vm/sqlb"
)

// PublishChanges makes every write of T announce itself to p.
//
// It registers hooks rather than wrapping the REST handlers, and that is the
// design rather than an implementation detail. Hooks are keyed by type and run
// inside the mutation, so one registration covers the generated CRUD handlers,
// the generated actions, and the application's own sqlb writes alike — the same
// reason a BeforeQuery hook is what scopes reads instead of each handler
// remembering to. A change feed fed only by the REST layer would go quiet for
// exactly the writes most likely to matter: the background job, the migration,
// the admin script.
//
// Wire it once at startup, beside the resources:
//
//	broker := rest.NewBroker(rest.BrokerOptions{})
//	rest.Must(rest.PublishChanges[blog.Post](broker))
//	rest.Must(rest.Events(srv.API, rest.EventsOptions{Source: broker}))
//
// # When the event is published
//
// After the transaction commits, through [sqlb.AfterCommit]. Announcing from
// inside the mutation would publish changes that then rolled back, and a client
// refetching on one of those would see the row unchanged and cache the
// contradiction.
//
// That requires the write to be in a transaction, which generated writes are by
// default ([Options.DisableTransactions] is what turns it off). Under
// autocommit there is no commit left to be after — the statement is already
// durable when the hook runs — so the event is published immediately. The
// distinction is real but not visible to a subscriber.
//
// # Unless p can do better
//
// A p that also implements [TxPublisher] is handed the events *inside* the
// transaction instead, so that the event and the change commit together or
// neither does. That is what an outbox is, and it is why swapping a [Broker] for
// one changes nothing here: this call is the same, and the assertion in
// [TxPublisher] decides which guarantee it gets.
//
// The visible difference is the failure. A Broker cannot fail a write — it is
// told after the commit, when there is nothing left to refuse — while a
// TxPublisher that cannot record the event rolls the mutation back. A row that
// exists while every subscriber believes it does not is the failure this feed
// exists to prevent, reached from the other direction.
//
// # Multi-tenancy
//
// When the model declares a `scope` column ([ADR-0030]), each event carries that
// column's value in [Event.Scope] — off the wire, for
// [EventsOptions.Filter] to compare against the subscriber's tenant. Without it
// a filter has nothing to decide on, because an invalidation names a row and not
// the tenant that owns it.
//
// A soft delete is an UPDATE, so it carries both the key and the scope. So now
// does a hard one; see below.
//
// # What a delete announces
//
// One event per removed row, each carrying that row's key and scope, through
// [sqlb.Hooks.AfterDeleteRows].
//
// This used to be a single keyless event naming the table, because the count
// hook it was registered against could not say which rows went. That was
// tolerable for invalidation — the collection changed either way — and wrong for
// a multi-tenant filter, which had no scope to compare and so had to let the
// event through to every subscriber (#144).
//
// The cost is that a delete of a published model runs `DELETE … RETURNING` and
// scans what it removed, which on a bulk delete is real. It is not avoidable
// while the event names a row, and a feed that goes vague on exactly the
// operation a client most needs to hear about is not worth the saving.
//
// # Which registry
//
// r is the registry the announcing hooks are registered into, and it must be
// the one the handle doing the writing resolves against — the registry passed
// to [sqlb.DB.WithHooks]. Publishing into a registry no handle carries
// registers hooks nothing will ever run, which looks exactly like a working
// invalidation feed that never emits.
//
// This used to default to a process-wide registry, with the registry-taking
// form under a longer name. Removing that default was [ADR-0047].
//
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required
// [ADR-0047]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#no-default-hook-registry
func PublishChanges[T any](r *sqlb.Registry, p Publisher) error {
	if r == nil {
		return errors.New("rest: PublishChanges needs a registry")
	}
	return publishChanges(sqlb.On[T](r), p)
}

func publishChanges[T any](h *sqlb.Hooks[T], p Publisher) error {
	if p == nil {
		return errors.New("rest: PublishChanges needs a Publisher")
	}
	m := sqlb.ModelOf[T]()
	if m.Table == "" {
		return fmt.Errorf("rest: %s has no table name to publish under", m.Type)
	}
	table := m.Table
	pk := m.PK
	// The column the schema declared `scope`, or nil. Reading it here rather
	// than per event keeps ADR-0030's obligation on this path too: the model
	// already says which column confines its rows, so the feed can say which
	// tenant an event belongs to without the endpoint knowing what a tenant is.
	scope := m.Scope

	h.AfterCreate(func(ctx context.Context, row *T) error {
		return announce(ctx, p, Event{
			Table: table,
			Key:   keyOf(pk, row),
			Op:    Created,
			Scope: keyOf(scope, row),
		})
	})

	h.AfterUpdate(func(ctx context.Context, rows []T) error {
		events := make([]Event, len(rows))
		for i := range rows {
			events[i] = Event{
				Table: table,
				Key:   keyOf(pk, &rows[i]),
				Op:    Updated,
				Scope: keyOf(scope, &rows[i]),
			}
		}
		return announce(ctx, p, events...)
	})

	// AfterDeleteRows rather than AfterDelete, which is what makes a delete event
	// carry the key and the scope. The count form cannot: it says how many rows
	// went and not which, so the event named a table and nothing else, and a
	// subscriber keyed on the row had nothing to invalidate but the whole
	// collection (#144). Registering this is also what makes the delete run
	// RETURNING, which is the cost — paid here because a feed whose delete events
	// name no row is the half of the feed that does not work.
	h.AfterDeleteRows(func(ctx context.Context, rows []T) error {
		// A delete that matched nothing changed nothing. Announcing it would
		// have every subscriber refetch a collection that is identical.
		if len(rows) == 0 {
			return nil
		}
		events := make([]Event, len(rows))
		for i := range rows {
			events[i] = Event{
				Table: table,
				Key:   keyOf(pk, &rows[i]),
				Op:    Deleted,
				Scope: keyOf(scope, &rows[i]),
			}
		}
		return announce(ctx, p, events...)
	})

	return nil
}

// announce hands the events to the publisher at the moment that publisher can
// use, which is one of three and not two.
//
// A [TxPublisher] inside a transaction records *into* it, so the event and the
// change commit together or neither does — the durability [ADR-0012] is about.
// Its error is returned rather than reported, which rolls the write back: a
// change that could not be recorded is one no subscriber would ever hear about,
// and a row that exists while every client believes it does not is worse than a
// write that failed and said so.
//
// Everything else publishes after the commit when there is one to be after, and
// immediately when there is not. That fallback is not a silent downgrade: under
// autocommit the statement committed before the hook was called, so publishing
// now *is* publishing after the commit. What it loses is atomicity across a
// multi-statement unit of work, which under autocommit does not exist to lose —
// and a TxPublisher lands here for the same reason, having no transaction to
// record into.
//
// [ADR-0012]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#change-feed-outbox
func announce(ctx context.Context, p Publisher, events ...Event) error {
	if len(events) == 0 {
		return nil
	}
	if _, inTx := sqlb.TxFrom(ctx); !inTx {
		p.Publish(events...)
		return nil
	}
	if tp, ok := p.(TxPublisher); ok {
		return tp.Record(ctx, events...)
	}
	return sqlb.AfterCommit(ctx, func(context.Context) error {
		p.Publish(events...)
		return nil
	})
}

// keyOf renders one of a row's columns the way a URL renders it, so that a
// subscriber can concatenate the primary key onto the resource path to refetch.
// A nil column — no primary key, or no declared scope — reads as empty.
func keyOf[T any](col *sqlb.ColumnInfo, row *T) string {
	if col == nil || row == nil {
		return ""
	}
	field, ok := fieldAt(reflect.ValueOf(row).Elem(), col.Index)
	if !ok {
		return ""
	}
	return keyString(field)
}

func keyString(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.CanInterface() {
		return ""
	}
	value := v.Interface()
	// A []byte key is bytes of text, not a number list: fmt would render it as
	// "[104 101 108 108 111]" and the client would ask for a row by that.
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	// Everything else goes through fmt, which reaches String() on the uuid and
	// time types a key is otherwise likely to be.
	return fmt.Sprint(value)
}
