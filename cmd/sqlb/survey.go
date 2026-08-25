package main

// `sqlb survey` — the one verb that reads a database instead of a declaration.
//
// It answers, for a whole existing database, the question the sqlb port keeps
// discovering ten tables at a time: which tables can sqlb's schema DSL
// describe, which cannot, and why.
//
// It is the repeatable form of a one-off adoption probe, with the one addition
// that makes the output triageable: every table is introspected ALONE as well
// as together, because introspect's report is per-construct but the drift gate
// is per-registry — one unmodelable table takes its whole module out of the
// gate (#109). Per-table isolation says which tables are adoptable today and
// which are blocked, instead of one flat list of skips.
//
// # Why this one compiles nothing
//
// Every other verb reads a registry, and a registry exists only once the schema
// package is linked in (ADR-0004), which is what the driver compile in
// driver.go is for. This verb reads a registry too — but it builds it by
// introspecting a live database, so there is no declaration to import and
// nothing to compile. That is the whole difference, and it is why `survey`
// takes two DSNs where the others take a package.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// defaultExcluded names the bookkeeping tables of the migration runners a Go
// Postgres project is most likely to be on. They are excluded rather than
// reported because no declaration will ever describe them, and a survey that
// counts them as blockers overstates the work.
//
// The list is a default, not a policy, and -exclude *extends* it rather than
// replacing it (#123). Replacing was the wrong default: it covers a runner used
// once per database, and a modular monolith running goose per module carries
// one bookkeeping table per module — none matching anything here. A caller who
// had to name those by hand also had to drop the five below to do it, and got
// no warning that they had. Entries absent from the database are dropped before
// the list reaches introspect — see the narrowing in survey.
var defaultExcluded = []string{
	"goose_db_version",       // goose
	"schema_migrations",      // golang-migrate, dbmate, ActiveRecord-style
	"atlas_schema_revisions", // atlas
	"_sqlx_migrations",       // sqlx
	"flyway_schema_history",  // flyway
}

// tableResult is one table's verdict from the per-table isolation phase.
type tableResult struct {
	Name    string
	Skips   []introspect.Skip
	Err     string
	Columns int
}

// report is where the survey writes, and the discarded write error is the same
// deliberate form as everywhere else in this command: a program whose stdout
// has gone away has no remaining channel on which to report that.
//
// The whole output is one markdown document meant to be redirected to a file,
// so a construct that failed to introspect is a *line in the report* rather
// than an error — it goes to stdout with everything else. Only a failure that
// stops the survey from running at all comes back as an error.
type report struct{ w io.Writer }

func (r report) printf(format string, a ...any) { _, _ = fmt.Fprintf(r.w, format, a...) }

// surveyUsage is separate from the top-level usage because this is the only
// verb whose arguments are not a package, and folding two argument shapes into
// one block made both harder to read.
const surveyUsage = `sqlb survey reports which of a database's tables sqlb can describe, and why not.

Usage:

    sqlb survey [flags] <src-migrated-dsn> <dst-empty-dsn>

Flags:

    -modules a,b,c    table-name prefixes to group the per-table verdict by, for
                      a modular monolith whose tables are named <module>_<table>
    -modules-file f   JSON mapping module name to its exact table names, for a
                      repo whose prefixes cannot cover every table. Wins over
                      -modules
    -exclude t1,%%pat  tables to leave out entirely, in addition to the built-in
                      migration-runner list below. %% matches any run of
                      characters, so -exclude '%%_schema_migrations' covers a
                      goose-per-module monolith:
                          %s

<src-migrated-dsn> is the database to survey; it is only read from.
<dst-empty-dsn> is a scratch database the round-trip phase writes into, and it
must already carry the extensions the source uses — Diff renders no CREATE
EXTENSION, so a bootstrap into a bare database fails once per table with
"function uuid_generate_v4() does not exist" rather than once with the missing
extension named — and Phase A prints the list as runnable SQL, so the second
run of this command is the one that works.
`

// survey runs the whole report, and is what `sqlb survey` dispatches to.
//
// Unlike the driving verbs it needs no module, no package and no compile, so it
// is reached before run resolves a package pattern.
func survey(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sqlb survey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, surveyUsage, strings.Join(defaultExcluded, "\n                          "))
	}
	modules := fs.String("modules", "", "comma-separated table-name prefixes to group the per-table verdict by")
	exclude := fs.String("exclude", "", "comma-separated tables to leave out entirely, in addition to the built-in migration-runner list. % matches any run of characters, so -exclude '%_schema_migrations' covers a goose-per-module monolith")
	modulesFile := fs.String("modules-file", "", `JSON file mapping module name to its exact table names, {"billing": ["invoices", ...]}, for a repo whose prefixes cannot cover every table. Takes precedence over -modules`)
	if err := fs.Parse(args); err != nil {
		// flag has already printed what was wrong and the usage above it.
		return exitCode(2)
	}

	rest := fs.Args()
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf(
				"%q is a flag, and survey's flags go before the two DSNs: "+
					"sqlb survey -modules billing,catalog $SRC $SCRATCH", a)
		}
	}
	if len(rest) != 2 {
		fs.Usage()
		return exitCode(2)
	}

	ctx := context.Background()
	src, err := open(ctx, rest[0])
	if err != nil {
		return fmt.Errorf("the database to survey: %w", err)
	}
	defer src.Close()
	dst, err := open(ctx, rest[1])
	if err != nil {
		return fmt.Errorf("the scratch database: %w", err)
	}
	defer dst.Close()

	return runSurvey(ctx, report{stdout}, src, dst, *modules, *modulesFile, *exclude)
}

// runSurvey is survey with the connections already open, which is what makes
// the report itself reachable from a test that has a database.
func runSurvey(ctx context.Context, out report, src, dst *pgxpool.Pool, modules, modulesFile, exclude string) error {
	wanted := append([]string(nil), defaultExcluded...)
	for _, t := range strings.Split(exclude, ",") {
		if t = strings.TrimSpace(t); t != "" {
			wanted = append(wanted, t)
		}
	}

	// Narrow the exclusion list to what the database actually holds.
	//
	// introspect reports a name in Exclude that it does not find, deliberately
	// — a typo would otherwise silently shrink what a gate checks. That is the
	// right behaviour for a gate and the wrong one for a default list covering
	// five migration runners, four of which are absent from any given project:
	// they would arrive as four skipped constructs, which is a finding about
	// this command's defaults rather than about the schema.
	//
	// It expands as well, because a pattern is the only workable spelling for a
	// runner used once per module: a goose-per-module monolith carries 11 to 17
	// bookkeeping tables and none of them matches a built-in name (#123).
	present, err := listTables(ctx, src, nil)
	if err != nil {
		return err
	}
	excluded := selectExcluded(wanted, present)

	all, err := listTables(ctx, src, excluded)
	if err != nil {
		return err
	}
	label := "nothing"
	if len(excluded) > 0 {
		label = strings.Join(excluded, ", ")
	}
	out.printf("# sqlb schema survey\n\n%d base tables (excluding %s)\n\n", len(all), label)

	// ---------------------------------------------------------------- phase A
	// Whole-schema introspect. This is what a drift gate over the entire
	// database would see.
	out.printf("## Phase A — whole-schema introspect\n\n")
	regAll, repAll, errAll := introspect.Registry(ctx, src, introspect.Options{Exclude: excluded})
	if errAll != nil {
		out.printf("REGISTRY ERROR: %s\n\n", oneline(errAll.Error()))
	} else {
		out.printf("registry built: %d tables modelled\n\n", len(regAll.Tables()))
	}
	if repAll != nil {
		// First, because it is the step before everything else in this run.
		// Phase C bootstraps each table into the scratch database and Diff
		// renders no CREATE EXTENSION, so a scratch database missing these
		// fails once per table naming a function rather than once naming the
		// extension — which is how this survey spent an hour (issue #115).
		if len(repAll.Extensions) > 0 {
			out.printf("### Extensions\n\n")
			out.printf("The source database has %d extension(s). No generated DDL creates them,\n",
				len(repAll.Extensions))
			out.printf("so the scratch database needs them before Phase C means anything:\n\n")
			out.printf("```sql\n")
			for _, e := range repAll.Extensions {
				out.printf("CREATE EXTENSION IF NOT EXISTS %q;\n", e)
			}
			out.printf("```\n\n")
		}
		out.printf("skipped constructs: %d\n", len(repAll.Skipped))
		out.printf("notes: %d\n\n", len(repAll.Notes))
		printByReason(out, repAll.Skipped)
		for _, n := range repAll.Notes {
			out.printf("  NOTE %s\n", oneline(n))
		}
		out.printf("\n")
	}

	// ---------------------------------------------------------------- phase B
	// Per-table isolation. A table that imports clean on its own is adoptable
	// now; one that does not is a blocker with a name attached.
	out.printf("## Phase B — per-table isolation\n\n")
	results := make([]tableResult, 0, len(all))
	for _, t := range all {
		r := tableResult{Name: t}
		reg, rep, err := introspect.Registry(ctx, src, introspect.Options{Only: []string{t}})
		if err != nil {
			r.Err = oneline(err.Error())
		} else if tbl := findTable(reg, t); tbl != nil {
			r.Columns = len(tbl.StoredFields())
		}
		if rep != nil {
			r.Skips = rep.Skipped
		}
		results = append(results, r)
	}

	var clean, skipped, errored []tableResult
	for _, r := range results {
		switch {
		case r.Err != "":
			errored = append(errored, r)
		case len(r.Skips) > 0:
			skipped = append(skipped, r)
		default:
			clean = append(clean, r)
		}
	}
	out.printf("| verdict | tables |\n|---|---:|\n")
	out.printf("| clean — imports with nothing dropped | %d |\n", len(clean))
	out.printf("| partial — imports, constructs dropped | %d |\n", len(skipped))
	out.printf("| refused — registry error | %d |\n\n", len(errored))

	if len(errored) > 0 {
		out.printf("### Refused\n\n")
		for _, r := range errored {
			out.printf("- **%s** — %s\n", r.Name, r.Err)
		}
		out.printf("\n")
	}
	if len(skipped) > 0 {
		out.printf("### Partial\n\n")
		for _, r := range skipped {
			out.printf("- **%s** (%d cols)\n", r.Name, r.Columns)
			for _, s := range r.Skips {
				obj := s.Object
				if obj == "" {
					obj = "-"
				}
				out.printf("    - `%s`: %s\n", obj, oneline(s.Reason))
				if s.Def != "" {
					out.printf("      `%s`\n", oneline(s.Def))
				}
			}
		}
		out.printf("\n")
	}
	out.printf("### Clean (%d)\n\n", len(clean))
	names := make([]string, 0, len(clean))
	for _, r := range clean {
		names = append(names, r.Name)
	}
	out.printf("%s\n\n", strings.Join(names, ", "))

	printByModule(out, modules, modulesFile, results)

	// ---------------------------------------------------------------- phase C
	out.printf("## Phase C — round-trip fixpoint\n\n")
	if errAll != nil {
		out.printf("skipped: whole-schema registry did not build\n\n")
		// A whole-schema registry that will not build is the survey's loudest
		// finding, not its failure — it is the answer to "can sqlb describe
		// this database", printed under Phase A and again here. Returning
		// errAll would replace two phases of report with one line on stderr.
		return nil //nolint:nilerr // errAll is reported in the document, which is the output
	}
	fp := fixpoint(ctx, out, dst, regAll, excluded)
	if !fp.complete {
		return nil
	}

	out.printf("## Verdict\n\n")
	out.printf("- tables: %d — %d clean, %d partial, %d refused\n", len(all), len(clean), len(skipped), len(errored))
	out.printf("- skipped constructs (whole schema): %d\n", lenSkips(repAll))
	out.printf("- DDL apply failures: %d\n", len(fp.applyFails))
	out.printf("- fixpoint residual: %d (reached after %s)\n", len(fp.residual), iterationCount(fp.iterations))
	return nil
}

// iterationCount spells the loop count, since "1 iterations" in a verdict is
// the kind of thing that gets read as a bug in the survey.
func iterationCount(n int) string {
	if n == 1 {
		return "1 iteration"
	}
	return fmt.Sprintf("%d iterations", n)
}

// maxFixpointRounds bounds the loop below.
//
// Three, because two is the answer for everything found so far and a bound of
// two could not tell "converged on the last round" from "still moving". A
// construct that has not settled by the third round is one whose spelling
// oscillates rather than one Postgres normalises, and that is a finding.
const maxFixpointRounds = 3

// fixpointResult is what Phase C found: whether the round trip settles, on
// which round, and what was still moving when the loop gave up.
type fixpointResult struct {
	// applyFails is from the first round only. It answers "does the DDL
	// rendered from this database apply", and every later round renders a
	// declaration derived from a database rather than the adopter's own.
	applyFails []string
	// residual is what the last round could not account for: empty when the
	// round trip settled.
	residual []migrate.Change
	// iterations is how many rounds it took to settle, or maxFixpointRounds
	// when it did not.
	iterations int
	complete   bool
}

// fixpoint renders the modelled registry's DDL into the empty database and
// re-introspects it: anything the round trip does not preserve is a construct
// the gate would report as drift forever.
//
// It iterates, and the iteration is the phase's whole point rather than
// belt-and-braces. Postgres normalises some expressions to a *different*
// spelling on the second application than on the first — a CHECK over a varchar
// is stored as a cast of the array, then, fed back verbatim, as a cast of each
// element — so the round trip is a fixpoint at two iterations and comparing
// after one reports a residual for a schema that is perfectly stable (#136).
// The residual measured after one round says "this schema will never settle"
// about a schema that settles on the next round, which is the more expensive of
// the two possible errors: Phase C is the phase an adopter trusts precisely
// when Phase B looked too good.
//
// So each round renders the *previous round's* declaration into an emptied
// database and asks whether what comes back is the same declaration. That is
// the literal reading of "is this a fixpoint", it needs no list of the
// spellings Postgres rewrites, and it therefore also covers whatever else
// normalises on second application that nobody has hit yet.
//
// It returns no error, and that is the design rather than an omission. Every
// failure in here is one of the survey's *answers* — a diff that will not
// compute against this database is exactly the kind of thing the report exists
// to name, and turning it into a non-zero exit would throw away the two phases
// already written. So a failed step is printed where it happened and the phase
// stops; complete says whether it got far enough for the verdict to have counts
// worth quoting.
func fixpoint(
	ctx context.Context, out report, dst *pgxpool.Pool,
	regAll *schema.Registry, excluded []string,
) fixpointResult {
	var res fixpointResult
	reg := regAll

	for round := 1; round <= maxFixpointRounds; round++ {
		if round > 1 {
			// The previous round's tables go, so that this one renders from
			// empty rather than diffing against what it already built.
			// Extensions stay, which is why this drops tables by name rather
			// than the schema they are in — Phase A told the adopter to create
			// them here and doing it once should be enough.
			if err := dropAll(ctx, dst, excluded); err != nil {
				out.printf("reset dst before round %d: %s\n\n", round, oneline(err.Error()))
				return res
			}
			out.printf("### Round %d\n\n", round)
			out.printf("Round %d did not settle, so what it produced is rendered again: "+
				"a declaration Postgres rewrote on the way in is fed back to find out "+
				"whether the rewrite is stable or the spelling oscillates.\n\n", round-1)
		}

		regEmpty, _, err := introspect.Registry(ctx, dst, introspect.Options{Exclude: excluded})
		if err != nil {
			out.printf("introspect dst: %s\n\n", oneline(err.Error()))
			return res
		}
		create, err := migrate.Diff(regEmpty, reg)
		if err != nil {
			out.printf("diff empty->all: %s\n\n", oneline(err.Error()))
			return res
		}
		var fails []string
		for _, c := range create {
			if _, err := dst.Exec(ctx, c.Up); err != nil {
				fails = append(fails, fmt.Sprintf("%s — %s", c.Comment, oneline(err.Error())))
			}
		}
		if round == 1 {
			res.applyFails = fails
		}
		out.printf("DDL statements: %d, apply failures: %d\n\n", len(create), len(fails))
		for i, f := range fails {
			if i >= 20 {
				out.printf("  … and %d more\n", len(fails)-20)
				break
			}
			out.printf("  FAIL %s\n", f)
		}
		if len(fails) > 0 {
			out.printf("\n")
		}

		regBack, _, err := introspect.Registry(ctx, dst, introspect.Options{Exclude: excluded})
		if err != nil {
			out.printf("re-introspect dst: %s\n\n", oneline(err.Error()))
			return res
		}
		residual, err := migrate.Diff(reg, regBack)
		if err != nil {
			out.printf("diff all->back: %s\n\n", oneline(err.Error()))
			return res
		}

		res.residual, res.iterations, res.complete = residual, round, true
		if len(residual) == 0 {
			out.printf("round trip settled after %s: residual **0**\n\n", iterationCount(round))
			return res
		}

		out.printf("round %d changed **%d** thing(s)", round, len(residual))
		if round < maxFixpointRounds {
			out.printf(" — not yet a fixpoint, so the loop continues\n\n")
		} else {
			out.printf(", and this was the last round: the residual below is what did **not** settle\n\n")
		}
		printResidual(out, residual)

		// The next round asks about what the database actually has, not about
		// what was declared to it. Comparing every round against regAll would
		// never converge on a construct Postgres rewrites, because regAll
		// keeps the spelling the source database happened to store.
		reg = regBack
	}
	return res
}

// printResidual writes one round's unaccounted-for changes, counted by kind
// and then listed.
func printResidual(out report, residual []migrate.Change) {
	byComment := map[string]int{}
	for _, c := range residual {
		byComment[kindOf(c.Comment)]++
	}
	kinds := keys(byComment)
	sort.Strings(kinds)
	out.printf("| residual kind | count |\n|---|---:|\n")
	for _, k := range kinds {
		out.printf("| %s | %d |\n", k, byComment[k])
	}
	out.printf("\n")
	for i, c := range residual {
		if i >= 30 {
			out.printf("  … and %d more\n", len(residual)-30)
			break
		}
		out.printf("  - %-45s %s\n", c.Comment, oneline(c.Up))
	}
	out.printf("\n")
}

// dropAll empties the scratch database of the tables a round created, so the
// next one renders into an empty schema.
//
// One statement with CASCADE, rather than a generated drop per table: the order
// a foreign key requires is exactly what CASCADE exists to not have to compute,
// and this is a scratch database whose entire contents this command wrote.
func dropAll(ctx context.Context, db *pgxpool.Pool, excluded []string) error {
	tables, err := listTables(ctx, db, excluded)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil
	}
	quoted := make([]string, len(tables))
	for i, t := range tables {
		quoted[i] = pgx.Identifier{t}.Sanitize()
	}
	_, err = db.Exec(ctx, "DROP TABLE IF EXISTS "+strings.Join(quoted, ", ")+" CASCADE")
	return err
}

// printByModule regroups the per-table verdict by table-name prefix.
//
// A flat verdict list under-reports a modular monolith. The gate is per
// registry, and a modular monolith has one registry per module (ADR-0015), so
// a table that cannot be modelled does not take out "the schema" — it takes
// out its module, and the count of affected modules is what decides how much
// of the port is blocked. Whether one blocked table means one app or six is
// not visible in a list sorted by table name.
//
// Prefixes are supplied rather than guessed: inferring them from table names
// would split hotel_rooms and hotels into two modules, and a wrong split reads
// as a real result.
func printByModule(out report, spec, mapFile string, results []tableResult) {
	// An exact mapping wins where it is given. The prefix convention is the
	// right default and this is its escape: grandfathered bare names are
	// permanent in the repos that have them, so a survey that can only group by
	// prefix cannot describe those repos at all (#122).
	if strings.TrimSpace(mapFile) != "" {
		printByMapping(out, mapFile, results)
		return
	}
	if strings.TrimSpace(spec) == "" {
		return
	}
	prefixes := make([]string, 0)
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return
	}
	// Longest prefix wins, so a module named "user" does not claim the tables
	// of one named "user_billing".
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })

	type mod struct{ clean, partial, refused int }
	mods := map[string]*mod{}
	for _, p := range prefixes {
		mods[p] = &mod{}
	}
	unclaimed := &mod{}
	var unclaimedNames []string

	for _, r := range results {
		m, name := unclaimed, ""
		for _, p := range prefixes {
			if strings.HasPrefix(r.Name, p+"_") {
				m, name = mods[p], p
				break
			}
		}
		switch {
		case r.Err != "":
			m.refused++
		case len(r.Skips) > 0:
			m.partial++
		default:
			m.clean++
		}
		if name == "" {
			unclaimedNames = append(unclaimedNames, r.Name)
		}
	}

	out.printf("### By module\n\n")
	out.printf("| module | tables | clean | partial | refused | gate |\n")
	out.printf("|---|---:|---:|---:|---:|---|\n")
	sort.Strings(prefixes)
	// Groups, not prefixes: the no-prefix row is a group that can be blocked,
	// so counting it in the numerator and not the denominator produced "3 of 3
	// modules blocked" over a table with a green row in it. Shared tables
	// blocking everything is the finding most worth keeping visible, so the
	// row stays in both halves of the fraction rather than being dropped from
	// the numerator.
	groups, blocked := len(prefixes), 0
	for _, p := range prefixes {
		m := mods[p]
		total := m.clean + m.partial + m.refused
		gate := "green"
		if m.partial+m.refused > 0 {
			gate = "**blocked**"
			blocked++
		}
		out.printf("| %s | %d | %d | %d | %d | %s |\n", p, total, m.clean, m.partial, m.refused, gate)
	}
	if n := unclaimed.clean + unclaimed.partial + unclaimed.refused; n > 0 {
		groups++
		gate := "green"
		if unclaimed.partial+unclaimed.refused > 0 {
			gate = "**blocked**"
			blocked++
		}
		// Named for what it is rather than for what it might be. The row used
		// to read "_(no prefix)_", which a reader completes as "the shared
		// core" — and a monolith with a large shared core is entirely
		// believable, so nothing prompted anyone to doubt it (#122).
		out.printf("| _unmatched — NOT a module_ | %d | %d | %d | %d | %s |\n",
			n, unclaimed.clean, unclaimed.partial, unclaimed.refused, gate)
	}
	out.printf("\n**%d of %d modules blocked.**\n\n", blocked, groups)
	if len(unclaimedNames) > 0 {
		sort.Strings(unclaimedNames)
		total := len(results)
		unmatched := len(unclaimedNames)
		out.printf("### Unmatched tables\n\n")
		out.printf("**%d of %d tables (%d%%) matched no prefix in -modules.**\n\n",
			unmatched, total, percent(unmatched, total))
		// A threshold, because the failure this guards against is silent and
		// the wrong answer is plausible. Below it, unmatched tables are the
		// ordinary shared core and grandfathered names every real monolith
		// has; above it, the likeliest explanation is a wrong or incomplete
		// flag value, and a reader who is told so checks it.
		if percent(unmatched, total) >= unmatchedWarnPercent {
			out.printf("> **Check the flag before reading this as a finding.** More than %d%% of the\n"+
				"> schema matched nothing, which is more often an incomplete -modules value than\n"+
				"> a genuinely large shared core. Grandfathered bare names are the other cause,\n"+
				"> and they are frozen legacy rather than shared — the two are not distinguishable\n"+
				"> from a prefix, so this needs an answer from the repository, not from the survey.\n\n",
				unmatchedWarnPercent)
		}
		out.printf("%s\n\n", strings.Join(unclaimedNames, ", "))
	}
}

// unmatchedWarnPercent is where an unmatched set stops reading as a shared core
// and starts reading as a wrong flag. Chosen from the two corpora that produced
// issue #122: 43 of 68 tables unmatched was a wrong flag value, and 16 of 30 was
// a real set of permanently grandfathered names. Both are above it, which is the
// point — the warning asks a question rather than answering one.
const unmatchedWarnPercent = 25

func percent(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

func lenSkips(r *introspect.Report) int {
	if r == nil {
		return 0
	}
	return len(r.Skipped)
}

func findTable(reg *schema.Registry, name string) *schema.TableDef {
	for _, t := range reg.Tables() {
		if t.Name() == name || t.LocalName() == name {
			return t
		}
	}
	return nil
}

func printByReason(out report, skips []introspect.Skip) {
	if len(skips) == 0 {
		return
	}
	by := map[string][]introspect.Skip{}
	for _, s := range skips {
		by[s.Reason] = append(by[s.Reason], s)
	}
	reasons := keys(by)
	sort.Slice(reasons, func(i, j int) bool { return len(by[reasons[i]]) > len(by[reasons[j]]) })
	for _, r := range reasons {
		out.printf("### [%d] %s\n\n", len(by[r]), oneline(r))
		for _, s := range by[r] {
			where := s.Table
			if s.Object != "" {
				where += "." + s.Object
			}
			out.printf("  - %-50s %s\n", where, oneline(s.Def))
		}
		out.printf("\n")
	}
}

// kindOf reduces a change comment to its verb so residuals can be counted by
// shape rather than listed one by one.
func kindOf(comment string) string {
	f := strings.Fields(comment)
	if len(f) >= 2 {
		return f[0] + " " + f[1]
	}
	if len(f) == 1 {
		return f[0]
	}
	return "(unlabelled)"
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listTables(ctx context.Context, db *pgxpool.Pool, skip []string) ([]string, error) {
	// A nil slice binds as SQL NULL, and `tablename <> ALL(NULL)` is NULL
	// rather than true — so a nil skip list would match no rows at all.
	if skip == nil {
		skip = []string{}
	}
	rows, err := db.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> ALL($1)
		ORDER BY tablename`, skip)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan table: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// selectExcluded expands the wanted patterns against the tables the database
// actually has.
//
// Two jobs in one pass. It narrows, because introspect reports a name in
// Exclude that it does not find — deliberately, since a typo would otherwise
// silently shrink what a gate checks. And it expands, because a pattern is the
// only workable spelling for a runner used once per module: a goose-per-module
// monolith carries 11 to 17 bookkeeping tables and none of them matches a
// built-in name (#123).
func selectExcluded(want, have []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range have {
		for _, w := range want {
			if !matchPattern(w, h) || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// matchPattern reports whether name matches pattern, where % stands for any run
// of characters.
//
// SQL's wildcard rather than the shell's, because the pattern this exists for
// is one a caller would otherwise have had to write as a LIKE against
// information_schema to discover the table names by hand. A pattern with no %
// is an exact name, which is what every caller before #123 wrote.
func matchPattern(pattern, name string) bool {
	if !strings.Contains(pattern, "%") {
		return pattern == name
	}
	parts := strings.Split(pattern, "%")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	rest := name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, seg := range parts[1 : len(parts)-1] {
		k := strings.Index(rest, seg)
		if k < 0 {
			return false
		}
		rest = rest[k+len(seg):]
	}
	return strings.HasSuffix(rest, last)
}

// printByMapping is printByModule over an exact table-to-module mapping.
//
// Same table, same verdict, same blocked count — the only thing that changes is
// how a table finds its module, and therefore what "unmatched" means. Here an
// unmatched table is one the caller left out of the file, which is a fact about
// the file and worth saying plainly rather than warning about.
func printByMapping(out report, path string, results []tableResult) {
	raw, err := os.ReadFile(path)
	if err != nil {
		out.printf("**-modules-file could not be read:** %s\n\n", oneline(err.Error()))
		return
	}
	var mapping map[string][]string
	if err := json.Unmarshal(raw, &mapping); err != nil {
		out.printf("**-modules-file %s is not valid JSON:** %s\n\n", path, oneline(err.Error()))
		return
	}
	if len(mapping) == 0 {
		out.printf("**-modules-file %s lists no modules.**\n\n", path)
		return
	}

	owner := map[string]string{}
	for mod, tables := range mapping {
		for _, t := range tables {
			if prev, dup := owner[t]; dup {
				out.printf("**-modules-file %s lists %q under both %q and %q**, so the grouping "+
					"would mean nothing.\n\n", path, t, prev, mod)
				return
			}
			owner[t] = mod
		}
	}

	type mod struct{ clean, partial, refused int }
	mods := map[string]*mod{}
	for name := range mapping {
		mods[name] = &mod{}
	}
	unlisted := &mod{}
	var unlistedNames []string

	for _, r := range results {
		m := unlisted
		if name, ok := owner[r.Name]; ok {
			m = mods[name]
		} else {
			unlistedNames = append(unlistedNames, r.Name)
		}
		switch {
		case r.Err != "":
			m.refused++
		case len(r.Skips) > 0:
			m.partial++
		default:
			m.clean++
		}
	}

	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	sort.Strings(names)

	out.printf("### By module (exact mapping from %s)\n\n", path)
	out.printf("| module | tables | clean | partial | refused | gate |\n")
	out.printf("|---|---:|---:|---:|---:|---|\n")
	blocked := 0
	for _, name := range names {
		m := mods[name]
		total := m.clean + m.partial + m.refused
		gate := "green"
		if m.partial+m.refused > 0 {
			gate = "**blocked**"
			blocked++
		}
		out.printf("| %s | %d | %d | %d | %d | %s |\n", name, total, m.clean, m.partial, m.refused, gate)
	}
	out.printf("\n**%d of %d modules blocked.**\n\n", blocked, len(names))

	// A module in the file with no tables in the database is a stale mapping,
	// and it reads as a green module — the one way this grouping can quietly
	// overstate how much is adoptable.
	var empty []string
	for _, name := range names {
		if m := mods[name]; m.clean+m.partial+m.refused == 0 {
			empty = append(empty, name)
		}
	}
	if len(empty) > 0 {
		out.printf("**%d module(s) in the mapping have no tables in this database** — stale entries, "+
			"and they count as green above: %s\n\n", len(empty), strings.Join(empty, ", "))
	}
	if len(unlistedNames) > 0 {
		sort.Strings(unlistedNames)
		out.printf("### Not in the mapping\n\n")
		out.printf("**%d of %d tables (%d%%) are absent from %s** and are counted in no module:\n\n%s\n\n",
			len(unlistedNames), len(results), percent(len(unlistedNames), len(results)), path,
			strings.Join(unlistedNames, ", "))
	}
}

func open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func oneline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
