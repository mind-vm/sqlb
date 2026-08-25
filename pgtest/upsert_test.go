package pgtest

import (
	"context"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/schema"
)

// Upsert assignments against a real Postgres.
//
// The engine's tests check the rendered statement. What they cannot check is
// that `DO UPDATE SET hits = "secrets"."hits" + $n` means what it reads as:
// EXCLUDED and the target table are both in scope inside DO UPDATE, and a
// qualifier that resolved to the wrong one produces a statement Postgres
// accepts and answers wrongly — the proposed row's value where the stored one
// was meant. That is a wrong number, not an error, so only a second insert of
// the same key can tell the difference (#90).

type Secret struct {
	ID        int64     `db:"id" sqlb:"type:bigint,pk,default"`
	Key       string    `db:"key" sqlb:"type:text"`
	Payload   string    `db:"payload" sqlb:"type:text"`
	Hits      int64     `db:"hits" sqlb:"type:bigint,default"`
	UpdatedAt time.Time `db:"updated_at" sqlb:"type:timestamptz,default"`
}

func (Secret) TableName() string { return "secrets" }

func secretRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("secrets",
		schema.BigInt("id").PrimaryKey().Default(schema.Expr("nextval('secrets_id_seq')")),
		schema.Text("key").Unique(),
		schema.Text("payload"),
		schema.BigInt("hits").Default(schema.Value(0)),
		schema.Timestamp("updated_at").Default(schema.Now()),
	)
	return r
}

func secretDB(t *testing.T) *sqlb.DB {
	t.Helper()
	raw := freshDB(t)
	mustExec(t, raw, `CREATE SEQUENCE secrets_id_seq`)
	applySchema(t, raw, secretRegistry())
	return sqlb.New(raw)
}

// The accumulate case, twice, because once cannot distinguish the stored row
// from the proposed one: on the first insert they hold the same value.
func TestUpsertAccumulatesFromTheStoredRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := secretDB(t)

	upsert := func() (Secret, error) {
		row := &Secret{Key: "k", Payload: "p"}
		out, err := sqlb.InsertRows(row).
			OnConflictUpdate([]string{"key"}, "payload").
			OnConflictSet("hits", sqlb.Add(sqlb.Current("hits"), sqlb.Val(1))).
			Exec(ctx, db)
		if err != nil {
			return Secret{}, err
		}
		if len(out) != 1 {
			t.Fatalf("got %d rows back, want 1", len(out))
		}
		return out[0], nil
	}

	first, err := upsert()
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.Hits != 0 {
		t.Errorf("a fresh insert took the conflict branch: hits = %d, want 0", first.Hits)
	}

	for want := int64(1); want <= 3; want++ {
		got, err := upsert()
		if err != nil {
			t.Fatalf("upsert %d: %v", want, err)
		}
		// If Current had resolved to EXCLUDED, every one of these would be 1:
		// the proposed row carries the zero value for hits each time.
		if got.Hits != want {
			t.Fatalf("hits = %d after %d conflicts, want %d — the accumulation is reading the proposed row, not the stored one",
				got.Hits, want, want)
		}
	}
}

// The case the issue opened with: the timestamp comes from the database rather
// than from the process, so it agrees with every other timestamp on the row.
func TestUpsertTakesTheTimestampFromTheDatabaseClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := secretDB(t)

	row := &Secret{Key: "k", Payload: "first"}
	created, err := sqlb.InsertRows(row).Exec(ctx, db)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := created[0].UpdatedAt

	updated, err := sqlb.InsertRows(&Secret{Key: "k", Payload: "second"}).
		OnConflictUpdate([]string{"key"}, "payload").
		OnConflictSet("updated_at", sqlb.Now()).
		Exec(ctx, db)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if updated[0].Payload != "second" {
		t.Errorf("payload = %q, want the proposed value", updated[0].Payload)
	}
	if !updated[0].UpdatedAt.After(before) {
		t.Errorf("updated_at did not move: %s then %s", before, updated[0].UpdatedAt)
	}
}
