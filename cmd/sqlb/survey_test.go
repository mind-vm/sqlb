package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/introspect"
)

// The survey itself needs two databases and is exercised by running it; what is
// covered here is everything around that — the argument handling that decides
// whether it runs at all, and the pure functions that shape the report.

// The verb that made this one command rather than two. Reaching survey at all
// means the dispatch runs *before* run's package-argument handling, which would
// otherwise read the second DSN as a package pattern and hand it to `go list`.
func TestSurveyIsNotTreatedAsAPackageVerb(t *testing.T) {
	code, out := invoke(t, "survey", "postgres:///src", "postgres:///dst", "extra")
	if code != 2 {
		t.Fatalf("survey with three positional arguments exited %d, want 2:\n%s", code, out)
	}
	if strings.Contains(out, "go list") || strings.Contains(out, "matched no packages") {
		t.Errorf("survey was routed through the package resolver, so its DSNs were read as "+
			"a package pattern:\n%s", out)
	}
	if !strings.Contains(out, "<src-migrated-dsn>") {
		t.Errorf("the misuse did not print survey's own usage, which is the only place the "+
			"two DSNs are described:\n%s", out)
	}
}

// The top-level usage has to list survey, because a verb absent from `sqlb
// help` is one nobody finds — which is the state the separate binary was in.
func TestUsageListsSurvey(t *testing.T) {
	code, out := invoke(t, "help")
	if code != 0 {
		t.Fatalf("help exited %d:\n%s", code, out)
	}
	for _, want := range []string{"sqlb survey", "-modules", "-exclude"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage does not mention %q:\n%s", want, out)
		}
	}
}

// The driving verbs take their package last, so flags sit between the verb and
// it. survey takes two positional arguments and so must take its flags first,
// and the stdlib flag package stops at the first non-flag argument — meaning
// `sqlb survey $SRC $DST -modules a,b` silently ignores -modules. It says so
// instead.
func TestSurveyRefusesAFlagAfterTheDSNs(t *testing.T) {
	code, out := invoke(t, "survey", "postgres:///src", "postgres:///dst", "-modules", "billing")
	if code == 0 {
		t.Fatalf("a flag after the DSNs was accepted, so -modules would have been ignored:\n%s", out)
	}
	if !strings.Contains(out, "flags go before the two DSNs") {
		t.Errorf("the error did not say where the flag belongs:\n%s", out)
	}
}

// The default exclusion list is narrowed to what a database actually holds
// before it reaches introspect, which reports an Exclude entry it cannot find.
// Without that narrowing every survey would report four absent migration
// runners as skipped constructs — a finding about this command's defaults
// rather than about the schema.
func TestSelectExcludedKeepsOnlyWhatIsPresent(t *testing.T) {
	got := selectExcluded(defaultExcluded, []string{"goose_db_version", "invoices", "users"})
	if len(got) != 1 || got[0] != "goose_db_version" {
		t.Errorf("selectExcluded(defaults, [goose_db_version invoices users]) = %v, want [goose_db_version]", got)
	}
	if got := selectExcluded(defaultExcluded, []string{"invoices"}); len(got) != 0 {
		t.Errorf("a database on no known runner excluded %v, want nothing", got)
	}
}

// Longest prefix wins, so a module named "user" does not claim the tables of
// one named "user_billing" — a wrong split reads as a real result.
func TestByModuleAssignsTheLongestPrefix(t *testing.T) {
	var out strings.Builder
	printByModule(report{&out}, "user,user_billing", "", []tableResult{
		{Name: "user_billing_invoices"},
		{Name: "user_accounts"},
		{Name: "audit_log", Skips: []introspect.Skip{{Reason: "no"}}},
	})
	got := out.String()
	for _, want := range []string{
		"| user | 1 | 1 | 0 | 0 | green |",
		"| user_billing | 1 | 1 | 0 | 0 | green |",
		"| _unmatched — NOT a module_ | 1 | 0 | 1 | 0 | **blocked** |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the by-module table is missing %q:\n%s", want, got)
		}
	}
	// The count that decides how much of a port is blocked. audit_log matches
	// no prefix, so it blocks the unclaimed group and neither declared module —
	// three groups on the table, one of them blocked.
	//
	// The numerator and the denominator must count the same population. They
	// did not: the no-prefix row incremented the count and `len(prefixes)` was
	// the total, which on a real survey printed "3 of 3 modules blocked" over a
	// table containing a green row.
	if !strings.Contains(got, "**1 of 3 modules blocked.**") {
		t.Errorf("the blocked count is wrong, and it is what the report is read for:\n%s", got)
	}
}

// A prefix nothing matches must still appear, green: a module with no tables in
// this database is a fact about the survey's arguments, and dropping the row
// would make it look like the prefix was never passed.
func TestByModuleKeepsAModuleWithNoTables(t *testing.T) {
	var out strings.Builder
	printByModule(report{&out}, "billing,catalog", "", []tableResult{{Name: "billing_invoices"}})
	got := out.String()
	if !strings.Contains(got, "| catalog | 0 | 0 | 0 | 0 | green |") {
		t.Errorf("a module with no tables was dropped from the table:\n%s", got)
	}
}

func TestByModuleSaysNothingWithoutPrefixes(t *testing.T) {
	var out strings.Builder
	printByModule(report{&out}, "  ", "", []tableResult{{Name: "invoices"}})
	if out.Len() != 0 {
		t.Errorf("the by-module section was printed without -modules:\n%s", out.String())
	}
}

func TestOnelineFlattensAndTruncates(t *testing.T) {
	if got := oneline("a\n  b\tc "); got != "a b c" {
		t.Errorf("oneline collapsed to %q, want %q", got, "a b c")
	}
	long := oneline(strings.Repeat("x", 500))
	if !strings.HasSuffix(long, "…") || len([]rune(long)) != 161 {
		t.Errorf("oneline returned %d runes, want 160 and an ellipsis", len([]rune(long)))
	}
}

func TestKindOfReducesACommentToItsVerb(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ALTER TABLE invoices ADD COLUMN paid", "ALTER TABLE"},
		{"CREATE", "CREATE"},
		{"", "(unlabelled)"},
	} {
		if got := kindOf(tc.in); got != tc.want {
			t.Errorf("kindOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The pattern grammar is one wildcard, and the cases below are the ones the
// exclusion list is actually written against: an exact name, a suffix for a
// goose-per-module monolith, and a prefix. A pattern that matched too widely
// here would silently shrink the survey rather than fail it, which is the
// failure mode worth a test (issue #123).
func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// No wildcard is an exact name — what every caller before #123 wrote.
		{"goose_db_version", "goose_db_version", true},
		{"goose_db_version", "goose_db_version_old", false},
		{"schema_migrations", "core_users_schema_migrations", false},

		// The case the issue is about.
		{"%_schema_migrations", "core_users_schema_migrations", true},
		{"%_schema_migrations", "app_billing_schema_migrations", true},
		{"%_schema_migrations", "schema_migrations", false},
		{"%_schema_migrations", "migrations", false},

		// Prefix and both-ends.
		{"goose_%", "goose_db_version", true},
		{"goose_%", "flyway_schema_history", false},
		{"%migrations%", "core_migrations_v2", true},
		{"%", "anything", true},

		// A wildcard must not swallow the anchors around it.
		{"a%z", "az", true},
		{"a%z", "abz", true},
		{"a%z", "abzq", false},
		{"a%z", "qabz", false},
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.name); got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// selectExcluded both narrows and expands, and the narrowing is the half that
// is easy to lose: introspect reports an Exclude name it cannot find, so a
// default list naming five migration runners would otherwise arrive as four
// findings about this command's defaults rather than about the schema.
func TestSelectExcludedExpandsPatterns(t *testing.T) {
	have := []string{
		"app_billing_schema_migrations",
		"core_users_schema_migrations",
		"goose_db_version",
		"invoices",
		"users",
	}
	want := []string{
		"goose_db_version",       // present, exact
		"flyway_schema_history",  // absent — must be dropped, not reported
		"atlas_schema_revisions", // absent
		"%_schema_migrations",    // pattern, matches two
	}
	got := selectExcluded(want, have)
	expect := []string{
		"app_billing_schema_migrations",
		"core_users_schema_migrations",
		"goose_db_version",
	}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("selectExcluded\n got: %v\nwant: %v", got, expect)
	}
}

// A table matching two patterns is excluded once. Duplicates would reach
// introspect's Exclude list and read as two tables that are not there.
func TestSelectExcludedDoesNotDuplicate(t *testing.T) {
	got := selectExcluded(
		[]string{"%_schema_migrations", "core_%", "core_users_schema_migrations"},
		[]string{"core_users_schema_migrations", "orders"},
	)
	if want := []string{"core_users_schema_migrations"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selectExcluded\n got: %v\nwant: %v", got, want)
	}
}

// Nothing matching is an empty list rather than a nil-vs-empty distinction the
// caller has to think about, and an empty database excludes nothing.
func TestSelectExcludedEmptyCases(t *testing.T) {
	if got := selectExcluded([]string{"goose_db_version"}, []string{"users"}); len(got) != 0 {
		t.Errorf("nothing present should exclude nothing, got %v", got)
	}
	if got := selectExcluded(nil, []string{"users"}); len(got) != 0 {
		t.Errorf("no patterns should exclude nothing, got %v", got)
	}
	if got := selectExcluded([]string{"%"}, nil); len(got) != 0 {
		t.Errorf("no tables should exclude nothing, got %v", got)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		n, total, want int
	}{
		{43, 68, 63},
		{16, 30, 53},
		{0, 30, 0},
		{30, 30, 100},
		// The guard that matters: an empty schema must not divide by zero.
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := percent(c.n, c.total); got != c.want {
			t.Errorf("percent(%d, %d) = %d, want %d", c.n, c.total, got, c.want)
		}
	}
}

// Both corpora that produced issue #122 sit above the threshold, which is the
// point: the warning asks a question rather than answering one. If someone
// later raises it past either, the warning stops firing on the two cases it was
// built from.
func TestUnmatchedWarnThresholdCoversBothCorpora(t *testing.T) {
	for _, c := range []struct {
		name           string
		unmatched, all int
	}{
		{"a wrong -modules value", 43, 68},
		{"permanently grandfathered bare names", 16, 30},
	} {
		if got := percent(c.unmatched, c.all); got < unmatchedWarnPercent {
			t.Errorf("%s: %d of %d is %d%%, below the %d%% threshold, so the warning would not fire",
				c.name, c.unmatched, c.all, got, unmatchedWarnPercent)
		}
	}
}
