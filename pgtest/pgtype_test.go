package pgtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mind-vm/sqlb"
)

// The load-bearing assumption under structs-first adoption over sqlc.
//
// sqlc generated with `sql_package: pgx/v5` emits pgtype columns — pgtype.Date,
// pgtype.Timestamptz, pgtype.UUID — and the port report in
// docs/review-adoption-port.md found those structs scanning through sqlb with
// *zero* model edits. That works for one reason and one only: pgx v5's pgtype
// implements sql.Scanner and driver.Valuer, so sqlb's database/sql path never
// has to know what a pgtype is.
//
// That report flagged the assumption as unverified and asked for a test. It
// asked for it in example/withsqlc, which is where the rest of the sqlc
// adoption story lives — and it cannot go there. example/withsqlc is in the
// root module, whose direct requirements `mise run deps-check` pins to huma
// alone, by name, precisely so a test file cannot quietly grow a driver
// dependency. Importing pgtype there would fail the gate that exists to catch
// exactly that.
//
// It belongs here anyway. The claim is not "sqlb maps these field names" —
// example/withsqlc/adopt_test.go already covers mapping, and could do so with
// no database at all. The claim is that the *values* survive a round trip
// through database/sql, which only a real Postgres can answer.

// The compile-time half. If a future pgx release drops these interfaces from
// pgtype, this stops building — which is the point the port report was making:
// the failure should land on sqlb's CI rather than on a consumer's upgrade.
//
// Assertions rather than a comment, because a comment saying "pgtype implements
// sql.Scanner" is exactly as true after pgx removes it.
var (
	_ driver.Valuer = pgtype.UUID{}
	_ sql.Scanner   = (*pgtype.UUID)(nil)
	_ driver.Valuer = pgtype.Text{}
	_ sql.Scanner   = (*pgtype.Text)(nil)
	_ driver.Valuer = pgtype.Int8{}
	_ sql.Scanner   = (*pgtype.Int8)(nil)
	_ driver.Valuer = pgtype.Date{}
	_ sql.Scanner   = (*pgtype.Date)(nil)
	_ driver.Valuer = pgtype.Timestamptz{}
	_ sql.Scanner   = (*pgtype.Timestamptz)(nil)
)

// SqlcPost mirrors what sqlc emits under `sql_package: pgx/v5`: pgtype columns
// and no struct tags of any kind. The absence of tags is deliberate — a test
// that added `db:"..."` would pass for a reason that does not generalise to the
// sqlc output people already have.
type SqlcPost struct {
	ID          pgtype.UUID
	Title       pgtype.Text
	ViewCount   pgtype.Int8
	PublishedAt pgtype.Date
	CreatedAt   pgtype.Timestamptz
}

// Describe mutates the cached model in place and panics if a statement was
// already built against it, so it runs exactly once per test binary. This is
// also the adoption path the port report describes: a Describe call over the
// sqlc struct, rather than tags added to generated code that regeneration would
// discard.
var describeSqlcPost = sync.OnceFunc(func() {
	sqlb.Describe[SqlcPost]().
		Table("sqlc_posts").
		PrimaryKey("id")
})

func sqlcPostTable(t *testing.T) *sqlb.DB {
	t.Helper()
	describeSqlcPost()
	raw := freshDB(t)
	// Everything but the key is nullable, so the same table answers both the
	// present-value and the NULL case.
	mustExec(t, raw, `
		CREATE TABLE sqlc_posts (
			id           uuid PRIMARY KEY,
			title        text,
			view_count   bigint,
			published_at date,
			created_at   timestamptz
		)`)
	return sqlb.New(raw)
}

func TestPgtypeColumnsRoundTripThroughTheBridge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := sqlcPostTable(t)

	published := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 7, 30, 9, 15, 30, 0, time.UTC)

	want := SqlcPost{
		ID:          pgtype.UUID{Bytes: [16]byte{0x01, 0x93, 0xa5, 0x5f}, Valid: true},
		Title:       pgtype.Text{String: "a post sqlc generated the struct for", Valid: true},
		ViewCount:   pgtype.Int8{Int64: 4200, Valid: true},
		PublishedAt: pgtype.Date{Time: published, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: created, Valid: true},
	}

	// Copied before the insert, and every assertion below compares against this
	// rather than against want.
	//
	// sqlb's insert writes the RETURNING values back into the caller's struct,
	// so want is a database result by the time Exec returns. Comparing the
	// later SELECT against it would be comparing two database results: a value
	// corrupted on the way *in* would corrupt both equally and pass. This is
	// the only copy of the intended values that the round trip never touched.
	expected := want

	// The insert exercises driver.Valuer on the way out and sql.Scanner on the
	// way back, since sqlb appends RETURNING over every column.
	stored, err := sqlb.InsertRows(&want).Exec(ctx, db)
	if err != nil {
		t.Fatalf("inserting pgtype columns: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("insert returned %d rows, want 1", len(stored))
	}

	got, err := sqlb.Query[SqlcPost]().Where(sqlb.F("id").Eq(expected.ID)).One(ctx, db)
	if err != nil {
		t.Fatalf("reading pgtype columns back: %v", err)
	}

	if got.ID.Bytes != expected.ID.Bytes || !got.ID.Valid {
		t.Errorf("id = %x (valid %t), want %x", got.ID.Bytes, got.ID.Valid, expected.ID.Bytes)
	}
	if got.Title != expected.Title {
		t.Errorf("title = %+v, want %+v", got.Title, expected.Title)
	}
	if got.ViewCount != expected.ViewCount {
		t.Errorf("view_count = %+v, want %+v", got.ViewCount, expected.ViewCount)
	}
	// Compared with Equal rather than ==: the driver returns these in whatever
	// location the session carries, and a wall-clock comparison would fail on a
	// value that is the same instant.
	if !got.PublishedAt.Valid || !got.PublishedAt.Time.Equal(published) {
		t.Errorf("published_at = %v (valid %t), want %v",
			got.PublishedAt.Time, got.PublishedAt.Valid, published)
	}
	if !got.CreatedAt.Valid || !got.CreatedAt.Time.Equal(created) {
		t.Errorf("created_at = %v (valid %t), want %v",
			got.CreatedAt.Time, got.CreatedAt.Valid, created)
	}
}

// The half that a happy-path test misses. pgtype's whole reason to exist over
// plain Go types is that it carries NULL, and a codec that quietly turned an
// absent value into a zero one would pass every assertion above.
func TestPgtypeNullsRoundTripThroughTheBridge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := sqlcPostTable(t)

	want := SqlcPost{
		ID: pgtype.UUID{Bytes: [16]byte{0x02}, Valid: true},
		// Every other field left at Valid:false, which is what sqlc hands a
		// caller for a nullable column that holds NULL.
	}
	if _, err := sqlb.InsertRows(&want).Exec(ctx, db); err != nil {
		t.Fatalf("inserting NULL pgtype columns: %v", err)
	}

	got, err := sqlb.Query[SqlcPost]().Where(sqlb.F("id").Eq(want.ID)).One(ctx, db)
	if err != nil {
		t.Fatalf("reading NULL pgtype columns back: %v", err)
	}

	if got.Title.Valid {
		t.Errorf("title came back valid with %q, want NULL", got.Title.String)
	}
	if got.ViewCount.Valid {
		t.Errorf("view_count came back valid with %d, want NULL", got.ViewCount.Int64)
	}
	if got.PublishedAt.Valid {
		t.Errorf("published_at came back valid with %v, want NULL", got.PublishedAt.Time)
	}
	if got.CreatedAt.Valid {
		t.Errorf("created_at came back valid with %v, want NULL", got.CreatedAt.Time)
	}

	// And the value really is NULL in the database, not an empty string or a
	// zero timestamp that scanned back as invalid.
	nulls, err := sqlb.Query[SqlcPost]().Where(sqlb.RawPred(
		`title IS NULL AND view_count IS NULL AND published_at IS NULL AND created_at IS NULL`,
	)).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting NULLs: %v", err)
	}
	if nulls != 1 {
		t.Errorf("%d rows have every nullable column NULL, want 1", nulls)
	}
}
