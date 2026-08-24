package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jryannel/sqlb"
)

// Named scopes, and the handle that releases them (#177).
//
// The property under test is not "a release works" — that one is easy and is
// the first case below. It is the pair of asymmetries that make the release
// safe to offer at all:
//
//   - an unnamed registration cannot be released, whatever a handle passes, so
//     the author of a rule decides whether the rule is negotiable; and
//   - a released registration stops counting toward ADR-0030, so a resource
//     cannot release its way past the check that exists to catch an unconfined
//     mount. That half is proved in rest/scope_test.go, where the mount is.
//
// Every guard here is asserted in both directions, because a scope test that
// only ever sees the predicate absent cannot tell "released" from "never
// registered" (ADR-0016).

// publishedOnly is the storefront's rule in the shape the shop actually writes
// it: a plain column predicate, so it survives requalification onto a join
// alias when a request reaches this model through ?expand.
func publishedOnly(_ context.Context, q *sqlb.Builder[User]) error {
	q.Where(sqlb.F("org_id").Eq("published"))
	return nil
}

func TestANamedScopeAppliesUntilAHandleReleasesIt(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)

	storefront := sqlb.New(h.db).WithHooks(reg)
	if _, err := sqlb.Query[User]().All(context.Background(), storefront); err != nil {
		t.Fatalf("All: %v", err)
	}
	// The direction that proves the rule is on at all. Without it, the release
	// below would pass against a hook that never ran.
	if got := h.lastSelect(t); !strings.Contains(got, `WHERE "org_id" =`) {
		t.Fatalf("the named scope did not apply to the handle that never released it: %s", got)
	}

	admin := storefront.WithoutScope("storefront")
	if _, err := sqlb.Query[User]().All(context.Background(), admin); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := h.lastSelect(t); strings.Contains(got, "WHERE") {
		t.Errorf("the released scope still applied: %s", got)
	}
}

// The asymmetry the whole design rests on. An ordinary BeforeQuery has no name,
// so nothing a handle passes can reach it — which is what keeps WithoutScope
// from being a way to turn scoping off, and is why the short spelling every
// existing codebase already uses stays absolute.
func TestAnUnnamedScopeCannotBeReleased(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).BeforeQuery(publishedOnly)

	// Every name this test can think to try, including the empty one.
	db := sqlb.New(h.db).WithHooks(reg).
		WithoutScope("storefront", "", "*", "all", "published")
	if _, err := sqlb.Query[User]().All(context.Background(), db); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := h.lastSelect(t); !strings.Contains(got, `WHERE "org_id" =`) {
		t.Errorf("an unnamed registration was released: %s", got)
	}
}

func TestReleasingOneScopeLeavesTheOthers(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)
	sqlb.On[User](reg).Scope("tenant").BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
		q.Where(sqlb.F("email").Eq("tenant@example.com"))
		return nil
	})

	db := sqlb.New(h.db).WithHooks(reg).WithoutScope("storefront")
	if _, err := sqlb.Query[User]().All(context.Background(), db); err != nil {
		t.Fatalf("All: %v", err)
	}
	// The predicate, not the select list — org_id is a column of the model and
	// appears in every projection whether or not a hook named it.
	got := h.lastSelect(t)
	if strings.Contains(got, `WHERE "org_id"`) || strings.Contains(got, `AND "org_id"`) {
		t.Errorf("the released scope still applied: %s", got)
	}
	if !strings.Contains(got, `WHERE "email"`) {
		t.Errorf("releasing one scope dropped another: %s", got)
	}
}

// A release accumulates rather than replacing, so a handle derived twice is
// released from both. The alternative — the second call winning — would make
// the order of two independent decisions matter.
func TestReleasesAccumulateAcrossDerivedHandles(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)
	sqlb.On[User](reg).Scope("tenant").BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
		q.Where(sqlb.F("email").Eq("tenant@example.com"))
		return nil
	})

	db := sqlb.New(h.db).WithHooks(reg).WithoutScope("storefront").WithoutScope("tenant")
	if _, err := sqlb.Query[User]().All(context.Background(), db); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := h.lastSelect(t); strings.Contains(got, "WHERE") {
		t.Errorf("a scope released by the first derivation came back: %s", got)
	}
}

// The release travels with the handle, including into a transaction — the same
// property WithHooks has, and for the same reason: a unit of work that silently
// re-applied a rule its handle released would be the surprise.
func TestAReleaseSurvivesIntoTheTransaction(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)

	db := sqlb.New(h.db).WithHooks(reg).WithoutScope("storefront")
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if got := tx.Released(); len(got) != 1 || got[0] != "storefront" {
			t.Errorf("the transaction handle lost its release: %v", got)
		}
		_, err := sqlb.Query[User]().All(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if got := h.lastSelect(t); strings.Contains(got, "WHERE") {
		t.Errorf("the release did not survive into the transaction: %s", got)
	}
}

// RegisteredFor is what the mount check reads, so it has to answer about the
// handle rather than about the registry. A registration the handle released is
// not confining anything, and reporting it would let a resource release its way
// past ADR-0030.
func TestRegisteredForDoesNotCountAReleasedRegistration(t *testing.T) {
	h := txHarness(t)

	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)

	db := sqlb.New(h.db).WithHooks(reg)
	if !sqlb.RegisteredFor[User](db).BeforeQuery {
		t.Fatal("the registration is not visible before any release")
	}
	if sqlb.RegisteredFor[User](db.WithoutScope("storefront")).BeforeQuery {
		t.Error("a released registration still counts as confining the model")
	}
	// An unnamed one keeps counting, which is the other half: releasing a name
	// must not be able to make an absolute rule disappear from the check.
	sqlb.On[User](reg).BeforeQuery(publishedOnly)
	if !sqlb.RegisteredFor[User](db.WithoutScope("storefront")).BeforeQuery {
		t.Error("an unnamed registration stopped counting when a name was released")
	}
}

func TestScopeNamesEnumeratesEveryRegisteredName(t *testing.T) {
	reg := sqlb.NewRegistry()
	if got := reg.ScopeNames(); len(got) != 0 {
		t.Errorf("a fresh registry has scope names: %v", got)
	}

	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly)
	sqlb.On[User](reg).Scope("storefront").BeforeQuery(publishedOnly) // deduped
	sqlb.On[User](reg).BeforeQuery(publishedOnly)                     // unnamed, not a scope
	// A second model, to prove a scope name is a property of the registry
	// rather than of one type: the shop's "storefront" spans four tables.
	sqlb.On[deleteDoc](reg).Scope("tenant").BeforeQuery(func(_ context.Context, _ *sqlb.Builder[deleteDoc]) error {
		return nil
	})

	got := reg.ScopeNames()
	want := []string{"storefront", "tenant"}
	if len(got) != len(want) {
		t.Fatalf("ScopeNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ScopeNames() = %v, want %v (sorted)", got, want)
		}
	}
}

// Scope("") is a programming error rather than a runtime condition: it would
// register an absolute rule through the spelling that means "negotiable", which
// reads at the call site as the opposite of what it does.
func TestAnEmptyScopeNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Scope(\"\") did not panic")
		}
	}()
	sqlb.On[User](sqlb.NewRegistry()).Scope("")
}

// ScopedHooks.BeforeCreate exists only to panic (#289's second report): the
// absence of a method reads as a missing feature, not as the deliberate
// refusal the doc comment on ScopedHooks explains, so a reader who writes the
// obvious fourth call should hit the reasoning at the call site rather than
// go looking for it in prose.
func TestScopedBeforeCreatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("ScopedHooks.BeforeCreate did not panic")
		}
	}()
	sqlb.On[User](sqlb.NewRegistry()).Scope("tenant").
		BeforeCreate(func(context.Context, *User) error { return nil })
}

// The create side, which Scope deliberately does not cover, and the shape that
// has to stand in for it (#289).
//
// BeforeCreate stamps a row rather than confining a set, so releasing it would
// write a row with no tenant instead of showing one more row — a released read
// fails visibly, a released stamp fails silently. The cost lands on the creates
// with no request behind them, which cannot be released and so must satisfy the
// hook: a fixture, a seed, an import, a job.
//
// docs/queries/hooks.md documents the fallback that answers for them. This is
// that hook, and all three of its branches, because the one that matters is the
// one a reader is most likely to write as an unconditional `return nil` — and
// that mistake passes any test which only ever creates rows with claims
// present.
//
// The stamp is read through a second BeforeCreate rather than off the row
// afterwards: an insert scans its RETURNING back over the row it was handed, so
// by the time Exec returns, what is in the field is what the database said and
// not what the policy decided.
func TestTheTrustedCreatePathStampsFallsBackAndStillFailsClosed(t *testing.T) {
	type callerKey struct{}

	var stamped []string
	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).
		BeforeCreate(func(ctx context.Context, row *User) error {
			org, ok := ctx.Value(callerKey{}).(string)
			if !ok {
				// No claims: a fixture, a seed, an import, a job. Trust the
				// row only if it named a tenant itself.
				if row.OrgID != "" {
					return nil
				}
				return errors.New("no tenant in this context")
			}
			row.OrgID = org // stamp; never trust the body when there is a caller
			return nil
		}).
		BeforeCreate(func(_ context.Context, row *User) error {
			stamped = append(stamped, row.OrgID)
			return nil
		})

	request := context.WithValue(context.Background(), callerKey{}, "org-from-claims")
	trusted := context.Background()

	insert := func(t *testing.T, ctx context.Context, row *User) error {
		t.Helper()
		stamped = nil
		h := txHarness(t)
		_, err := sqlb.InsertRows(row).Exec(ctx, sqlb.New(h.db).WithHooks(reg))
		return err
	}

	t.Run("a request stamps from the claims, not from the body", func(t *testing.T) {
		row := &User{ID: "u1", Email: "a@b.c", Name: "Ada", OrgID: "somebody-elses-org"}
		if err := insert(t, request, row); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if len(stamped) != 1 || stamped[0] != "org-from-claims" {
			t.Errorf("the body's tenant survived a request that had claims: %v", stamped)
		}
	})

	t.Run("a trusted caller supplying the tenant is allowed through", func(t *testing.T) {
		row := &User{ID: "u2", Email: "b@b.c", Name: "Grace", OrgID: "org-from-the-seed"}
		if err := insert(t, trusted, row); err != nil {
			t.Fatalf("a seed naming its own tenant should be allowed: %v", err)
		}
		if len(stamped) != 1 || stamped[0] != "org-from-the-seed" {
			t.Errorf("the fallback did not leave the row's own tenant alone: %v", stamped)
		}
	})

	t.Run("neither claims nor a tenant is still refused", func(t *testing.T) {
		row := &User{ID: "u3", Email: "c@b.c", Name: "Alan"}
		if err := insert(t, trusted, row); err == nil {
			t.Error("a create with no claims and no tenant was written; this is the branch " +
				"an unconditional `return nil` gets wrong, and the row is unowned forever")
		}
	})
}
