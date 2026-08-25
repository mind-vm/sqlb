// Package outboxschema declares the schema for example/outbox: a
// transactional outbox table claimed by a pool of competing workers, with a
// retry policy and a dead-letter rule around the claim.
//
// See ../README.md before reading this as more than one worked answer.
// ADR-0012 (docs/architecture.md, "Change feed outbox") says in so many words that
// freezing an outbox row format on a guess is the mistake sqlb's pre-1.0
// stance exists to avoid — this schema is that guess, made once, on the
// record, so the claim mechanism underneath it has something real to be
// tested against.
package outboxschema

import "github.com/mind-vm/sqlb/schema"

// Event is one outbox row: a unit of work a worker claims, processes, and
// either completes or fails. Topic and Payload are the caller's contract —
// this package only knows about the claim lifecycle, not what any topic
// means.
var Event = schema.Table("outbox_events",
	schema.BigSerial("id").PrimaryKey(),
	schema.Text("topic").Filterable(),

	// jsonb over bytea: a real event payload is a JSON-shaped document a
	// consumer inspects (and, on Postgres, could index into with a GIN
	// expression), not an opaque blob whose structure only the producer
	// understands. This package has no codegen step, so worker.go's
	// OutboxEvent carries it as a plain []byte — pgx round-trips jsonb through
	// []byte exactly as it does bytea — but the column stays jsonb rather than
	// bytea because that is what tells anything inspecting this table
	// directly (a dashboard query, an ad-hoc SELECT) that the bytes are a
	// document rather than an opaque blob. See ../README.md for the fuller
	// case against Bytes.
	schema.JSON("payload"),

	// pending -> processing on Claim, processing -> done on Complete or back
	// to pending (retry) / dead (dead-letter) on Fail. There is no
	// processing -> dead path directly from a stuck claim — see the README's
	// "still open" section: nothing here reclaims a row a worker claimed and
	// then never finished.
	schema.Enum("status", "pending", "processing", "done", "dead").
		Default(schema.Value("pending")).
		Filterable(),

	schema.Int("attempts").Default(schema.Value(0)).Filterable(),
	schema.Int("max_attempts").Default(schema.Value(5)),

	// The relative-time predicate Claim filters on — available_at <= now() —
	// is the exact shape pgtest/census_test.go's
	// TestRelativeTimeWindowNeedsRawOrAGoComputedInstant settles, and this
	// table picks the spelling that test says to pick for a shared boundary:
	// worker.go's Claim compares available_at against the database's own
	// now() through sqlb.RawPred, not against an instant computed in Go and
	// bound as an ordinary parameter. Every worker asking "what is due right
	// now" has to get the same answer regardless of whose host clock asked.
	// Fail, writing this same column back on retry, makes the opposite
	// choice for the opposite reason — see its comment in worker.go.
	schema.Timestamp("available_at").Default(schema.Now()).Filterable(),

	schema.Timestamps(),
).Index("status", "available_at")
