package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// The two models a checkout needs: the row a request creates, and the row its
// consequence lands on. Stock's scope column is the *shop's*, and Order's is the
// buyer's, which is the whole of the problem below.
type xmOrder struct {
	ID  string `db:"id" sqlb:"pk,default"`
	SKU string `db:"sku" sqlb:"filter"`
	Qty int32  `db:"qty"`
}

func (xmOrder) TableName() string { return "orders" }

type xmStock struct {
	ID     string `db:"id" sqlb:"pk,default"`
	ShopID string `db:"shop_id" sqlb:"filter,readonly"`
	SKU    string `db:"sku" sqlb:"filter"`
	Count  int32  `db:"count"`
}

func (xmStock) TableName() string { return "stocks" }

// A hook reaching another model runs on the handle the request is running on,
// and that handle carries the request's rules.
//
// This is the trap documented in docs/queries/hooks.md#writing-the-consequence,
// pinned as a statement rather than as a paragraph. The buyer-scoping hook that
// rest.Resource obliges of Stock is appended to the shop's inventory write, so
// against a real database the UPDATE matches nothing — and with Exec that is an
// empty slice and a nil error, which commits (#159).
//
// The behaviour is correct and is not what this test argues about. What it pins
// is that the behaviour is *this* one, so the documentation cannot drift from it
// and a change here has to be deliberate.
func TestAHookInheritsTheRequestsRules(t *testing.T) {
	h := newHarness(t, []string{"id", "shop_id", "sku", "count"}, [][]any{{"s1", "buyer-7", "abc", int32(3)}})
	defer h.close()

	reg := sqlb.NewRegistry()
	// What a Scoped stocks table obliges: every update is confined to the
	// caller. Written for the request, and it does not know a hook exists.
	sqlb.On[xmStock](reg).BeforeUpdate(func(_ context.Context, u *sqlb.Update[xmStock]) error {
		u.Where(sqlb.F("shop_id").Eq("buyer-7"))
		return nil
	})

	var seen string
	sqlb.On[xmOrder](reg).AfterCreate(func(ctx context.Context, o *xmOrder) error {
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return errors.New("orders must be placed in a transaction")
		}
		_, err := sqlb.UpdateRows[xmStock]().
			SetExpr("count", sqlb.Raw{SQL: `"count" - ?`, Args: []any{o.Qty}}).
			Where(sqlb.F("sku").Eq(o.SKU)).
			Exec(ctx, tx)
		seen = h.lastQuery()
		return err
	})

	db := h.handle(reg)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.InsertRows(&xmOrder{SKU: "abc", Qty: 1}).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("placing the order: %v", err)
	}
	if !strings.Contains(seen, `"shop_id" = $`) {
		t.Errorf("the request's scope should reach the hook's own write, so that the "+
			"documented remedy has something to remedy:\n%s", seen)
	}
}

// And the remedy: a second registry on the same transaction.
//
// WithHooks clones the handle and replaces the registry without touching the
// executor, so this is the same unit of work — the order and the decrement still
// commit or roll back together — with the request's rules off the statement that
// was never about the request.
func TestAHookEscalatesThroughASecondRegistry(t *testing.T) {
	h := newHarness(t, []string{"id", "shop_id", "sku", "count"}, [][]any{{"s1", "shop-1", "abc", int32(2)}})
	defer h.close()

	reg := sqlb.NewRegistry()
	sqlb.On[xmStock](reg).BeforeUpdate(func(_ context.Context, u *sqlb.Update[xmStock]) error {
		u.Where(sqlb.F("shop_id").Eq("buyer-7"))
		return nil
	})
	system := sqlb.NewRegistry()

	var seen string
	sqlb.On[xmOrder](reg).AfterCreate(func(ctx context.Context, o *xmOrder) error {
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return errors.New("orders must be placed in a transaction")
		}
		// One, not Exec: zero rows means there was nothing to sell, and that
		// has to refuse rather than commit an order that reserved nothing.
		_, err := sqlb.UpdateRows[xmStock]().
			SetExpr("count", sqlb.Raw{SQL: `"count" - ?`, Args: []any{o.Qty}}).
			Where(sqlb.F("sku").Eq(o.SKU)).
			One(ctx, tx.WithHooks(system))
		seen = h.lastQuery()
		return err
	})

	db := h.handle(reg)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.InsertRows(&xmOrder{SKU: "abc", Qty: 1}).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("placing the order: %v", err)
	}
	// The WHERE alone: shop_id is a column of the table and is in RETURNING
	// either way, so the whole statement is the wrong thing to read.
	_, where, _ := strings.Cut(seen, " WHERE ")
	where, _, _ = strings.Cut(where, " RETURNING ")
	if strings.Contains(where, "shop_id") {
		t.Errorf("the escalated write should carry no scope from the request:\n%s", seen)
	}
	if !strings.Contains(seen, `UPDATE "stocks"`) || !strings.Contains(where, `"sku" = $`) {
		t.Errorf("the escalated write should still be the statement the hook wrote:\n%s", seen)
	}
	// Same transaction, so the two still commit together. Anything else would
	// make the remedy worse than the trap.
	var begins int
	h.mu.Lock()
	for _, q := range h.log {
		if q == "BEGIN" {
			begins++
		}
	}
	h.mu.Unlock()
	if begins != 1 {
		t.Errorf("the escalated write opened its own transaction (%d BEGINs); "+
			"a consequence outside the unit of work cannot be rolled back with it", begins)
	}
}

// The quiet half, stated on its own: Exec reports a write that matched nothing
// as success, and One refuses it.
//
// Neither is wrong — a bulk update matching nothing is not an error — but in a
// hook the difference decides whether an order that reserved no stock commits.
func TestExecIsSilentWhereOneRefuses(t *testing.T) {
	h := newHarness(t, []string{"id", "shop_id", "sku", "count"}, nil)
	defer h.close()

	rows, err := sqlb.UpdateRows[xmStock]().
		Set("count", 1).Where(sqlb.F("sku").Eq("nothing")).
		Exec(context.Background(), h.db)
	if err != nil || len(rows) != 0 {
		t.Errorf("Exec over no rows = (%v, %v), want an empty slice and no error", rows, err)
	}

	_, err = sqlb.UpdateRows[xmStock]().
		Set("count", 1).Where(sqlb.F("sku").Eq("nothing")).
		One(context.Background(), h.db)
	if !errors.Is(err, sqlb.ErrNotFound) {
		t.Errorf("One over no rows = %v, want ErrNotFound", err)
	}
}
