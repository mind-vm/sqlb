package migrate

// This file is about one question: which Postgres will the generated migration
// actually run on?
//
// Almost nothing here needs to ask. The DDL sqlb emits is ordinary SQL that has
// been valid for a decade. The exception is a default whose *generator* arrived
// in a particular release, where writing the modern spelling produces a
// migration that fails on an older server, and writing the old one produces a
// migration that needs an extension installed.
//
// Note where this lives. Options, which Render and Write take, is too late:
// by then the SQL is a string and the choice has already been made. So this is
// an option on Diff, which is what turns two registries into statements.

import "github.com/jryannel/sqlb/schema"

// Option configures Diff.
type Option func(*diffOptions)

type diffOptions struct {
	// minPG is the oldest Postgres major version the output must run on, or 0
	// when the caller has not said.
	minPG int
}

// MinPostgres declares the oldest Postgres major version the generated
// migration has to run on, which lets the DDL layer use a built-in where one
// exists instead of requiring an extension.
//
// Today it changes exactly one thing. schema.GenUUIDv7 emits
// uuid_generate_v7(), which is the pg_uuidv7 extension's spelling — so a
// migration for a UUIDv7 primary key does not apply to a stock Postgres at all.
// Postgres 18 has uuidv7() built in, and MinPostgres(18) emits that instead.
//
// Unset means the old spelling, which is the behaviour every migration
// generated before this option existed already has. A default that silently
// changed emitted DDL would be the one mistake ADR-0014 says is not recoverable
// by regenerating.
//
// Pass it consistently across a project. Generating one migration with it and
// the next without leaves a table whose columns default through two different
// spellings of the same generator — harmless to the database, confusing to
// read, and a diff will not flag it because both import back to the same
// schema.GenUUIDv7.
func MinPostgres(major int) Option {
	return func(o *diffOptions) { o.minPG = major }
}

// resolve returns the spelling to emit for a raw default expression.
//
// The table is schema.TargetDefaults rather than one of this package's own.
// It used to live here, which was defensible while this was the only code that
// read it; it stopped being so once schema.Registry.Lint needed the same pairs
// to recognise a column that had spelled a target's answer out by hand with
// schema.Expr (#293). Two copies of "uuidv7() is what GenUUIDv7 renders from 18
// onward" is one copy too many — a lint rule that drifted from this function
// would advise against SQL this function itself emits.
//
// Anything unrecognised is returned untouched: schema.Expr takes arbitrary SQL,
// and rewriting something this does not understand is exactly the guessing this
// project refuses elsewhere.
func (o diffOptions) resolve(raw string) string {
	for _, b := range schema.TargetDefaults() {
		if raw == b.Canonical && o.minPG >= b.Since {
			return b.Builtin
		}
	}
	return raw
}
