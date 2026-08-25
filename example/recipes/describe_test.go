package recipes_test

import (
	"fmt"
	"time"

	"github.com/mind-vm/sqlb"
)

// Invoice is a struct sqlb did not generate and does not own — the output of
// another generator, or a type from a package you would rather not edit. It
// carries no `sqlb` tags at all.
type Invoice struct {
	ID           string
	CustomerID   string
	AmountDue    int64
	Paid         bool
	InternalMemo string
	CreatedAt    time.Time
}

// Describe attaches the same metadata at runtime that the tags would carry.
// It is the answer to "can I use this without code generation", and the more
// common case: layering sqlb over structs that already exist.
//
// Call it during initialisation, before any query runs — from init, as here in
// spirit. It mutates the cached model in place and does not lock, because a
// mutex there would put a cost on the read path of every query to pay for
// something that happens once at startup. Calling it after the first statement
// panics rather than racing, and naming a column that does not exist panics
// too, listing the ones that do.
func Example_describeAModelWithoutTags() {
	sqlb.Describe[Invoice]().
		Table("invoices").
		PrimaryKey("id").
		Column("InternalMemo", "internal_memo").
		Defaulted("id", "created_at").
		Filterable("customer_id", "paid", "amount_due").
		Sortable("created_at", "amount_due").
		Hidden("internal_memo")

	show(sqlb.Query[Invoice]().
		Where(sqlb.F("paid").Eq(false)).
		OrderBy(sqlb.F("amount_due").Desc()).
		Limit(5))
	// Output:
	// SELECT "invoices"."id", "invoices"."customer_id", "invoices"."amount_due", "invoices"."paid", "invoices"."internal_memo", "invoices"."created_at" FROM "invoices" WHERE "paid" = $1 ORDER BY "amount_due" DESC LIMIT 5
	// args: [false]
}

// Without tags or a description the builder still works — column names are
// derived from field names — but no column is filterable, sortable or
// searchable, so the REST layer rejects every request against it.
//
// That is the intended default rather than an oversight: capabilities are
// opt-in, and an undescribed model exposes nothing. The alternative default
// exposes a table the moment someone writes a struct.
type Ledger struct {
	ID     string
	Secret string
}

func Example_describeDefaultsToNoCapabilities() {
	model := sqlb.ModelOf[Ledger]()

	fmt.Println("table:", model.Table)
	for _, c := range model.Columns {
		fmt.Printf("  %-7s filterable=%v sortable=%v\n", c.Name, c.Filterable, c.Sortable)
	}
	// Output:
	// table: ledgers
	//   id      filterable=false sortable=false
	//   secret  filterable=false sortable=false
}

// Receipt is partly tagged, which is the usual state of a model being adopted.
type Receipt struct {
	ID        string    `db:"id" sqlb:"pk,default"`
	OrderID   string    `db:"order_id"`
	Total     int64     `db:"total"`
	CreatedAt time.Time `db:"created_at" sqlb:"default"`
}

// A description merges onto whatever the tags already said, so a partly tagged
// model is completed rather than restated. ModelOf reads back what the two
// together produced — which is also the value filter.Options is handed, so this
// is how to check what a REST resource will actually accept.
func Example_describeInspectTheModel() {
	sqlb.Describe[Receipt]().
		Table("receipts").
		Filterable("order_id").
		Sortable("created_at", "total")

	model := sqlb.ModelOf[Receipt]()
	fmt.Println("table:", model.Table, "pk:", model.PK.Name)
	for _, c := range model.Columns {
		fmt.Printf("  %-10s filterable=%-5v sortable=%-5v defaulted=%v\n",
			c.Name, c.Filterable, c.Sortable, c.HasDefault)
	}
	// Output:
	// table: receipts pk: id
	//   id         filterable=true  sortable=false defaulted=true
	//   order_id   filterable=true  sortable=false defaulted=false
	//   total      filterable=false sortable=true  defaulted=false
	//   created_at filterable=false sortable=true  defaulted=true
}
