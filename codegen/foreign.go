package codegen

import "github.com/mind-vm/sqlb/schema"

// A table another package already generates the model for is migrated by this
// schema and emitted by nothing in it (#284).
//
// The arrangement this exists for is a library declaring its tables into the
// host's registry, which is what lets a host's column carry a real foreign key
// into a library's table. Everything downstream of the registry then saw those
// tables, so the host's generated package gained a row struct for each — and
// since hooks are keyed by Go type, a query written against the host's copy ran
// with no hook at all. Same table, same rows, no confinement, no error.
//
// The filter is derived from a declaration rather than configured, because a
// list of tables to skip is a second copy of the library's table set: it is
// right the day it is written and silently wrong the release the library adds
// a table. See [schema.TableDef.ModelsIn].

// owns reports whether this run is the one that emits code for t.
//
// A table naming no package is owned by whoever generates it, which is every
// schema that does not use the feature — so this answers true and nothing
// changes for them. A table naming *this* package is the library generating its
// own models, which is the one run that must emit them.
func owns(opts Options, t *schema.TableDef) bool {
	pkg := t.ModelsPackage()
	return pkg == "" || pkg == opts.Package
}

// ownTables is opts.Registry.Tables() less the tables another package owns.
//
// Every Go and client emitter goes through this rather than through the
// registry, so that one of them cannot keep emitting a type the others have
// stopped referring to — which would be a package that does not compile rather
// than the shadow this removes.
func ownTables(opts Options) []*schema.TableDef {
	return ownOnly(opts, opts.Registry.Tables())
}

// ownTablesFor is ownTables for a caller carrying its own options struct.
// `sqlb eject` has one, and the exit it writes has the same reason to leave a
// library's tables alone: the whole point of ejecting is to hand over code that
// compiles and behaves as the generated code did.
func ownTablesFor(r *schema.Registry, pkg string) []*schema.TableDef {
	return ownOnly(Options{Package: pkg}, r.Tables())
}

// ownViews is ownTables for views, which are enumerated separately because a
// view has no DDL machinery of its own (see [schema.Registry.Views]).
func ownViews(opts Options) []*schema.TableDef {
	return ownOnly(opts, opts.Registry.Views())
}

func ownOnly(opts Options, in []*schema.TableDef) []*schema.TableDef {
	out := make([]*schema.TableDef, 0, len(in))
	for _, t := range in {
		if owns(opts, t) {
			out = append(out, t)
		}
	}
	return out
}

// dropForeignFromManifest removes the tables another package models.
//
// The manifest documents what this package serves, and this one serves nothing
// for a table it emits no model, no columns facade and no mount for. Left in,
// it would advertise a resource that is not there — and the agent skill, which
// is built from the same manifest, would offer a model the package does not
// have.
//
// Filtered here rather than in BuildManifest, because ownership is a fact about
// the generation run and not about the registry: the same registry produces the
// library's manifest, where these tables belong, and the host's, where they do
// not.
func dropForeignFromManifest(m *schema.Manifest, opts Options) {
	keep := make(map[string]bool, len(m.Tables))
	for _, t := range ownTables(opts) {
		keep[t.Name()] = true
	}
	for _, v := range ownViews(opts) {
		keep[v.Name()] = true
	}
	out := m.Tables[:0]
	for _, t := range m.Tables {
		if keep[t.Name] {
			out = append(out, t)
		}
	}
	m.Tables = out
}
