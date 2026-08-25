package codegen

// `sqlb impact` is the API-compatibility counterpart of `sqlb migrate`. Migrate
// asks whether a schema edit is safe for the *database*; impact asks whether it
// is safe for the *deployed REST client* — and the two answers differ, often
// invert, because a clean RENAME is a wire break and an un-exposed column is a
// break with no DDL at all (ADR-0039).
//
// It works like every other verb: the driver has already linked the schema
// package in, so opts.Registry holds the current contract. The other side of
// the diff is a snapshot file committed to the repository, which is what makes
// "backward compatible relative to what?" a concrete, reviewable answer rather
// than a guess. `-write` records that baseline; a bare run states the delta;
// `-error` turns a breaking delta into a non-zero exit for CI.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// impactFlags is the verb's command line.
type impactFlags struct {
	write  bool
	strict bool
}

func parseImpactFlags(args []string, stderr io.Writer) (*impactFlags, error) {
	f := new(impactFlags)
	fs := flag.NewFlagSet("sqlb impact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&f.write, "write", false, "record the current REST contract as the new baseline")
	fs.BoolVar(&f.strict, "error", false, "exit non-zero if the contract has breaking changes")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("impact takes no positional arguments, got %q", fs.Arg(0))
	}
	if f.write && f.strict {
		return nil, fmt.Errorf("-write and -error are mutually exclusive: one records a baseline, the other checks against it")
	}
	return f, nil
}

// contractPath resolves where the snapshot lives, defaulting to a file beside
// the generated code so a project that never sets it still works.
func contractPath(p Project, opts Options) string {
	if p.ContractFile != "" {
		return p.ContractFile
	}
	return filepath.Join(opts.Dir, "restcontract.json")
}

// runImpact records or checks the REST contract snapshot.
func runImpact(p Project, opts Options, reg *schema.Registry, args []string, stdout, stderr io.Writer) int {
	f, err := parseImpactFlags(args, stderr)
	if err != nil {
		// flag has already printed the usage and the reason.
		return 2
	}

	current := restcompat.Capture(reg)
	path := contractPath(p, opts)

	if f.write {
		if err := writeSnapshot(path, current); err != nil {
			line(stderr, err)
			return 1
		}
		say(stderr, "sqlb: recorded the REST contract baseline at %s\n", path)
		return 0
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		say(stderr, "sqlb: no contract baseline at %s to compare against.\n", path)
		line(stderr, "sqlb: record the current contract as the baseline with: sqlb impact -write")
		return 2
	}
	if err != nil {
		line(stderr, err)
		return 1
	}
	var old restcompat.Snapshot
	if err := json.Unmarshal(raw, &old); err != nil {
		return snapshotUnreadable(stderr, path, err)
	}
	if old.Version != restcompat.SnapshotVersion {
		say(stderr, "sqlb: the contract baseline at %s is format version %d, and this sqlb writes version %d.\n",
			path, old.Version, restcompat.SnapshotVersion)
		line(stderr, "sqlb: re-record it with: sqlb impact -write")
		return 2
	}

	breaks := restcompat.DiffSnapshots(old, current)
	if len(breaks) == 0 {
		line(stderr, "sqlb: the REST contract is unchanged")
		return 0
	}

	// The report goes to stdout so it can be captured; the summary and the
	// verdict go to stderr, the way `check` splits its output.
	for _, b := range breaks {
		line(stdout, b.String())
	}
	breaking := restcompat.Breaking(breaks)
	say(stderr, "sqlb: %d contract change(s), %d breaking\n", len(breaks), len(breaking))

	if len(breaking) == 0 {
		return 0
	}
	// Breaking changes exist. Whether that fails the command is the caller's
	// policy: state by default, gate with -error (ADR-0039).
	line(stderr, "sqlb: the change is intended? re-record the baseline with: sqlb impact -write")
	if f.strict {
		return 1
	}
	return 0
}

// writeSnapshot marshals a snapshot to path, creating the directory if needed
// and ending the file with a newline so it is a well-behaved committed file.
func writeSnapshot(path string, s restcompat.Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not encode the contract snapshot: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("could not create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", path, err)
	}
	return nil
}

func snapshotUnreadable(stderr io.Writer, path string, err error) int {
	say(stderr, "sqlb: the contract baseline at %s is not valid JSON: %v\n", path, err)
	line(stderr, "sqlb: if it is corrupt, re-record it with: sqlb impact -write")
	return 1
}
