package codegen

// The floor under `sqlb check`'s advisory block, and the reason there is one.
//
// Lint is wired into check (#201) because a diagnostic nobody runs is a
// diagnostic nobody reads. What #267 then found on a fourteen-table schema is
// the other end of the same problem: 102 advisory lines, and the one line the
// command was run for at the bottom of them. Both times the reporter piped the
// output through grep to find the verdict, which is the tell — the first thing
// an adopter does with the advisories is discard all of them, because there is
// no way to discard only the ones they have already decided about.
//
// So the detail is behind a flag and the count is not. A schema that has read
// its info notes once keeps the warn ones with -lint=warn; a schema that wants
// the wall back asks for it with -lint=all; and the default says how many of
// each there are in one line, which is enough to notice the number moving
// without being enough to bury the verdict under it.
//
// Silence is not one of the levels by default, deliberately. -lint=off exists
// for a project that has made its decision, but a project that has not made one
// should not get there by never being told there was something to decide.

import (
	"fmt"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// lintLevel is how much of the lint result `check` prints.
type lintLevel string

const (
	// lintOff prints nothing at all, not even the count.
	lintOff lintLevel = "off"
	// lintSummary prints one line of counts. The default.
	lintSummary lintLevel = "summary"
	// lintWarn lists the warn-level diagnostics and counts the rest.
	lintWarn lintLevel = "warn"
	// lintAll lists everything.
	lintAll lintLevel = "all"
)

// lintLevelUsage is the -lint flag's help text.
const lintLevelUsage = "how much of the lint result to print: " +
	"off, summary (counts only), warn (list the warnings), or all"

// parseLintLevel reads the -lint flag value, naming what would have been
// accepted when it cannot — the same courtesy every other rejection in sqlb
// owes its reader (docs/architecture.md, "Actionable errors").
func parseLintLevel(s string) (lintLevel, error) {
	switch l := lintLevel(s); l {
	case lintOff, lintSummary, lintWarn, lintAll:
		return l, nil
	default:
		return "", fmt.Errorf("unknown -lint level %q: want off, summary, warn or all", s)
	}
}

// listed is the diagnostics this level prints in full.
func (l lintLevel) listed(ds schema.Diagnostics) schema.Diagnostics {
	switch l {
	case lintAll:
		return ds
	case lintWarn:
		return ds.Warnings()
	default:
		return nil
	}
}

// summary is the one line that survives every level but off: how many
// diagnostics there are, by severity, and how to see the ones this level did
// not print. It is empty when there is nothing to say.
func (l lintLevel) summary(ds schema.Diagnostics) string {
	if l == lintOff || len(ds) == 0 {
		return ""
	}

	warn := len(ds.Warnings())
	info := len(ds) - warn
	var counts []string
	if warn > 0 {
		counts = append(counts, fmt.Sprintf("%d warn", warn))
	}
	if info > 0 {
		counts = append(counts, fmt.Sprintf("%d info", info))
	}

	s := "sqlb: lint: " + strings.Join(counts, ", ")
	switch {
	case l == lintAll, l == lintWarn && info == 0:
		// Everything there is to see has already been printed above.
	case l == lintWarn:
		s += " — the info ones are behind -lint=all"
	case warn > 0:
		s += " — run `sqlb check -lint=warn` to list the warnings, -lint=all for every one"
	default:
		s += " — run `sqlb check -lint=all` to list them"
	}
	return s
}
