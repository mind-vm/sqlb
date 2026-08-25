// Package meterschema declares the schema of the `meter` example: one row per
// (tenant, kind), written by many concurrent producers, read as a chart.
//
// It exists to settle docs/special-cases.md's "meter" case, and settles it in
// two directions. First, a correction: the census's headline finding — "the
// arithmetic upsert form has no spelling" — is closed. Insert.OnConflictSet
// landed in #90, and meter_test.go's concurrency test is its demonstration
// under real writers, not a bug report. Second, an argument this schema itself
// carries: the table this example actually wants has a composite key,
// (tenant, kind), and sqlb still refuses to declare one.
package meterschema

import "github.com/mind-vm/sqlb/schema"

// Meter is one row per (tenant, kind): a counter a producer increments with
// OnConflictSet rather than reads, mutates and writes back — see
// meter_test.go's TestArithmeticUpsertUnderConcurrency for why that
// distinction is the whole point of this table.
//
// The natural key is (tenant, kind), and ADR-0034 refuses a composite
// PRIMARY KEY outright — ".PrimaryKey()" on two fields is a schema-time error
// naming the workaround below. So `id` is a surrogate the table does not
// otherwise want: nothing here reads it, nothing here means it, and every
// producer still addresses a row by (tenant, kind), through OnConflictUpdate.
// It exists to give the row a single column a REFERENCES clause, a REST path
// or a keyset cursor could address — none of which this example needs — and
// its cost is real: a second index (the UniqueIndex below, which is what
// actually enforces "one row per tenant and kind"), and a client that has to
// know the surrogate is not the identity, or it will build a REST resource
// path on `id` and then be unable to explain what the id names.
//
// pgtest/census_test.go's TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround
// proves the refusal and the workaround directly; this schema is that same
// shape used for something real rather than re-proven here.
var Meter = schema.Table("meters",
	schema.BigSerial("id").PrimaryKey(),
	schema.Text("tenant").Filterable(),
	schema.Text("kind").Filterable(),

	// The clock the producer's write lands on, not the clock the producer
	// reads — Default(schema.Now()) puts "when" in the database rather than
	// trusting every producer's wall clock to agree, which with concurrent
	// writers on different hosts it will not.
	schema.Timestamp("at").Default(schema.Now()).Filterable().Sortable(),

	// The counter. Defaulting to zero matters for exactly one path: the
	// fresh-key branch of an arithmetic upsert, where OnConflictSet's
	// expression never runs and the row is INSERTed as written — see
	// TestArithmeticUpsertUnderConcurrency's first-writer-wins seed.
	schema.BigInt("count").Default(schema.Value(0)).Filterable(),
).
	// The workaround ADR-0034's refusal names, applied: (tenant, kind)
	// carried by a unique index rather than a primary key. This is what
	// makes OnConflictUpdate([]string{"tenant", "kind"}) a valid conflict
	// target, and it is also the invariant
	// TestUniqueIndexHoldsTheCompositeKey checks directly — a second
	// unconditional insert at an existing (tenant, kind) is rejected, and the
	// rejection is on this index, not on id.
	UniqueIndex("tenant", "kind")
