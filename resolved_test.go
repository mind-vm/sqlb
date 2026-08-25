package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

var errNoTenant = errors.New("no tenant in context")

// scopePredicate is the hook's clause as it renders. Matching on the bare
// column name would match the projection too, which every statement over User
// carries whether or not anything confined it.
const scopePredicate = `"org_id" = `

// scopedRegistry confines every read of User to one org, and stamps every
// update — the two hooks whose absence from an inspection is the whole of #153.
func scopedRegistry() *sqlb.Registry {
	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).
		BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
			q.Where(sqlb.F("org_id").Eq("acme"))
			return nil
		}).
		BeforeUpdate(func(_ context.Context, u *sqlb.Update[User]) error {
			u.Where(sqlb.F("org_id").Eq("acme"))
			return nil
		}).
		BeforeDelete(func(_ context.Context, d *sqlb.Delete[User]) error {
			d.Where(sqlb.F("org_id").Eq("acme"))
			return nil
		})
	return reg
}

// Resolved renders the statement that runs, which SQL() alone does not.
//
// The failure it closes is directional and silent: a facet count assembled from
// SQL() counts rows the query would never have returned, and in a multi-tenant
// application it counts every tenant's (#153).
func TestResolvedAppliesTheQueryHooks(t *testing.T) {
	h := newHarness(t, nil, nil)
	db := h.handle(scopedRegistry())

	q := sqlb.Query[User]().Where(sqlb.F("name").Eq("ada"))

	bare, _, err := q.SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bare, scopePredicate) {
		t.Fatalf("SQL() is documented as rendering what the caller built: %s", bare)
	}

	resolved, err := q.Resolved(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	sql, args, err := resolved.SQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, scopePredicate) {
		t.Errorf("the resolved statement is missing the hook's predicate: %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want the caller's value and the hook's", args)
	}

	// The receiver is untouched, as on every exec path: resolving twice must
	// not accumulate the predicate.
	again, err := q.Resolved(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	sql2, _, _ := again.SQL()
	if sql2 != sql {
		t.Errorf("resolving twice changed the statement:\n%s\n%s", sql, sql2)
	}
	if after, _, _ := q.SQL(); after != bare {
		t.Errorf("Resolved amended the caller's builder:\n%s\n%s", bare, after)
	}
}

// A handle with no hooks resolves to the statement as built, so Resolved is
// safe to reach for unconditionally.
func TestResolvedWithoutHooksIsTheStatementAsBuilt(t *testing.T) {
	h := newHarness(t, nil, nil)
	q := sqlb.Query[User]().Where(sqlb.F("name").Eq("ada"))

	want, _, _ := q.SQL()
	resolved, err := q.Resolved(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := resolved.SQL(); got != want {
		t.Errorf("an unhooked query resolved to something else:\n%s\n%s", want, got)
	}
}

// Explain's second advertised half — "did the plan regress" — is answered about
// the statement that runs. Without this it plans a query nobody issues, and a
// regression test written on it stays green through exactly the change that
// makes the real query seq-scan.
func TestExplainPlansTheStatementThatRuns(t *testing.T) {
	h := explainHarness(t, goodPlan)
	defer h.close()
	db := h.handle(scopedRegistry())

	q := sqlb.Query[User]().Where(sqlb.F("name").Eq("ada"))
	plan, err := sqlb.Explain(context.Background(), db, q)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(h.lastQuery(), scopePredicate) {
		t.Errorf("EXPLAIN planned a statement without the hook's predicate: %s", h.lastQuery())
	}
	// And the plan reports the statement it planned, so a caller logging
	// plan.SQL sees the same thing the database was asked about.
	if !strings.Contains(plan.SQL, scopePredicate) {
		t.Errorf("plan.SQL is not the statement that was planned: %s", plan.SQL)
	}
}

// ExplainAnalyze executes, so resolving hooks there is a correctness property
// rather than a reporting one: the statement it runs is the confined one.
func TestExplainAnalyzeRunsTheConfinedStatement(t *testing.T) {
	h := explainHarness(t, goodPlan)
	defer h.close()
	db := h.handle(scopedRegistry())

	if _, err := sqlb.ExplainAnalyze(context.Background(), db,
		sqlb.Query[User]().Where(sqlb.F("name").Eq("ada"))); err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	if !strings.Contains(h.lastQuery(), scopePredicate) {
		t.Errorf("ANALYZE ran a statement the hooks had not confined: %s", h.lastQuery())
	}
}

// The write statements resolve too, and for a delete that is the difference
// between explaining what will be removed and explaining a wider statement.
func TestWriteStatementsResolveTheirHooks(t *testing.T) {
	h := newHarness(t, nil, nil)
	db := h.handle(scopedRegistry())
	ctx := context.Background()

	upd, err := sqlb.UpdateRows[User]().Set("name", "ada").
		Where(sqlb.F("id").Eq("u1")).Resolved(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if sql, _, _ := upd.SQL(); !strings.Contains(sql, scopePredicate) {
		t.Errorf("the resolved update is missing the hook's predicate: %s", sql)
	}

	del, err := sqlb.DeleteRows[User]().Where(sqlb.F("id").Eq("u1")).Resolved(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if sql, _, _ := del.SQL(); !strings.Contains(sql, scopePredicate) {
		t.Errorf("the resolved delete is missing the hook's predicate: %s", sql)
	}
}

// A hook that refuses stops the inspection rather than being skipped, which is
// what makes Explain safe on a model whose scope hook errors when the tenant is
// absent from the context — the case ADR-0030 is about.
func TestResolvedPropagatesAHookError(t *testing.T) {
	h := newHarness(t, nil, nil)
	reg := sqlb.NewRegistry()
	sqlb.On[User](reg).BeforeQuery(func(context.Context, *sqlb.Builder[User]) error {
		return errNoTenant
	})
	db := h.handle(reg)

	if _, err := sqlb.Query[User]().Resolved(context.Background(), db); !errors.Is(err, errNoTenant) {
		t.Errorf("err = %v, want the hook's own error unwrapped", err)
	}
	if _, err := sqlb.Explain(context.Background(), db, sqlb.Query[User]()); !errors.Is(err, errNoTenant) {
		t.Errorf("Explain err = %v, want the hook's own error unwrapped", err)
	}
}
