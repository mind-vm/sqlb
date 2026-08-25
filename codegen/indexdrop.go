package codegen

// Three different situations produce one output.
//
// `DROP INDEX CONCURRENTLY "messages_correlation_id_idx";` is what sqlb emits
// for an index the author has deliberately stopped declaring, for an index
// somebody built by hand that the declaration never knew about, and — for one
// migration, once, after upgrading past v0.14 — for an index sqlb used to
// create by implication from a reference and no longer does (#259). The DDL is
// byte-identical in all three, it applies cleanly in all three, and only in the
// first is the loss intended (#268).
//
// The recovery cost is what makes this worth a note rather than a shrug:
// reading one extra line costs seconds, and rebuilding a dropped index on a
// large table under CONCURRENTLY does not.
//
// Two signals are available without asking the operator anything, and they are
// independent:
//
//   - Provenance. `check` already reads MigrationsDir to see whether a
//     header-bearing file has had DDL pasted into it (#178, provenance.go). The
//     same scan answers a different question: did any sqlb-generated migration
//     ever create this index? If none did, it was made by hand or by another
//     tool, and nothing in the declaration will put it back.
//
//   - Shape. A single-column non-unique index over a column the declaration
//     still calls a reference is exactly what v0.14 inferred, and the fix if it
//     is wanted is one word — `.Indexed()` on the ref — rather than a migration
//     written by hand to undo the drop.
//
// Neither signal fires on the ordinary case: sqlb built the index because the
// declaration asked for one, the declaration has stopped asking, and the drop
// is what the author just requested.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// createdIndexPattern matches the CREATE INDEX forms migrate/ddl.go emits —
// plain, UNIQUE, CONCURRENTLY, IF NOT EXISTS — and captures the index name,
// which is always quoted because quoteIdent quotes unconditionally.
var createdIndexPattern = regexp.MustCompile(
	`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"([^"]+)"`)

// noteIndexDrops annotates every proposed index drop that the declaration
// cannot vouch for, so the reader of the migration — or of `check`'s output —
// is told the one thing the statement cannot say about itself.
//
// current is the schema being migrated away from, which is where the index
// being dropped is described; declared is the schema being migrated to, which
// is where a reference that has lost its index would be found. Changes it has
// nothing to say about are returned unaltered.
func noteIndexDrops(changes []migrate.Change, current, declared *schema.Registry, migrationsDir string) []migrate.Change {
	var names []string
	for _, c := range changes {
		if n := c.DroppedIndex(); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return changes
	}

	built, err := indexesCreatedBySqlb(migrationsDir)
	if err != nil {
		// A directory that cannot be read is not this function's failure to
		// report: check and migrate both open it for their own reasons and
		// will say so. Losing the provenance half of the note is the right
		// degradation — the shape half still fires, and an annotation that
		// aborted a migration would be a note with more authority than it has.
		built = nil
	}

	out := make([]migrate.Change, len(changes))
	copy(out, changes)
	for i, c := range out {
		name := c.DroppedIndex()
		if name == "" {
			continue
		}
		if note := indexDropNote(name, built, current, declared, migrationsDir); note != "" {
			out[i].Comment = strings.TrimSpace(c.Comment + "\n" + note)
		}
	}
	return out
}

// indexDropNote is what is worth saying above this drop, or "".
func indexDropNote(name string, built map[string]bool, current, declared *schema.Registry, migrationsDir string) string {
	var notes []string

	// Provenance. built is nil when there is no migration directory to read,
	// which is not evidence of anything — a project generating its migrations
	// elsewhere has a history this cannot see, and a note claiming otherwise
	// would be wrong rather than cautious.
	if built != nil && !built[name] {
		notes = append(notes, fmt.Sprintf(
			"note: no sqlb-generated migration in %s ever created this index, so it was "+
				"built by hand or by another tool and nothing in the declaration will put it back",
			migrationsDir))
	}

	// Shape. The v0.14 inference's index, still unclaimed by the declaration.
	if table, column := unindexedRefBehind(name, current, declared); column != "" {
		notes = append(notes, fmt.Sprintf(
			"note: this indexes %s.%s, which the declaration still calls a reference and no "+
				"declared index covers — sqlb v0.14 and earlier created this index by implication "+
				"and v0.15 does not (#259). If it is wanted, declare it with .Indexed() on the "+
				"reference rather than letting this drop it",
			table, column))
	}

	return strings.Join(notes, "\n")
}

// unindexedRefBehind reports the table and column of the reference an index is
// the only cover for, when the index is shaped like the one v0.14 inferred and
// the declaration has not replaced it. It returns "" otherwise.
func unindexedRefBehind(name string, current, declared *schema.Registry) (string, string) {
	if current == nil || declared == nil {
		return "", ""
	}
	for _, t := range current.Tables() {
		for _, idx := range t.Indexes() {
			if idx.Name != name {
				continue
			}
			// Anything with a second column, a method, a predicate or
			// uniqueness is a considered index, not the one-line implication
			// v0.14 made out of a foreign key.
			if len(idx.Columns) != 1 || idx.Unique || idx.Method != "" || idx.Where != "" {
				return "", ""
			}
			column := idx.Columns[0]

			// The declaration is what decides, not the database: the question
			// is whether dropping this leaves a reference the schema still
			// declares with nothing to seek on.
			target := declared.Get(t.Name())
			if target == nil {
				return "", ""
			}
			f := target.StoredField(column)
			if f == nil || f.Desc().Ref == nil {
				return "", ""
			}
			if leadsAnIndex(target, column) {
				return "", ""
			}
			return target.Name(), column
		}
	}
	return "", ""
}

// leadsAnIndex reports whether the declaration already gives the column
// something to seek on, which is the same rule Field.Indexed itself applies:
// leading an index, unique, or the primary key.
func leadsAnIndex(t *schema.TableDef, column string) bool {
	if f := t.StoredField(column); f != nil {
		if d := f.Desc(); d.PrimaryKey || d.Unique {
			return true
		}
	}
	for _, idx := range t.Indexes() {
		if len(idx.Columns) > 0 && idx.Columns[0] == column {
			return true
		}
	}
	return false
}

// indexesCreatedBySqlb is every index name a header-bearing migration file in
// dir creates.
//
// Header-bearing is the whole test, and it is the same one provenance.go
// makes: a file sqlb wrote claims its DDL is derived from a declaration, so an
// index in it is one sqlb built. A hand-composed file — rendered with
// migrate.Options{Handwritten: true}, or simply never touched by sqlb — is the
// case #268 actually hit, where the index was created by a migration written
// specifically to undo a phantom drop. That index is real, it is nobody's
// implication, and it is exactly the one worth naming.
//
// A missing directory is not an error, the same way historyIsEmpty treats it
// as empty rather than broken. It returns nil, which reads as "no provenance
// to be had" rather than "nothing was ever created".
func indexesCreatedBySqlb(dir string) (map[string]bool, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	built := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		content := string(body)
		if !strings.Contains(content, migrate.Header) {
			continue
		}
		// Comments are dropped for the reason provenance.go drops them: a
		// destructive change's Down is rendered as prose above the statement
		// and routinely contains the SQL it reverses, so a commented-out
		// CREATE INDEX would otherwise read as one that ran.
		for _, l := range uncommentedLines(content) {
			for _, m := range createdIndexPattern.FindAllStringSubmatch(l, -1) {
				built[m[1]] = true
			}
		}
	}
	return built, nil
}
