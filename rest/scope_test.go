package rest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// Scoped is what codegen emits for a table whose tenant column declared
// Scoped: the column is ReadOnly, so it is absent from the create body, and
// carries the marker the mount-time check reads.
type Scoped struct {
	ID        string     `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	OrgID     string     `db:"org_id" json:"org_id" sqlb:"filter,readonly,scope"`
	Title     string     `db:"title" json:"title" sqlb:"filter,sort"`
	DeletedAt *time.Time `db:"deleted_at" json:"deleted_at" sqlb:"readonly,softdelete"`
}

func (Scoped) TableName() string { return "scoped" }

type scopedCreate struct {
	Title string `json:"title"`
}

func (c scopedCreate) Row() (*Scoped, error) { return &Scoped{Title: c.Title}, nil }

type scopedUpdate struct {
	Title *string `json:"title,omitempty"`
}

func (u scopedUpdate) Changes() (map[string]any, error) {
	out := map[string]any{}
	if u.Title != nil {
		out["title"] = *u.Title
	}
	return out, nil
}

func scopedOptions() rest.Options {
	return rest.Options{
		Path: "/scoped",
		Name: "scoped",
		Ops:  rest.CRUD | rest.OpList,
	}
}

// mountScoped attempts the mount and returns whatever error it produced.
func mountScoped(t *testing.T, db sqlb.Executor, opts rest.Options) error {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	return rest.Resource[Scoped, scopedCreate, scopedUpdate](api, db, opts)
}

// The case the whole check exists for: the table says its rows are confined,
// nobody wrote the hook, and without this the resource would answer 200 with
// every tenant's rows in it.
func TestResourceRefusesAScopedModelWithNoHooks(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	err := mountScoped(t, db, scopedOptions())
	if err == nil {
		t.Fatal("expected mounting to fail: nothing confines a model whose schema says it is confined")
	}

	// Every unmet obligation in one message, each naming the hook that would
	// satisfy it and the declaration that asked.
	for _, want := range []string{
		"BeforeQuery", "BeforeCreate", "BeforeUpdate", "BeforeDelete",
		"org_id is Scoped", "deleted_at declares a soft delete",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}
}

// Registering everything the operations require is what makes the resource
// mountable, and nothing about the hooks' contents is inspected.
func TestResourceAcceptsAScopedModelWithHooks(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).
		BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil }).
		BeforeCreate(func(context.Context, *Scoped) error { return nil }).
		BeforeUpdate(func(context.Context, *sqlb.Update[Scoped]) error { return nil }).
		BeforeDelete(func(context.Context, *sqlb.Delete[Scoped]) error { return nil })

	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)
	if err := mountScoped(t, db, scopedOptions()); err != nil {
		t.Fatalf("mounting a resource whose hooks are all registered: %v", err)
	}
}

// The obligation follows the operations, so a read-only resource needs one
// registration rather than four. This is the case that would otherwise push
// people towards registering empty hooks to get past the check.
func TestScopeObligationsFollowTheExposedOperations(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).
		BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil })
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	opts := scopedOptions()
	opts.Ops = rest.OpList | rest.OpRead
	if err := mountScoped(t, db, opts); err != nil {
		t.Fatalf("mounting a read-only resource with a read hook: %v", err)
	}

	// Adding update to the same resource asks a question BeforeQuery does not
	// answer: it constrains what a request can see, not what it can overwrite
	// by id.
	opts.Ops = rest.OpList | rest.OpRead | rest.OpUpdate
	err := mountScoped(t, db, opts)
	if err == nil {
		t.Fatal("expected mounting to fail: an exposed update has no hook narrowing it")
	}
	if !strings.Contains(err.Error(), "BeforeUpdate") {
		t.Errorf("error = %v, want it to name BeforeUpdate", err)
	}
	if strings.Contains(err.Error(), "BeforeQuery") {
		t.Errorf("error = %v, want it to leave the satisfied obligation out", err)
	}
}

// A model declaring neither obligation is unaffected, which is what keeps this
// from being a tax on the schemas that never claimed to be multi-tenant.
func TestUndeclaredModelsMountWithoutHooks(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	if err := rest.Resource[Post, PostCreate, PostUpdate](api, db, postOptions()); err != nil {
		t.Fatalf("mounting an undeclared model: %v", err)
	}
}

// The registry the handle carries is the one that is asked. Two handles over
// the same database, differing only in their registry, must get different
// answers — otherwise a program that scopes one handle would be told its hooks
// are missing while they run on every query.
func TestScopeCheckReadsTheHandlesRegistry(t *testing.T) {
	confined := sqlb.NewRegistry()
	sqlb.On[Scoped](confined).BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil })

	opts := scopedOptions()
	opts.Ops = rest.OpList

	if err := mountScoped(t, sqlb.New(newFakeDB(t).db).WithHooks(confined), opts); err != nil {
		t.Fatalf("mounting against the registry holding the hook: %v", err)
	}
	// The same model, the same options, a handle whose registry is empty.
	bare := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())
	if err := mountScoped(t, bare, opts); err == nil {
		t.Fatal("expected mounting to fail: the handle's own registry is empty")
	}
}

// --- released scopes (#177) -------------------------------------------------

// allFour registers every hook a fully-exposed Scoped model requires, under
// name if one is given and unnamed otherwise.
func allFour(reg *sqlb.Registry, name string) {
	q := func(_ context.Context, b *sqlb.Builder[Scoped]) error {
		b.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	}
	u := func(_ context.Context, s *sqlb.Update[Scoped]) error { return nil }
	d := func(_ context.Context, s *sqlb.Delete[Scoped]) error { return nil }
	c := func(_ context.Context, row *Scoped) error { row.OrgID = "org1"; return nil }

	sqlb.On[Scoped](reg).BeforeCreate(c)
	if name == "" {
		sqlb.On[Scoped](reg).BeforeQuery(q).BeforeUpdate(u).BeforeDelete(d)
		return
	}
	sqlb.On[Scoped](reg).Scope(name).BeforeQuery(q).BeforeUpdate(u).BeforeDelete(d)
}

// The property that makes Unscoped safe to offer at all, and the one ADR-0030
// was protecting when it declined to add an escape hatch: releasing every rule
// that confines a Scoped model does not get the resource mounted. The check is
// the same one, asked of the handle the resource will actually serve from.
func TestAResourceCannotReleaseItsWayPastTheObligationCheck(t *testing.T) {
	reg := sqlb.NewRegistry()
	allFour(reg, "storefront")
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	// Proven both ways (ADR-0016): without the release the same mount succeeds,
	// so the refusal below is the release and not a model that never satisfied
	// the check.
	if err := mountScoped(t, db, scopedOptions()); err != nil {
		t.Fatalf("the mount fails before any release, so this proves nothing: %v", err)
	}

	// One obligation at a time. A resource exposing all of CRUD has four of
	// them, and asserting only that the mount failed is satisfied by any one —
	// so a BeforeQuery that had quietly stopped being release-aware would still
	// leave this green on BeforeUpdate's refusal. That is not hypothetical: the
	// first version of this test was written that way, and it passed with the
	// release deliberately hidden from the check. Each case now exposes exactly
	// the operation that requires the hook it is about.
	for _, tc := range []struct {
		name string
		ops  rest.Op
		hook string
	}{
		{"read", rest.OpList | rest.OpRead, "BeforeQuery"},
		{"update", rest.OpUpdate, "BeforeUpdate"},
		{"delete", rest.OpDelete, "BeforeDelete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := scopedOptions()
			opts.Ops = tc.ops
			opts.Unscoped = []string{"storefront"}

			err := mountScoped(t, db, opts)
			if err == nil {
				t.Fatal("a resource released every rule confining a Scoped model and still mounted")
			}
			for _, want := range []string{tc.hook, "org_id is Scoped", `"storefront"`, "releases"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %s:\n%s", want, err)
				}
			}
		})
	}
}

// Releasing one of two leaves the other confining the model, so the mount is
// allowed. Without this the check would be "any release refuses", which is a
// feature nobody could use.
func TestReleasingOneOfTwoRulesStillMounts(t *testing.T) {
	reg := sqlb.NewRegistry()
	allFour(reg, "storefront")
	allFour(reg, "") // a second, unnamed, absolute rule
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	opts := scopedOptions()
	opts.Unscoped = []string{"storefront"}
	if err := mountScoped(t, db, opts); err != nil {
		t.Errorf("releasing one of two confining rules refused the mount: %v", err)
	}
}

// A name nothing registered is a typo, and the failure it would otherwise cause
// is the quiet one: a mount that reads as narrowed and serves the wide rule.
func TestAnUnknownReleasedScopeRefusesToMount(t *testing.T) {
	reg := sqlb.NewRegistry()
	allFour(reg, "storefront")
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	opts := scopedOptions()
	opts.Unscoped = []string{"storfront"} // the typo
	err := mountScoped(t, db, opts)
	if err == nil {
		t.Fatal("a release naming a scope nothing registered was accepted")
	}
	// The rejection names what would have been accepted (ADR-0011).
	for _, want := range []string{`"storfront"`, `"storefront"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%s", want, err)
		}
	}
}

// Unscoped over an executor that carries no registry is a promise nothing
// keeps: there is no rule to release and no name to check.
func TestUnscopedOverAHandlelessExecutorRefusesToMount(t *testing.T) {
	reg := sqlb.NewRegistry()
	allFour(reg, "storefront")

	opts := scopedOptions()
	opts.Unscoped = []string{"storefront"}
	err := mountScoped(t, newFakeDB(t).db, opts)
	if err == nil {
		t.Fatal("Unscoped was honoured over an executor with no registry")
	}
	if !strings.Contains(err.Error(), "*sqlb.DB") {
		t.Errorf("error does not say what the executor should have been:\n%s", err)
	}
}

// The sharpest case the two features make together, and it landed while this
// branch was open: OpSingleton removes the {id}, so the row every one of its
// operations addresses is the one the scope hook leaves. Releasing that scope
// does not widen a result set — there is no key in the path and no predicate in
// the statement, so a PATCH would reach every row in the table, which is the
// default-open outcome ADR-0030 exists to close.
//
// It composes correctly because the obligation check reads the released handle,
// not because anything here knows about singletons. Asserted anyway: the two
// were designed independently, and this is the case where the composition being
// wrong would be worst.
func TestASingletonCannotReleaseTheScopeThatChoosesItsRow(t *testing.T) {
	reg := sqlb.NewRegistry()
	allFour(reg, "storefront")
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	opts := scopedOptions()
	opts.Ops = rest.OpSingleton

	// Both ways: the singleton mounts while the rule confines it.
	if err := mountScoped(t, db, opts); err != nil {
		t.Fatalf("the singleton fails to mount before any release, so this proves nothing: %v", err)
	}

	opts.Unscoped = []string{"storefront"}
	err := mountScoped(t, db, opts)
	if err == nil {
		t.Fatal("a singleton released the scope that chooses its row and still mounted")
	}
	if !strings.Contains(err.Error(), "singleton read") {
		t.Errorf("error does not name the singleton read as the unmet obligation:\n%s", err)
	}
}
