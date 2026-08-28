package rest_test

import (
	"context"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// The composition claim behind the emitted scoping hooks (#274).
//
// codegen writes exactly the four registrations below, under a releasable name
// for the three predicates and unreleasable for the stamp. What that file is
// worth depends on a claim its own text cannot make: that registering them
// discharges the obligation rest.Resource checks, so a scoped resource mounts
// once RegisterScopes has been called and refuses until it has.
//
// Written here by hand, in the shape the emitter produces, because the
// generated file lives in a consumer's package and cannot be imported back.
// codegen/scopes_test.go asserts the emitter produces this shape; this asserts
// the shape is worth producing.

// registerScopesShape is what the emitted RegisterScopes does for one table.
func registerScopesShape(reg *sqlb.Registry, name string, resolve func(context.Context) (string, error)) {
	sqlb.On[Scoped](reg).Scope(name).
		BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Scoped]) error {
			v, err := resolve(ctx)
			if err != nil {
				return err
			}
			q.Where(sqlb.F("org_id").Eq(v))
			return nil
		}).
		BeforeUpdate(func(ctx context.Context, u *sqlb.Update[Scoped]) error {
			v, err := resolve(ctx)
			if err != nil {
				return err
			}
			u.Where(sqlb.F("org_id").Eq(v))
			return nil
		}).
		BeforeDelete(func(ctx context.Context, d *sqlb.Delete[Scoped]) error {
			v, err := resolve(ctx)
			if err != nil {
				return err
			}
			d.Where(sqlb.F("org_id").Eq(v))
			return nil
		})
	sqlb.On[Scoped](reg).BeforeCreate(func(ctx context.Context, row *Scoped) error {
		v, err := resolve(ctx)
		if err != nil {
			return err
		}
		row.OrgID = v
		return nil
	})
}

func TestTheEmittedScopeShapeDischargesTheMountObligation(t *testing.T) {
	reg := sqlb.NewRegistry()
	registerScopesShape(reg, "tenant", func(context.Context) (string, error) { return "acme", nil })
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Scoped, scopedCreate, rest.None[Scoped]](api, db, rest.Options{
		Path: "/scoped", Name: "scoped", Ops: rest.CRUD | rest.OpList,
	}); err != nil {
		t.Fatalf("the emitted shape should discharge every obligation a Scoped column declares: %v", err)
	}
}

// The other direction, which is what makes the assertion above mean anything:
// without the registration the same mount is refused. If it mounted either way
// the generated file would be decorative.
func TestWithoutTheScopeShapeTheSameMountIsRefused(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Scoped, scopedCreate, rest.None[Scoped]](api, db, rest.Options{
		Path: "/scoped", Name: "scoped", Ops: rest.CRUD | rest.OpList,
	})
	if err == nil {
		t.Fatal("a scoped resource mounted with nothing confining it")
	}
}

// Releasing the name releases the three predicates and not the stamp — the
// asymmetry the emitted file is written around. A released read shows one more
// row; a released stamp writes a row belonging to nobody, so a create on an
// admin handle must still be refused for want of a tenant rather than silently
// writing an empty one.
func TestReleasingTheScopeKeepsTheCreateStamp(t *testing.T) {
	reg := sqlb.NewRegistry()
	var stamped string
	registerScopesShape(reg, "tenant", func(context.Context) (string, error) { return "acme", nil })
	sqlb.On[Scoped](reg).BeforeCreate(func(_ context.Context, row *Scoped) error {
		stamped = row.OrgID
		return nil
	})

	fake := newFakeDB(t, reply{
		cols: []string{"id", "org_id", "title", "deleted_at"},
		rows: [][]any{{"s1", "acme", "hello", nil}},
	})
	admin := sqlb.New(fake.db).WithHooks(reg).WithoutScope("tenant")

	row := &Scoped{Title: "hello"}
	if _, err := sqlb.InsertRows(row).Exec(context.Background(), admin); err != nil {
		t.Fatalf("Insert on a released handle: %v", err)
	}
	if stamped != "acme" {
		t.Errorf("the create stamp was released along with the read predicates; stamped %q", stamped)
	}
}
