// Package tasksevolved_test is example/tasks's second year: the same two
// tables, walked through six changes that are not additive. See
// docs/special-cases.md's "tasks-evolved" entry for the full framing, and
// README.md in this directory for what each step found.
//
// Every step diffs the previous registry against the next with migrate.Diff,
// inspects the resulting []migrate.Change before doing anything with it, and
// then — where the step says a human applies it — executes each Change.Up
// against the one Postgres connection every subtest in this file shares, so
// that data really does carry from one non-additive change to the next.
package tasksevolved_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/migrate"
	"github.com/jryannel/sqlb/schema"

	tschema "github.com/jryannel/sqlb/example/tasks-evolved/schema"
)

// apply executes every non-empty Change.Up in order against pool. It is used
// for the changes each step decides to run — never for a change a step is
// only inspecting, and never for the changes this file deliberately runs
// expecting an error, which call pool.Exec directly so the failure is
// attributed to one statement rather than buried in a loop.
func apply(t *testing.T, ctx context.Context, pool *pgxpool.Pool, changes []migrate.Change) {
	t.Helper()
	for _, c := range changes {
		up := strings.TrimSpace(c.Up)
		if up == "" {
			continue
		}
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("applying %q failed: %v\n%s", c.Comment, err, up)
		}
	}
}

// mustDiff runs migrate.Diff and fails the test on error, so every step below
// reads as "diff, then look at what came back" rather than repeating the
// same three lines of error handling six times.
func mustDiff(t *testing.T, current, target *schema.Registry) []migrate.Change {
	t.Helper()
	changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("migrate.Diff: %v", err)
	}
	return changes
}

func TestTasksEvolved(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	// --- Step 0: baseline -----------------------------------------------
	//
	// The same bootstrap every sqlb example uses: migrate.Diff(nil, v0, ...)
	// against an empty database is exactly what `sqlb migrate` would produce
	// for a schema's first migration.
	v0 := tschema.V0()
	apply(t, ctx, pool, mustDiff(t, nil, v0))

	var aliceID, bobID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name) VALUES ('Alice') RETURNING id`,
	).Scan(&aliceID); err != nil {
		t.Fatalf("seeding Alice: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name) VALUES ('Bob') RETURNING id`,
	).Scan(&bobID); err != nil {
		t.Fatalf("seeding Bob: %v", err)
	}

	seedTasks := []struct{ title, status string }{
		{"Write RFC", "todo"},
		{"Ship feature", "doing"},
		{"Cut release", "done"},
	}
	for _, s := range seedTasks {
		// labels has no default (it is declared exactly as the spec asks —
		// schema.Text("labels").Array().Filterable(), nothing else) so every
		// insert against V0 has to name it explicitly until step 4 drops the
		// column.
		if _, err := pool.Exec(ctx,
			`INSERT INTO tasks (title, status, labels) VALUES ($1, $2, '{}')`, s.title, s.status,
		); err != nil {
			t.Fatalf("seeding task %q: %v", s.title, err)
		}
	}

	// --- Step 1: rename status -> state -----------------------------------
	t.Run("step1_rename_status_to_state", func(t *testing.T) {
		// The trap first: a rename expressed as "delete the old name, declare
		// a new one" with no annotation. Diff has no way to know these two
		// columns are the same column, so it proposes exactly what the names
		// say: drop one, add the other.
		v1bad := tschema.V1Bad()
		badChanges := mustDiff(t, v0, v1bad)

		var sawDrop, sawAdd, sawRename bool
		for _, c := range badChanges {
			switch {
			case strings.Contains(c.Up, `DROP COLUMN "status"`):
				sawDrop = true
			case strings.Contains(c.Up, `ADD COLUMN "state"`):
				sawAdd = true
			case strings.Contains(c.Up, "RENAME COLUMN"):
				sawRename = true
			}
		}
		if !sawDrop || !sawAdd {
			t.Fatalf("expected a DROP COLUMN status and an ADD COLUMN state with no RenamedFrom hint, got:\n%s", renderedUp(badChanges))
		}
		if sawRename {
			t.Fatalf("did not expect a RENAME COLUMN without a RenamedFrom hint, got:\n%s", renderedUp(badChanges))
		}
		// Every value in "status" would be gone if this were applied: the add
		// has no way to recover what the drop just threw away. Confirmed by
		// reading the SQL, not by running it — running it would delete the
		// seed data the rest of this test depends on.

		// Now the fix: RenamedFrom turns the same rename into one statement
		// that keeps the data.
		v1 := tschema.V1()
		goodChanges := mustDiff(t, v0, v1)

		var renameStmt string
		for _, c := range goodChanges {
			if strings.Contains(c.Up, "RENAME COLUMN") {
				renameStmt = c.Up
			}
			if c.Destructive {
				t.Fatalf("a RenamedFrom rename should not be Destructive, got one: %s", c.Reason)
			}
		}
		if renameStmt == "" {
			t.Fatalf("expected a RENAME COLUMN statement, got:\n%s", renderedUp(goodChanges))
		}
		if !strings.Contains(renameStmt, `"status"`) || !strings.Contains(renameStmt, `"state"`) {
			t.Fatalf("rename statement does not name both columns: %s", renameStmt)
		}

		apply(t, ctx, pool, goodChanges)

		// The values themselves, not just the column's name, have to survive.
		got := map[string]string{}
		rows, err := pool.Query(ctx, `SELECT title, state FROM tasks`)
		if err != nil {
			t.Fatalf("querying renamed column: %v", err)
		}
		for rows.Next() {
			var title, state string
			if err := rows.Scan(&title, &state); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			got[title] = state
		}
		rows.Close()
		for _, s := range seedTasks {
			if got[s.title] != s.status {
				t.Errorf("task %q: state = %q, want %q (the value from before the rename)", s.title, got[s.title], s.status)
			}
		}
	})

	// --- Step 2: widen the enum --------------------------------------------
	t.Run("step2_widen_enum", func(t *testing.T) {
		v1 := tschema.V1()
		v2 := tschema.V2()

		// Before the migration: "blocked" violates the CHECK the three-value
		// enum still has.
		_, err := pool.Exec(ctx,
			`INSERT INTO tasks (title, state, labels) VALUES ('Should fail', 'blocked', '{}')`)
		if err == nil {
			t.Fatalf("expected inserting state = 'blocked' to violate the pre-widen CHECK, it did not")
		}
		t.Logf("as expected, pre-widen insert of state='blocked' failed: %v", err)

		changes := mustDiff(t, v1, v2)
		for _, c := range changes {
			if c.Destructive {
				t.Fatalf("widening an enum should not be Destructive, got one: %s", c.Reason)
			}
		}
		apply(t, ctx, pool, changes)

		// Existing rows: untouched.
		got := map[string]string{}
		rows, err := pool.Query(ctx, `SELECT title, state FROM tasks`)
		if err != nil {
			t.Fatalf("querying after widen: %v", err)
		}
		for rows.Next() {
			var title, state string
			if err := rows.Scan(&title, &state); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			got[title] = state
		}
		rows.Close()
		for _, s := range seedTasks {
			if got[s.title] != s.status {
				t.Errorf("task %q: state = %q after widening, want unchanged %q", s.title, got[s.title], s.status)
			}
		}

		// After: the same insert succeeds.
		if _, err := pool.Exec(ctx,
			`INSERT INTO tasks (title, state, labels) VALUES ('Should succeed', 'blocked', '{}')`); err != nil {
			t.Fatalf("expected state = 'blocked' to be accepted after widening, got: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM tasks WHERE title = 'Should succeed'`); err != nil {
			t.Fatalf("cleaning up probe row: %v", err)
		}
	})

	// --- Step 3: assignee_id NOT NULL, with a backfill ---------------------
	t.Run("step3_assignee_not_null_with_backfill", func(t *testing.T) {
		v2 := tschema.V2()

		// First: the shape that looks like the obvious way to add a required
		// reference. Diff against a registry that adds it directly — UUID,
		// Ref(users), NOT NULL, no default — in one step.
		v3direct := tschema.V3Direct()
		directChanges := mustDiff(t, v2, v3direct)

		var addAssignee *migrate.Change
		for i := range directChanges {
			if strings.Contains(directChanges[i].Up, `ADD COLUMN "assignee_id"`) {
				addAssignee = &directChanges[i]
				break
			}
		}
		if addAssignee == nil {
			t.Fatalf("expected an ADD COLUMN assignee_id change, got:\n%s", renderedUp(directChanges))
		}
		if !addAssignee.Destructive || addAssignee.Reason == "" {
			t.Fatalf("adding a NOT NULL column with no default should be Destructive with a Reason, got Destructive=%v Reason=%q",
				addAssignee.Destructive, addAssignee.Reason)
		}

		// Diff marks it Destructive without ever touching the database. Now
		// find out what Postgres itself does: run just that one statement
		// against the live tasks table, which already has rows.
		_, err := pool.Exec(ctx, addAssignee.Up)
		if err == nil {
			t.Fatalf("expected ADD COLUMN assignee_id ... NOT NULL to fail against a table with existing rows, it did not")
		}
		t.Logf("as expected, the direct NOT NULL add failed at apply time: %v", err)
		// Nothing was added — Postgres rejects the whole statement, so v2's
		// shape is still exactly what the database has. The two-step path
		// below starts clean.

		// Two-step path, half one: add the column nullable.
		v3nullable := tschema.V3Nullable()
		nullableChanges := mustDiff(t, v2, v3nullable)
		for _, c := range nullableChanges {
			if c.Destructive {
				t.Fatalf("adding a nullable column should not be Destructive, got one: %s", c.Reason)
			}
		}
		apply(t, ctx, pool, nullableChanges)

		// DML, not DDL: migrate renders schema, never data, so backfilling
		// every existing row onto some users row is written by hand. This is
		// the asymmetry step 3 exists to show — the ADD COLUMN above came out
		// of Diff, this UPDATE never could.
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET assignee_id = $1 WHERE assignee_id IS NULL`, aliceID,
		); err != nil {
			t.Fatalf("backfilling assignee_id: %v", err)
		}

		// Two-step path, half two: now that no row is NULL, require it.
		v3notnull := tschema.V3NotNull()
		notNullChanges := mustDiff(t, v3nullable, v3notnull)

		var setNotNull *migrate.Change
		for i := range notNullChanges {
			if strings.Contains(notNullChanges[i].Up, `SET NOT NULL`) {
				setNotNull = &notNullChanges[i]
			}
		}
		if setNotNull == nil {
			t.Fatalf("expected a SET NOT NULL change, got:\n%s", renderedUp(notNullChanges))
		}
		// Diff has no way to know the backfill above ran — it is a pure
		// function over two registries, not a database. So it marks this
		// Destructive exactly as it would if no row had been touched.
		if !setNotNull.Destructive || setNotNull.Reason == "" {
			t.Fatalf("SET NOT NULL should be Destructive with a Reason even after a backfill, got Destructive=%v Reason=%q",
				setNotNull.Destructive, setNotNull.Reason)
		}

		apply(t, ctx, pool, notNullChanges)

		var nullable string
		if err := pool.QueryRow(ctx,
			`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'assignee_id'`,
		).Scan(&nullable); err != nil {
			t.Fatalf("checking assignee_id nullability: %v", err)
		}
		if nullable != "NO" {
			t.Fatalf("assignee_id is_nullable = %q, want NO", nullable)
		}
	})

	// --- Step 4: split labels into task_labels ------------------------------
	t.Run("step4_split_labels_into_join_table", func(t *testing.T) {
		v3notnull := tschema.V3NotNull()

		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET labels = ARRAY['urgent','backend']::text[] WHERE title = 'Ship feature'`,
		); err != nil {
			t.Fatalf("seeding labels: %v", err)
		}

		// Create task_labels alongside the still-present labels array. This
		// half is ordinary Diff output: a new table is never destructive.
		v4join := tschema.V4WithJoinTable()
		joinChanges := mustDiff(t, v3notnull, v4join)
		for _, c := range joinChanges {
			if c.Destructive {
				t.Fatalf("creating task_labels should not be Destructive, got one: %s", c.Reason)
			}
		}
		apply(t, ctx, pool, joinChanges)

		// DML again: migrate has no spelling for "copy every array element
		// into a row of a different table". unnest() is the ordinary SQL
		// answer, hand-written because nothing else could have produced it.
		if _, err := pool.Exec(ctx,
			`INSERT INTO task_labels (task_id, label)
			 SELECT t.id, label FROM tasks t, unnest(t.labels) AS label
			 WHERE cardinality(t.labels) > 0`,
		); err != nil {
			t.Fatalf("copying labels into task_labels: %v", err)
		}

		var copied int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_labels`).Scan(&copied); err != nil {
			t.Fatalf("counting task_labels: %v", err)
		}
		if copied != 2 {
			t.Fatalf("task_labels has %d rows, want 2 (urgent, backend)", copied)
		}

		// Now drop the array column. Diff cannot see that the DML above
		// already preserved the data elsewhere, so this is Destructive too.
		v4final := tschema.V4Final()
		dropChanges := mustDiff(t, v4join, v4final)

		var dropLabels *migrate.Change
		for i := range dropChanges {
			if strings.Contains(dropChanges[i].Up, `DROP COLUMN "labels"`) {
				dropLabels = &dropChanges[i]
			}
		}
		if dropLabels == nil {
			t.Fatalf("expected a DROP COLUMN labels change, got:\n%s", renderedUp(dropChanges))
		}
		if !dropLabels.Destructive || dropLabels.Reason == "" {
			t.Fatalf("dropping labels should be Destructive with a Reason, got Destructive=%v Reason=%q",
				dropLabels.Destructive, dropLabels.Reason)
		}
		apply(t, ctx, pool, dropChanges)

		var labelsStillThere int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'labels'`,
		).Scan(&labelsStillThere); err != nil {
			t.Fatalf("checking labels column: %v", err)
		}
		if labelsStillThere != 0 {
			t.Fatalf("tasks.labels still exists after the drop")
		}

		// The join table's data is what is left, and it should still match
		// what the array held.
		rows, err := pool.Query(ctx,
			`SELECT label FROM task_labels tl JOIN tasks t ON t.id = tl.task_id WHERE t.title = 'Ship feature' ORDER BY label`)
		if err != nil {
			t.Fatalf("querying task_labels: %v", err)
		}
		var labels []string
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				t.Fatalf("scanning label: %v", err)
			}
			labels = append(labels, l)
		}
		rows.Close()
		want := []string{"backend", "urgent"}
		if len(labels) != len(want) || labels[0] != want[0] || labels[1] != want[1] {
			t.Fatalf("task_labels for 'Ship feature' = %v, want %v", labels, want)
		}
	})

	// --- Step 5: drop priority, a column something else still names --------
	t.Run("step5_drop_priority", func(t *testing.T) {
		v4final := tschema.V4Final()
		v5 := tschema.V5()

		changes := mustDiff(t, v4final, v5)

		var dropPriority *migrate.Change
		for i := range changes {
			if strings.Contains(changes[i].Up, `DROP COLUMN "priority"`) {
				dropPriority = &changes[i]
			}
		}
		if dropPriority == nil {
			t.Fatalf("expected a DROP COLUMN priority change, got:\n%s", renderedUp(changes))
		}
		if !dropPriority.Destructive || dropPriority.Reason == "" {
			t.Fatalf("dropping priority should be Destructive with a Reason, got Destructive=%v Reason=%q",
				dropPriority.Destructive, dropPriority.Reason)
		}

		mig := migrate.Migration{
			Version: migrate.SequentialVersion(5),
			Name:    "drop_priority",
			Changes: changes,
		}

		// Default: rendered commented out. This is the gate the full doc
		// entry's "a client generated one commit ago" scenario is really
		// about — a destructive change does not go live because a generator
		// decided it should. This lean module has no generated TypeScript
		// client to check the drop against (docs/special-cases.md's tasks-
		// evolved entry imagines one, generated one commit before the drop);
		// what is left, and what is checked here, is the mechanism that
		// would stop such a client from being broken silently: the statement
		// does not run until a human uncomments it.
		defaultFiles, err := migrate.Render(mig, migrate.Options{})
		if err != nil {
			t.Fatalf("rendering with defaults: %v", err)
		}
		defaultBody := oneFile(t, defaultFiles)
		if !strings.Contains(defaultBody, "DESTRUCTIVE") {
			t.Fatalf("expected the default render to mark the change DESTRUCTIVE, got:\n%s", defaultBody)
		}
		if !strings.Contains(defaultBody, `-- ALTER TABLE "tasks" DROP COLUMN "priority";`) {
			t.Fatalf("expected the DROP COLUMN to be commented out by default, got:\n%s", defaultBody)
		}
		if strings.Contains(defaultBody, "\nALTER TABLE \"tasks\" DROP COLUMN \"priority\";") {
			t.Fatalf("the DROP COLUMN must not appear live in the default render:\n%s", defaultBody)
		}

		// AllowDestructive: the same statement, live.
		allowedFiles, err := migrate.Render(mig, migrate.Options{AllowDestructive: true})
		if err != nil {
			t.Fatalf("rendering with AllowDestructive: %v", err)
		}
		allowedBody := oneFile(t, allowedFiles)
		if !strings.Contains(allowedBody, `ALTER TABLE "tasks" DROP COLUMN "priority";`) {
			t.Fatalf("expected the DROP COLUMN live under AllowDestructive, got:\n%s", allowedBody)
		}
		if strings.Contains(allowedBody, `-- ALTER TABLE "tasks" DROP COLUMN "priority";`) {
			t.Fatalf("did not expect the DROP COLUMN commented out under AllowDestructive, got:\n%s", allowedBody)
		}

		// Apply it directly, to keep the one live database moving into step
		// 6 — a choice this test makes, not something Diff or Render did for
		// it. The point above is already made; this is bookkeeping.
		apply(t, ctx, pool, changes)
	})

	// --- Step 6: partial unique index against data that violates it --------
	t.Run("step6_partial_unique_index_against_violating_data", func(t *testing.T) {
		v5 := tschema.V5()
		v6 := tschema.V6()

		var shipFeatureAssignee string
		if err := pool.QueryRow(ctx,
			`SELECT assignee_id FROM tasks WHERE title = 'Ship feature'`,
		).Scan(&shipFeatureAssignee); err != nil {
			t.Fatalf("reading Ship feature's assignee: %v", err)
		}
		// 'Ship feature' is already state = 'doing' from the seed data. A
		// second row, same assignee, same state, is the violation.
		if _, err := pool.Exec(ctx,
			`INSERT INTO tasks (title, state, assignee_id) VALUES ('Fix bug', 'doing', $1)`,
			shipFeatureAssignee,
		); err != nil {
			t.Fatalf("seeding the conflicting row: %v", err)
		}

		changes := mustDiff(t, v5, v6)
		var createIndex *migrate.Change
		for i := range changes {
			if strings.Contains(changes[i].Up, "CREATE UNIQUE INDEX") {
				createIndex = &changes[i]
			}
		}
		if createIndex == nil {
			t.Fatalf("expected a CREATE UNIQUE INDEX change, got:\n%s", renderedUp(changes))
		}
		if createIndex.Destructive {
			t.Fatalf("a new index is not Destructive in this package's sense (nothing is lost); got Destructive=true")
		}
		// An index added to a table that already has rows is always rendered
		// CONCURRENTLY (migrate/diff.go's indexCreated: "the table already
		// holds rows, so building the index without CONCURRENTLY would lock
		// it against writes") — which sharpens the point of this step. A
		// plain CREATE INDEX that fails rolls back atomically and leaves
		// nothing behind; CONCURRENTLY cannot, because it builds outside a
		// single transaction, so a failed build leaves an INVALID index
		// occupying the name.
		if !strings.Contains(createIndex.Up, "CONCURRENTLY") {
			t.Fatalf("expected the index build to be CONCURRENTLY, got: %s", createIndex.Up)
		}

		// Diff never touched the database, so it has no way to know two rows
		// already violate this. Applying it is where that surfaces.
		_, err := pool.Exec(ctx, createIndex.Up)
		if err == nil {
			t.Fatalf("expected CREATE UNIQUE INDEX to fail against violating data, it did not")
		}
		t.Logf("as expected, the partial unique index failed against live data: %v", err)

		// Confirmed by trying immediately again: a clean retry would fail
		// with the same duplicate-key error; what it actually gets is
		// "already exists", from the invalid index the failed build left
		// behind — a worse error to hand an operator mid-incident, since it
		// no longer names the real problem.
		if _, err := pool.Exec(ctx, createIndex.Up); err == nil {
			t.Fatalf("expected a second attempt, before cleanup, to fail on the leftover invalid index")
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf(`expected "already exists" from the leftover invalid index, got: %v`, err)
		}

		// The same Change carries what removes it: Down for a concurrent
		// index build is DROP INDEX CONCURRENTLY, which is exactly the
		// cleanup an invalid index needs.
		if _, err := pool.Exec(ctx, createIndex.Down); err != nil {
			t.Fatalf("dropping the invalid index (%s): %v", createIndex.Down, err)
		}

		// Now fix the data — move one of the two rows out of 'doing' — and
		// reapply the exact same statement Diff proposed. This time nothing
		// is in the way and nothing violates it.
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET state = 'done' WHERE title = 'Fix bug'`,
		); err != nil {
			t.Fatalf("fixing the conflicting row: %v", err)
		}
		if _, err := pool.Exec(ctx, createIndex.Up); err != nil {
			t.Fatalf("CREATE UNIQUE INDEX still failed after dropping the invalid attempt and fixing the data: %v", err)
		}
	})
}

// renderedUp joins every change's Up SQL, for a failure message that shows
// what Diff actually proposed.
func renderedUp(changes []migrate.Change) string {
	var b strings.Builder
	for _, c := range changes {
		b.WriteString("-- ")
		b.WriteString(c.Comment)
		b.WriteString("\n")
		b.WriteString(c.Up)
		b.WriteString("\n")
	}
	return b.String()
}

// oneFile returns the single file Render produced, failing loudly if it
// produced more or fewer than one — a Migration this small should never
// split.
func oneFile(t *testing.T, files map[string]string) string {
	t.Helper()
	if len(files) != 1 {
		var names []string
		for name := range files {
			names = append(names, name)
		}
		t.Fatalf("expected exactly one rendered file, got %d: %v", len(files), names)
	}
	for _, body := range files {
		return body
	}
	return ""
}
