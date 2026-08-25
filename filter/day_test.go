package filter_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// Booking is the shape #241 was reported against: a booking calendar whose
// column is a timestamptz, beside a date column and a text one, because the
// answer differs for all three and the type is what decides it.
type Booking struct {
	ID       string    `db:"id" sqlb:"filter"`
	Room     string    `db:"room" sqlb:"filter"`
	StartsAt time.Time `db:"starts_at" sqlb:"type:timestamptz,filter"`
	OnDate   time.Time `db:"on_date" sqlb:"type:date,filter"`
	Loose    time.Time `db:"loose" sqlb:"filter"`
}

func bookingWhere(t *testing.T, query string) (string, []any, error) {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Booking]()})
	if err != nil {
		return "", nil, err
	}
	text, args, err := filter.Apply(sqlb.Query[Booking]().Select(sqlb.F("id")), q).SQL()
	if err != nil {
		t.Fatalf("building %q: %v", query, err)
	}
	_, where, _ := strings.Cut(text, "WHERE ")
	return where, args, nil
}

// The day operator compiles to a half-open range, which is the same rows
// `starts_at::date = $1::date` selects and a range an index can serve.
func TestDayOperatorIsAHalfOpenRange(t *testing.T) {
	where, args, err := bookingWhere(t, "starts_at=day.2026-09-01")
	if err != nil {
		t.Fatalf("day filter refused: %v", err)
	}
	for _, want := range []string{`"starts_at" >= $1::date`, `"starts_at" < ($2::date + (1))`} {
		if !strings.Contains(where, want) {
			t.Errorf("where = %q, want it to contain %q", where, want)
		}
	}
	// Bound as text, so that the date is a calendar date rather than an instant
	// whose own time zone decides which day Postgres sees.
	if len(args) != 2 || args[0] != "2026-09-01" || args[1] != "2026-09-01" {
		t.Errorf("args = %#v, want the date bound twice as text", args)
	}
}

// The silent case is now a refusal that names both ways out. This is the whole
// of #241: the request compiled, returned 200, and matched nothing.
func TestBareDateAgainstATimestampIsRefused(t *testing.T) {
	for _, query := range []string{
		"starts_at=eq.2026-09-01",
		"starts_at=2026-09-01", // the shorthand, which infers eq
		"starts_at=ne.2026-09-01",
		"starts_at=in.2026-09-01,2026-09-02",
	} {
		_, _, err := bookingWhere(t, query)
		if err == nil {
			t.Errorf("%s was accepted", query)
			continue
		}
		for _, want := range []string{
			"compares against midnight",
			"starts_at=day.2026-09-01",
			"or give a full timestamp",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error does not say %q:\n%v", query, want, err)
			}
		}
	}
}

// And only that case. A full timestamp is exact, the ordering operators mean
// what they say against midnight, a date column compares against a date
// correctly, and a column whose type nothing declared is left alone — PGType is
// empty for a hand-written model, and unknown is a real answer.
func TestWhatTheRefusalDoesNotTouch(t *testing.T) {
	for _, query := range []string{
		"starts_at=eq.2026-09-01T09:00:00Z",
		"starts_at=gte.2026-09-01",
		"starts_at=lt.2026-09-01",
		"starts_at=between.2026-09-01,2026-09-30",
		"on_date=eq.2026-09-01",
		"loose=eq.2026-09-01",
	} {
		if _, _, err := bookingWhere(t, query); err != nil {
			t.Errorf("%s was refused: %v", query, err)
		}
	}
}

// The day operator works on a date column too, where it is merely a longer way
// to say eq — worth allowing, because a caller filtering "that day" should not
// have to know which of the two types the column is.
func TestDayOperatorOnADateColumn(t *testing.T) {
	where, _, err := bookingWhere(t, "on_date=day.2026-09-01")
	if err != nil {
		t.Fatalf("day on a date column was refused: %v", err)
	}
	if !strings.Contains(where, `"on_date" >= $1::date`) {
		t.Errorf("where = %q", where)
	}
}

// It is refused on a column that is not a time at all, naming the column's type
// rather than leaving Postgres to answer with a 500 about an operator.
func TestDayOperatorNeedsATimeColumn(t *testing.T) {
	_, _, err := bookingWhere(t, "room=day.2026-09-01")
	if err == nil {
		t.Fatal("day on a text column was accepted")
	}
	if !strings.Contains(err.Error(), "needs a date or timestamp column") {
		t.Errorf("error = %v", err)
	}
}

// A malformed day is a 400 that shows the spelling, not a database error.
func TestDayOperatorNeedsACalendarDate(t *testing.T) {
	for _, query := range []string{
		"starts_at=day.2026-09-01T09:00:00Z",
		"starts_at=day.yesterday",
		"starts_at=day.",
	} {
		_, _, err := bookingWhere(t, query)
		if err == nil {
			t.Errorf("%s was accepted", query)
			continue
		}
		if !strings.Contains(err.Error(), "takes a calendar date") {
			t.Errorf("%s: error = %v", query, err)
		}
	}
}

// The JSON frontend compiles to the same predicate and refuses the same
// request, which is ADR-0003's claim about the two frontends.
func TestJSONFrontendAgrees(t *testing.T) {
	parse := func(body string) (*filter.Query, error) {
		return filter.Parse(url.Values{"filter": {body}}, filter.Options{Model: sqlb.ModelOf[Booking]()})
	}

	q, err := parse(`{"op":"day","field":"starts_at","value":"2026-09-01"}`)
	if err != nil {
		t.Fatalf("JSON day filter refused: %v", err)
	}
	text, _, _ := filter.Apply(sqlb.Query[Booking]().Select(sqlb.F("id")), q).SQL()
	if !strings.Contains(text, `"starts_at" >= $1::date`) {
		t.Errorf("JSON day filter compiled to %q", text)
	}

	if _, err := parse(`{"op":"eq","field":"starts_at","value":"2026-09-01"}`); err == nil {
		t.Error("a bare date was accepted through the JSON frontend")
	} else if !strings.Contains(err.Error(), "compares against midnight") {
		t.Errorf("error = %v", err)
	}
}
