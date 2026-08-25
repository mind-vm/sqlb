package schema_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

func TestParseExclusionRoundTrip(t *testing.T) {
	cases := []string{
		`EXCLUDE USING gist (coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&) WHERE ((status = 'confirmed'::text))`,
		`EXCLUDE USING gist (room_id WITH =) WHERE ((cancelled = false))`,
		`EXCLUDE USING gist (tstzrange(starts_at, ends_at) WITH &&)`,
		`EXCLUDE (a WITH =)`,
	}
	for _, def := range cases {
		e, ok := schema.ParseExclusion(def)
		if !ok {
			t.Fatalf("could not parse %q", def)
		}
		if got := e.Def(); got != def {
			t.Errorf("round trip\n got: %s\nwant: %s", got, def)
		}
	}
}

func TestParseExclusionRefusals(t *testing.T) {
	for _, def := range []string{
		`UNIQUE (a, b)`,
		`EXCLUDE USING gist (a WITH =) DEFERRABLE`,
		`EXCLUDE USING gist (a WITH =`,
		`EXCLUDE ()`,
		`EXCLUDE USING (a WITH =)`,
	} {
		if _, ok := schema.ParseExclusion(def); ok {
			t.Errorf("should refuse %q", def)
		}
	}
}

func TestCutBalancedHandlesLiterals(t *testing.T) {
	def := `EXCLUDE USING gist (a WITH =) WHERE ((note = 'a)b'::text))`
	e, ok := schema.ParseExclusion(def)
	if !ok {
		t.Fatalf("a paren inside a literal ended the group early: %q", def)
	}
	if e.Where != `(note = 'a)b'::text)` {
		t.Errorf("Where = %q", e.Where)
	}
	if got := e.Def(); got != def {
		t.Errorf("round trip\n got: %s\nwant: %s", got, def)
	}
}
