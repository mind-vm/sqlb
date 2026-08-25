package codegen

// `sqlb migrate`: the other half of the loop ADR-0032 opened.
//
// generate and check answer "does the committed output match the schema". This
// answers the harder one — "does the committed migration history *build* the
// schema" — and the difference is a database. The current state has to come
// from replaying the history into an empty Postgres, because reading a live one
// reports what the database looks like rather than whether the migrations
// produce it (ADR-0014, and shadow's package doc at more length).
//
// # Why this lives in codegen
//
// It makes codegen import migrate and shadow, which is a wider footprint than a
// package about emitting text wants. The alternative was a second package for
// the driver's entry point, which would either split Project away from Options
// — two imports in every project's sqlb.go instead of one — or move Main out
// from under the type it takes. Neither is worth it while this is the only
// caller: both are in-module and standard-library-only, so deps-check is
// unmoved and no consumer inherits anything. If a third verb needs a third
// subsystem, that is the signal to split.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"
)

// migrateFlags is the verb's command line.
type migrateFlags struct {
	name             string
	check            bool
	dryRun           bool
	unblock          bool
	allowDestructive bool
}

func parseMigrateFlags(args []string, stderr io.Writer) (*migrateFlags, error) {
	f := new(migrateFlags)
	fs := flag.NewFlagSet("sqlb migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.name, "name", "", "what the migration is called, which becomes part of its filename")
	fs.BoolVar(&f.check, "check", false, "report whether the schema has moved ahead of the history; write nothing")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the migration that would be written; write nothing")
	fs.BoolVar(&f.unblock, "unblock", false, "replace long-lock statements with their concurrent equivalents (migrate.Unblock)")
	fs.BoolVar(&f.allowDestructive, "allow-destructive", false, "emit destructive statements live instead of commented out")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("migrate takes no positional arguments, got %q", fs.Arg(0))
	}
	return f, nil
}

// runMigrate diffs the declared schema against the one the checked-in history
// builds, and writes what closes the gap.
func runMigrate(p Project, target *schema.Registry, args []string, stdout, stderr io.Writer) int {
	f, err := parseMigrateFlags(args, stderr)
	if err != nil {
		// flag has already printed the usage and the reason.
		return 2
	}

	if p.MigrationsDir == "" {
		line(stderr, "sqlb: this project has no MigrationsDir, so there is nowhere to write. "+
			"Set it on the Project returned by "+ProjectFunc+", or keep generating "+
			"migrations with a generator of your own")
		return 1
	}

	format, err := migrate.ByName(p.MigrationFormat)
	if err != nil {
		line(stderr, err)
		return 1
	}

	ctx := context.Background()
	current, err := currentSchema(ctx, p, format, target, stderr)
	if err != nil {
		line(stderr, err)
		return 1
	}

	var diffOpts []migrate.Option
	if p.MinPostgres > 0 {
		diffOpts = append(diffOpts, migrate.MinPostgres(p.MinPostgres))
	}
	changes, err := migrate.Diff(current, target, diffOpts...)
	if err != nil {
		line(stderr, err)
		return 1
	}

	if len(changes) == 0 {
		line(stderr, "sqlb: the history already builds the declared schema; nothing to write")
		return 0
	}
	// The first migration after upgrading past v0.14 can propose dropping a
	// real index, and the statement that does it is indistinguishable from one
	// the author asked for (#268). This is where the difference gets said.
	changes = noteIndexDrops(changes, current, target, p.MigrationsDir)

	if f.check {
		// The migration counterpart of `sqlb check`, and the reason it is a
		// flag rather than a verb of its own: what it reports is exactly the
		// changes that would be written, so sharing the code path is what
		// makes the two answers agree.
		say(stderr, "sqlb: the schema has moved ahead of the migration history by %d change(s); "+
			"run: sqlb migrate -name <what-changed>\n", len(changes))
		for _, c := range changes {
			line(stderr, "  "+summarise(c))
		}
		return 1
	}

	if f.unblock {
		changes = migrate.Unblock(changes)
	}

	name := f.name
	if f.dryRun && name == "" {
		// The name reaches nothing but the filename, and a dry run writes no
		// file — so requiring one here would be ceremony in front of the mode
		// whose entire purpose is to look before deciding.
		name = "preview"
	}

	version, err := nextVersion(p.MigrationsDir, time.Now())
	if err != nil {
		line(stderr, err)
		return 1
	}

	opts := migrate.Options{Format: format, AllowDestructive: f.allowDestructive}
	m := migrate.Migration{
		Version: version,
		Name:    name,
		Changes: changes,
	}

	if f.dryRun {
		files, err := migrate.Render(m, opts)
		if err != nil {
			line(stderr, err)
			return 1
		}
		for _, name := range sortedNames(files) {
			say(stdout, "--- %s\n%s\n", name, files[name])
		}
		say(stderr, "sqlb: %d change(s) in %d file(s); nothing written (-dry-run)\n",
			len(changes), len(files))
		return 0
	}

	if f.name == "" {
		// Required for a write and not for the other two modes, because the
		// name lands in a filename that migrate.Write refuses to overwrite
		// afterwards. "update" is not a thing you can rename later.
		line(stderr, "sqlb: migrate needs -name to say what the migration does, because it "+
			"becomes part of a filename that is append-only once applied")
		return 1
	}

	written, err := migrate.Write(p.MigrationsDir, m, opts)
	if err != nil {
		line(stderr, err)
		return 1
	}
	for _, name := range written {
		line(stdout, filepath.Join(p.MigrationsDir, name))
	}
	if m.Destructive() && !f.allowDestructive {
		// Loud, because the file looks complete and is not: the destructive
		// statements are in it as comments, and a runner will apply the rest.
		line(stderr, "sqlb: this migration contains destructive changes, which are commented "+
			"out. Read them, then uncomment what you meant — or regenerate with "+
			"-allow-destructive if you meant all of it")
	}
	say(stderr, "sqlb: wrote %d file(s) to %s\n", len(written), p.MigrationsDir)
	return 0
}

// currentSchema is the state the checked-in history builds — and, while the
// same connection is open, the place target is made comparable with it.
//
// The second job is here rather than beside the diff because it needs the
// database this function opens and closes: Postgres stores a CHECK as a parse
// tree and hands back its own spelling, so a declared check and an introspected
// one never match as strings, and the only reliable way to compare them is to
// put the declared one through the same normalisation (issue #24, and
// shadow.Normalize at length). Handing the pool back out instead would
// widen this function's contract to "and also, close this" for one caller.
//
// An empty migration directory is the baseline case — the first migration of a
// project — and it is answered without a database at all. That is worth the
// special case: it means adopting sqlb does not require standing up a scratch
// Postgres before the first `sqlb migrate` will run, and the answer is not a
// guess. An empty history replays to an empty schema, which is exactly what an
// empty registry is. Nothing needs normalising either, because every check in
// the declaration is new and nothing is being compared with it.
func currentSchema(ctx context.Context, p Project, format migrate.Format, target *schema.Registry, stderr io.Writer) (*schema.Registry, error) {
	empty, err := historyIsEmpty(p.MigrationsDir)
	if err != nil {
		return nil, err
	}
	if empty {
		line(stderr, "sqlb: no migrations yet, so this is a baseline: diffing against nothing")
		return schema.NewRegistry(), nil
	}

	if p.ShadowDB == nil {
		return nil, fmt.Errorf(
			"sqlb: %s has migrations, so the current schema has to be read by replaying "+
				"them, and this project declares no ShadowDB. Set it on the Project returned "+
				"by %s — it opens a connection to an empty scratch database, and it is a "+
				"function in your code because only your code knows which database is scratch",
			p.MigrationsDir, ProjectFunc)
	}

	db, err := p.ShadowDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlb: opening the shadow database: %w", err)
	}
	if db == nil {
		return nil, fmt.Errorf("sqlb: %s.ShadowDB returned no error and no database", ProjectFunc)
	}
	defer db.Close()

	opts := shadow.Options{
		Dir:    p.MigrationsDir,
		Format: format,
		Schema: p.PostgresSchema,
		Module: p.Module,
	}
	reg, report, res, err := shadow.Build(ctx, db, opts)
	if err != nil {
		return nil, err
	}
	say(stderr, "sqlb: replayed %d migration(s), %d statement(s)\n", len(res.Files), res.Statements)

	// The replayed tables are in the database at this point, which is what
	// makes the declared expressions probeable at all.
	unprobed, err := shadow.Normalize(ctx, db, target, opts)
	if err != nil {
		return nil, err
	}
	if len(unprobed) > 0 {
		// Not a failure. The ordinary reason a check cannot be probed is that
		// it names a column this migration is about to add, and such a check is
		// necessarily new — so the diff reports it as new either way, which is
		// the right answer. Said out loud because the alternative is a silent
		// fallback to the comparison that #24 is about.
		line(stderr, "sqlb: some declared checks could not be normalised against the "+
			"replayed schema, so they are compared as written and may show up as changed "+
			"when they are not:")
		for _, u := range unprobed {
			line(stderr, "  "+u)
		}
	}

	if report != nil && !report.Empty() {
		// Not a refusal. A trigger or a partial index the DSL cannot express
		// is a normal thing for a real history to contain, and refusing would
		// make the command useless to every project that has one. But the
		// registry it produced does not describe the database completely, so
		// the diff below is computed against a partial picture — and the case
		// that bites is a construct introspect *can* read back but the
		// declaration does not have, which reads as "drop it".
		line(stderr, "sqlb: the replayed schema contains constructs the DSL cannot express, "+
			"so `current` is incomplete and the diff below is computed against a partial "+
			"picture. Read the migration before applying it:")
		for _, l := range strings.Split(strings.TrimSpace(report.String()), "\n") {
			line(stderr, "  "+l)
		}
	}
	return reg, nil
}

// nextVersion returns a version that sorts strictly after every migration
// already in the directory.
//
// The obvious implementation — migrate.TimestampVersion(time.Now()) — is wrong,
// and wrong in a way that produces a *broken history rather than an error*.
// Goose's timestamp format has one-second resolution, so two migrations
// generated in the same second get the same version, and shadow then refuses
// the whole directory: the order they applied in is not recorded anywhere, so
// replaying them cannot be faithful. Nothing about the second `sqlb migrate`
// looks like a failure at the time; it is the next `-check` that stops working.
//
// That is not a hypothetical. It is what the first run of the end-to-end test
// did, and a project regenerating a baseline it has just deleted would hit it
// the same way.
//
// So the timestamp is a starting point and the directory is the authority: if
// the clock does not already produce something later than every version
// present, the highest one is incremented instead. Sequential histories — the
// ones written with migrate.SequentialVersion, as example/tasks is — keep
// working for the same reason, since 00002 is a smaller number than any
// timestamp and simply loses the comparison.
func nextVersion(dir string, now time.Time) (string, error) {
	version := migrate.TimestampVersion(now)
	n, err := strconv.ParseUint(version, 10, 64)
	if err != nil {
		return "", fmt.Errorf("sqlb: unparseable timestamp version %q: %w", version, err)
	}

	entries, readErr := os.ReadDir(dir)
	if errors.Is(readErr, os.ErrNotExist) {
		return version, nil
	}
	if readErr != nil {
		return "", readErr
	}

	var highest uint64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		digits := leadingDigits(e.Name())
		if digits == "" {
			// Not a migration this package wrote. Ignoring it is right: a
			// README or a Go file in the directory should not decide versions.
			continue
		}
		if v, err := strconv.ParseUint(digits, 10, 64); err == nil && v > highest {
			highest = v
		}
	}
	if n > highest {
		return version, nil
	}
	return strconv.FormatUint(highest+1, 10), nil
}

func leadingDigits(name string) string {
	for i, r := range name {
		if r < '0' || r > '9' {
			return name[:i]
		}
	}
	return name
}

// historyIsEmpty reports whether the directory holds no migrations, treating a
// directory that does not exist as one that holds none.
func historyIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			return false, nil
		}
	}
	return true, nil
}

// summarise renders one change for the -check listing.
//
// The first line of the SQL rather than the whole statement: a CREATE TABLE is
// twenty lines and the point of the list is to be countable at a glance. The
// comment is preferred where there is one, because it was written to say what
// the change is for.
func summarise(c migrate.Change) string {
	if c.Comment != "" {
		return c.Comment
	}
	first, _, _ := strings.Cut(strings.TrimSpace(c.Up), "\n")
	return strings.TrimSpace(first)
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
