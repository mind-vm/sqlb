package recipes_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// orgKey is how these recipes carry a tenant on the context. In an application
// it is whatever the authentication middleware put there.
type orgKey struct{}

func orgFrom(ctx context.Context) (string, bool) {
	org, ok := ctx.Value(orgKey{}).(string)
	return org, ok
}

// BeforeQuery is the load-bearing hook: it receives the query itself, so one
// registration constrains every read of the model — including the reads that
// generated REST handlers issue. Tenant scoping stops being something each call
// site has to remember.
//
// Registration happens once at startup, into a registry the handle then
// carries. There is no process-wide registry to fall back on (ADR-0047), which
// is what makes the rules in force a property of how the handle was built.
func Example_hooksScopeEveryRead() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[recipes.Post](reg)

	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[recipes.Post]) error {
		org, ok := orgFrom(ctx)
		if !ok {
			// Not "no restriction". A read with no tenant is a bug, and the
			// shape most tenancy failures take is the fallback that lets it
			// through.
			return errors.New("no tenant on the context")
		}
		q.Where(sqlb.F("org_id").Eq(org), sqlb.F("deleted_at").IsNull())
		return nil
	})

	db := recordingDB().WithHooks(reg)
	ctx := context.WithValue(context.Background(), orgKey{}, "acme")

	// The caller filters on status and knows nothing about tenants.
	if _, err := sqlb.Query[recipes.Post]().Where(sqlb.F("status").Eq("published")).All(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("list: ", lastWhere())

	// A different entry point, scoped the same.
	if _, err := sqlb.Query[recipes.Post]().Count(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("count:", lastWhere())

	// And a request that never established a tenant does not run at all.
	_, err := sqlb.Query[recipes.Post]().All(context.Background(), db)
	fmt.Println("no tenant:", err)
	// Output:
	// list:  (("status" = $1) AND ("org_id" = $2)) AND ("deleted_at" IS NULL)
	// count: ("org_id" = $1) AND ("deleted_at" IS NULL)
	// no tenant: no tenant on the context
}

// The hook amends a clone, so running the same builder twice does not
// accumulate its predicates. That is what makes a base query safe to keep
// around.
func Example_hooksDoNotAccumulate() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[recipes.Post](reg)
	hooks.BeforeQuery(func(_ context.Context, q *sqlb.Builder[recipes.Post]) error {
		q.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	db := recordingDB().WithHooks(reg)
	ctx := context.Background()
	q := sqlb.Query[recipes.Post]().Where(sqlb.F("status").Eq("published"))

	for range 2 {
		if _, err := q.All(ctx, db); err != nil {
			panic(err)
		}
		fmt.Println(lastWhere())
	}
	// Output:
	// ("status" = $1) AND ("org_id" = $2)
	// ("status" = $1) AND ("org_id" = $2)
}

// BeforeCreate runs on each row before insert and may modify it: normalising an
// email, deriving a slug, stamping the owner from the context. Doing it here
// rather than in a handler means it also holds for the rows a generated REST
// handler creates.
func Example_hooksNormaliseOnWrite() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[recipes.Post](reg)

	hooks.BeforeCreate(func(ctx context.Context, p *recipes.Post) error {
		p.Title = strings.TrimSpace(p.Title)
		if p.Title == "" {
			return errors.New("a post needs a title")
		}
		if org, ok := orgFrom(ctx); ok {
			p.OrgID = org
		}
		return nil
	})

	ctx := context.WithValue(context.Background(), orgKey{}, "acme")
	post := recipes.Post{Title: "  Hello  "}
	if _, err := sqlb.InsertRows(&post).One(ctx, recordingDB().WithHooks(reg)); err != nil {
		panic(err)
	}
	fmt.Printf("%q in %q\n", post.Title, post.OrgID)

	empty := recipes.Post{Title: "   "}
	_, err := sqlb.InsertRows(&empty).One(ctx, recordingDB().WithHooks(reg))
	fmt.Println("refused:", err)
	// Output:
	// "Hello" in "acme"
	// refused: a post needs a title
}

// BeforeUpdate and BeforeDelete receive the statement rather than the rows, so
// they can force a column or narrow what is affected. Forcing updated_at here
// means no call site can forget it — including one written next year.
func Example_hooksAmendAStatement() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[recipes.Post](reg)

	hooks.BeforeUpdate(func(ctx context.Context, u *sqlb.Update[recipes.Post]) error {
		org, ok := orgFrom(ctx)
		if !ok {
			return errors.New("no tenant on the context")
		}
		u.Where(sqlb.F("org_id").Eq(org))
		return nil
	})

	ctx := context.WithValue(context.Background(), orgKey{}, "acme")
	_, err := sqlb.UpdateRows[recipes.Post]().
		Set("status", "published").
		Where(sqlb.F("id").Eq("p1")).
		Exec(ctx, recordingDB().WithHooks(reg))
	if err != nil {
		panic(err)
	}
	fmt.Println(lastWhere())
	// Output:
	// ("id" = $2) AND ("org_id" = $3)
}

// Two registries, and therefore two sets of domain rules, coexisting in one
// process. A handle carries exactly the rules it was given, so the strict one
// and the unrestricted one are the same pool seen through different rules.
func Example_hooksInTheirOwnRegistry() {
	strict := sqlb.NewRegistry()
	sqlb.On[recipes.Post](strict).BeforeQuery(func(_ context.Context, q *sqlb.Builder[recipes.Post]) error {
		q.Where(sqlb.F("status").Eq("published"))
		return nil
	})

	ctx := context.Background()
	db := recordingDB()

	if _, err := sqlb.Query[recipes.Post]().All(ctx, db.WithHooks(strict)); err != nil {
		panic(err)
	}
	fmt.Println("with registry:", lastWhere())

	if _, err := sqlb.Query[recipes.Post]().All(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("without:      ", lastWhere())
	// Output:
	// with registry: "status" = $1
	// without:       (no WHERE clause)
}

// A hook that must read rows written earlier in the same transaction has to
// read through the transaction handle. Reading through the pool would miss
// them, because they are not committed yet — and TxFrom is how the hook gets
// hold of it.
func Example_hooksReadInsideTheTransaction() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[recipes.Post](reg)

	hooks.BeforeCreate(func(ctx context.Context, p *recipes.Post) error {
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return errors.New("posts must be created inside a transaction")
		}
		n, err := sqlb.Query[recipes.Post]().Where(sqlb.F("title").Eq(p.Title)).Count(ctx, tx)
		if err != nil {
			return err
		}
		fmt.Println("existing posts with that title, as this transaction sees it:", n)
		return nil
	})

	db := recordingDB().WithHooks(reg)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		post := recipes.Post{Title: "Hello"}
		_, err := sqlb.InsertRows(&post).One(ctx, tx)
		return err
	})
	if err != nil {
		panic(err)
	}

	// Outside one, the hook refuses rather than reading the wrong thing.
	post := recipes.Post{Title: "Hello"}
	_, err = sqlb.InsertRows(&post).One(context.Background(), db)
	fmt.Println("outside a transaction:", err)
	// Output:
	// existing posts with that title, as this transaction sees it: 1
	// outside a transaction: posts must be created inside a transaction
}
