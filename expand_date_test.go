package sqlb_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// The shape from #84: an expansion target carrying a date column. A date and a
// timestamptz are both time.Time in the row struct, so the tag is the only
// thing that tells them apart.
type dateProject struct {
	ID      string     `db:"id" json:"id" sqlb:"type:uuid,pk"`
	Name    string     `db:"name" json:"name" sqlb:"type:text"`
	DueOn   *time.Time `db:"due_on" json:"due_on" sqlb:"type:date"`
	Started time.Time  `db:"started_at" json:"started_at" sqlb:"type:timestamptz"`
}

func (dateProject) TableName() string { return "projects" }

type dateEntry struct {
	ID        string       `db:"id" json:"id" sqlb:"type:uuid,pk"`
	ProjectID string       `db:"project_id" json:"project_id" sqlb:"type:uuid,expand"`
	Project   *dateProject `db:"-" json:"project,omitempty" sqlb:"expands=project_id"`
}

func (dateEntry) TableName() string { return "time_entries" }

// json_build_object serialises a date as "2026-07-01", and encoding/json parses
// a time.Time strictly as RFC 3339, so the expansion answered 500. The cast is
// what makes the embedded value the same shape a direct read of the same column
// produces (#84).
func TestAnExpandedDateColumnIsCastToRFC3339(t *testing.T) {
	sql, _, err := sqlb.Query[dateEntry]().Expand("project").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `'due_on', ("__ex_project"."due_on"::timestamp AT TIME ZONE 'UTC')`
	if !strings.Contains(sql, want) {
		t.Errorf("the date column was not cast:\nwant it to contain: %s\ngot: %s", want, sql)
	}
}

// AT TIME ZONE 'UTC' rather than ::timestamptz is the whole correctness of the
// cast: ::timestamptz resolves through the session TimeZone, so under
// Europe/Berlin the date would come back a day earlier. Pinned because the
// shorter spelling looks equivalent and is not.
func TestTheDateCastDoesNotGoThroughTheSessionTimeZone(t *testing.T) {
	sql, _, err := sqlb.Query[dateEntry]().Expand("project").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, `"due_on"::timestamptz`) {
		t.Errorf("the date is cast through the session time zone, which shifts it by a day:\n%s", sql)
	}
}

// Only the type that needs it. A timestamptz is already RFC 3339 in
// json_build_object's output, and casting it would be a second representation
// of a value that had a correct one.
func TestOtherColumnsAreNotCast(t *testing.T) {
	sql, _, err := sqlb.Query[dateEntry]().Expand("project").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, plain := range []string{
		`'started_at', "__ex_project"."started_at"`,
		`'name', "__ex_project"."name"`,
		`'id', "__ex_project"."id"`,
	} {
		if !strings.Contains(sql, plain) {
			t.Errorf("a column that needs no cast acquired one:\nwant %s\ngot: %s", plain, sql)
		}
	}
}

// The decode half, which is what the 500 actually was. This is the JSON
// Postgres produces after the cast, and it has to land in the time.Time the
// row struct declares.
func TestTheCastFormDecodesIntoTheRowStruct(t *testing.T) {
	// What json_build_object emits for a timestamptz at UTC midnight.
	raw := []byte(`{"id":"p1","name":"Atlas","due_on":"2026-07-01T00:00:00+00:00","started_at":"2026-06-01T09:00:00+00:00"}`)

	var got dateProject
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the cast form does not decode into the row struct: %v", err)
	}
	if got.DueOn == nil || !got.DueOn.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("due_on decoded to %v, want 2026-07-01T00:00:00Z", got.DueOn)
	}

	// And the form that caused the issue still does not, which is why the cast
	// is on the SQL side rather than left to the decoder.
	if err := json.Unmarshal([]byte(`{"due_on":"2026-07-01"}`), &dateProject{}); err == nil {
		t.Error("a bare date now decodes; the premise of the fix has changed")
	}
}
