package recipes_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// WithTx runs a unit of work on one connection, committing if the function
// returns nil and rolling back otherwise. The handle passed in executes on the
// transaction, so every statement inside lands there.
//
// Pass the *inner* ctx onward, not the enclosing one: it is what carries the
// transaction, and it is what makes TxFrom work inside a hook.
func Example_transactionCommits() {
	db := recordingDB()

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		post := recipes.Post{OrgID: "acme", Title: "Hello"}
		if _, err := sqlb.InsertRows(&post).One(ctx, tx); err != nil {
			return err
		}
		_, err := sqlb.UpdateRows[recipes.Comment]().
			Set("post_id", post.ID).
			Where(sqlb.F("id").Eq("c1")).
			Exec(ctx, tx)
		return err
	})
	if err != nil {
		panic(err)
	}
	for _, s := range statements() {
		fmt.Println(firstWords(s, 3))
	}
	// Output:
	// BEGIN
	// INSERT INTO "posts"
	// UPDATE "comments" SET
	// COMMIT
}

// Returning an error rolls back. So does a panic — which is re-raised
// afterwards, so a transaction is never left open by one.
func Example_transactionRollsBack() {
	db := recordingDB()

	errRejected := errors.New("the domain said no")
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		post := recipes.Post{OrgID: "acme", Title: "Hello"}
		if _, err := sqlb.InsertRows(&post).One(ctx, tx); err != nil {
			return err
		}
		return errRejected
	})

	fmt.Println("returned:", errors.Is(err, errRejected))
	for _, s := range statements() {
		fmt.Println(firstWords(s, 3))
	}
	// Output:
	// returned: true
	// BEGIN
	// INSERT INTO "posts"
	// ROLLBACK
}

// AfterCommit is where anything the outside world can observe belongs —
// publishing an event, enqueuing a job, invalidating a cache.
//
// The AfterCreate family runs *inside* the transaction, which is correct for
// validation (an error there rolls the write back) and wrong for a side effect,
// because the transaction may still abort after the hook has already announced
// a write that then never happened.
func Example_transactionAfterCommit() {
	db := recordingDB()

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		post := recipes.Post{OrgID: "acme", Title: "Hello"}
		if _, err := sqlb.InsertRows(&post).One(ctx, tx); err != nil {
			return err
		}
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			fmt.Println("published post.created")
			return nil
		})
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("last statement:", statements()[len(statements())-1])
	// Output:
	// published post.created
	// last statement: COMMIT
}

// A callback registered on a transaction that rolls back never runs. That is
// the whole guarantee, and it is the reason to reach for AfterCommit rather
// than doing the side effect after WithTx returns — the registration sits next
// to the write it belongs to.
func Example_transactionAfterCommitSkippedOnRollback() {
	db := recordingDB()

	err := db.WithTx(context.Background(), func(ctx context.Context, _ *sqlb.DB) error {
		if err := sqlb.AfterCommit(ctx, func(context.Context) error {
			fmt.Println("this never prints")
			return nil
		}); err != nil {
			return err
		}
		return errors.New("aborted")
	})
	fmt.Println("err:", err)
	fmt.Println("last statement:", statements()[len(statements())-1])
	// Output:
	// err: aborted
	// last statement: ROLLBACK
}

// Nesting joins rather than nests: WithTx on a handle already in a transaction
// runs on that same transaction and leaves the commit to the outermost call.
// That keeps a function which opens a transaction callable from inside one,
// which is what a service method needs to be.
func Example_transactionNestingJoins() {
	db := recordingDB()

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		return tx.WithTx(ctx, func(ctx context.Context, inner *sqlb.DB) error {
			fmt.Println("inner is in a transaction:", inner.InTx())
			post := recipes.Post{Title: "Hello"}
			_, err := sqlb.InsertRows(&post).One(ctx, inner)
			return err
		})
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("begins:", count(statements(), "BEGIN"), "commits:", count(statements(), "COMMIT"))
	// Output:
	// inner is in a transaction: true
	// begins: 1 commits: 1
}
