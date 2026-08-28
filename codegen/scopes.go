package codegen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// The tenancy bundle's missing half (#274).
//
// A `Scoped` column already travels: it is one declaration, it obliges a hook,
// and `rest.Resource` refuses to mount without one. What could not travel is
// the hook itself. A consumer with nineteen tables wrote the same column
// declaration on sixteen of them and paired it by hand with one generic
// function registered sixteen times — two files kept aligned by care, which is
// the arrangement the "Mixins carry behaviour" decision named as the cost it
// was accepting, and its revisit trigger is a multi-tenancy bundle wanting to
// travel with its own scoping hook.
//
// The decision also settled where the carrier goes, and it is here: the schema
// package cannot register a hook, because it imports nothing from the engine
// and the generated model type does not exist when a schema is declared.
// Codegen is the only layer holding both, and what it writes is committed,
// readable and deletable — which is what makes generating behaviour acceptable
// where hiding it in the schema was not.
//
// What is emitted is the registration, not the policy. Which tenant a caller
// is in is the one thing this cannot know, so it is a func the application
// supplies — the same direction the obligation already travels in: the schema
// says these rows belong to someone, and the application says who.

// scopeField is one distinct scope column across the tables that declare it.
type scopeField struct {
	key     string             // the grouping key: the declared scope name, or the column name
	column  string             // the column name of the first table in the group
	goName  string             // the Go field on Scopes, e.g. "WorkspaceID"
	goType  string             // the columns' shared Go type, e.g. "string"
	columns map[string]string  // table name -> the column it is scoped on
	tables  []*schema.TableDef // every table in the group, in name order
}

// scopeFields groups the owned tables by the column they are scoped on.
//
// Grouped rather than one entry per table, because the resolver answers a
// question about the caller and not about the table: sixteen tables scoped on
// workspace_id want one func, which is the repetition this exists to remove.
func scopeFields(opts Options, ov *overrides) ([]scopeField, error) {
	byGroup := map[string]*scopeField{}
	for _, t := range ownTables(opts) {
		pk := t.PrimaryKey()
		for _, f := range t.Fields() {
			d := f.Desc()
			if !d.Scoped {
				continue
			}
			// A table scoped on its own primary key is skipped unless it says
			// which scope that is.
			//
			// Two reasons, and the second is why this is a skip rather than a
			// refusal. Under its own column name it would group with every
			// other table scoped on a primary key, and those are not one
			// question — a workspace confined to its own id and a user confined
			// to theirs share the name "id" and nothing else. And what the
			// emitted hook writes is `column = value`, which is the shape of a
			// reference to the tenant table and not the shape of every
			// identity scope: a user confined by a membership subquery has a
			// predicate this cannot write, and generating an equality for it
			// would be confidently wrong.
			//
			// So the default is to leave it alone, keeping the hand-written
			// hook such a table already has. Naming the scope is how one opts
			// in, and is how a table that *is* the tenant comes to share an
			// answer with the tables pointing at it.
			if d.ScopeName == "" && pk != nil && pk.Name() == d.Name {
				continue
			}
			key := d.ScopeName
			if key == "" {
				key = d.Name
			}
			goType := goType(TypeName(t), t.Name(), d, ov)
			sf, seen := byGroup[key]
			if !seen {
				byGroup[key] = &scopeField{
					key: key, column: d.Name, goName: GoName(key), goType: goType,
					columns: map[string]string{t.Name(): d.Name},
					tables:  []*schema.TableDef{t},
				}
				continue
			}
			sf.columns[t.Name()] = d.Name
			// One name, two Go types is a resolver that cannot be written: the
			// func would have to return both. Refused rather than resolved to
			// whichever table came first, which would compile and stamp one of
			// them with a converted value.
			if sf.goType != goType {
				return nil, fmt.Errorf(
					"codegen: scope %q is %s on %s and %s on %s; one scope has one type, "+
						"since one func answers for every table it confines",
					key, sf.goType, sf.tables[0].Name(), goType, t.Name())
			}
			sf.tables = append(sf.tables, t)
		}
	}
	out := make([]scopeField, 0, len(byGroup))
	for _, sf := range byGroup {
		sort.Slice(sf.tables, func(i, j int) bool { return sf.tables[i].Name() < sf.tables[j].Name() })
		out = append(out, *sf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// uncovered lists the scoped tables this file does not register hooks for, so
// the generated document does not read as complete.
//
// A generated file that looks like it covered everything is worse than one that
// covers less and says so: the reader stops looking at exactly the wrong
// moment, and here the thing they would stop looking for is a confinement.
func uncovered(opts Options, covered []scopeField) []string {
	inGroup := map[string]bool{}
	for _, sf := range covered {
		for name := range sf.columns {
			inGroup[name] = true
		}
	}
	var out []string
	for _, t := range ownTables(opts) {
		if inGroup[t.Name()] {
			continue
		}
		for _, f := range t.Fields() {
			if f.Desc().Scoped {
				out = append(out, t.Name())
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// renderScopes writes the scopes file, or nothing when no table is scoped.
func renderScopes(opts Options, ov *overrides) ([]byte, error) {
	fields, err := scopeFields(opts, ov)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		// A schema declaring no scope gets no file at all, so this is inert for
		// every project that does not use it.
		return nil, nil
	}

	b := header(opts.Package, []string{"context", "fmt", "github.com/mind-vm/sqlb"})

	renderScopesStruct(b, fields)
	renderRegisterScopes(b, fields, uncovered(opts, fields))

	return gofmt(opts.scopesFile(), b.Bytes())
}

func renderScopesStruct(b *bytes.Buffer, fields []scopeField) {
	b.WriteString("\n// Scopes carries the funcs that answer which tenant a caller is in.\n")
	b.WriteString("//\n// One field per scope column this schema declares, typed as that column is,\n")
	b.WriteString("// so the compiler demands the exact signature and a scope column added to the\n")
	b.WriteString("// schema is a build error at the call site rather than a table that silently\n")
	b.WriteString("// confines nothing. A field left nil is refused by RegisterScopes, not by the\n")
	b.WriteString("// request that would have gone unconfined.\n")
	b.WriteString("type Scopes struct {\n")
	for i, sf := range fields {
		if i > 0 {
			b.WriteString("\n")
		}
		names := make([]string, len(sf.tables))
		for j, t := range sf.tables {
			names[j] = t.Name()
		}
		fmt.Fprintf(b, "\t// %s answers which %s the caller is in. It confines %s.\n",
			sf.goName, strings.TrimSuffix(sf.column, "_id"), andList(names))
		fmt.Fprintf(b, "\t//\n\t// Returning an error refuses the statement, which is what a caller with no\n")
		fmt.Fprintf(b, "\t// tenant should get: a read answers nothing rather than everything.\n")
		fmt.Fprintf(b, "\t%s func(context.Context) (%s, error)\n", sf.goName, sf.goType)
	}
	b.WriteString("}\n")
}

func renderRegisterScopes(b *bytes.Buffer, fields []scopeField, skipped []string) {
	b.WriteString("\n// RegisterScopes registers the confining hooks this schema's Scoped columns\n")
	b.WriteString("// declare: a predicate on every read, update and delete, and a stamp on every\n")
	b.WriteString("// create.\n")
	b.WriteString("//\n// name is the releasable scope name. A handle derived with\n")
	b.WriteString("// DB.WithoutScope(name) is released from the predicates below — an admin\n")
	b.WriteString("// surface that reads across tenants is the case — and is *not* released from\n")
	b.WriteString("// the create stamp, which cannot be released at all: releasing a predicate\n")
	b.WriteString("// shows one more row and releasing a stamp writes a row belonging to nobody.\n")
	b.WriteString("//\n// Registering these satisfies the obligation rest.Resource checks, so a\n")
	b.WriteString("// resource over a scoped model mounts once this has been called and refuses\n")
	b.WriteString("// to until it has.\n")
	b.WriteString("//\n// It is generated, so it is yours: delete the file and write the hooks by\n")
	b.WriteString("// hand the day this shape stops fitting.\n")
	if len(skipped) > 0 {
		fmt.Fprintf(b, "//\n// It does not cover %s, which declare a scope on their own primary key\n",
			andList(skipped))
		b.WriteString("// without naming it. What confines such a table is particular to it — an\n")
		b.WriteString("// identity, a membership subquery — and is not the `column = value` written\n")
		b.WriteString("// below, so those keep their hand-written hooks and still owe them. Naming\n")
		b.WriteString("// the scope on the column is how one opts in.\n")
	}
	b.WriteString("func RegisterScopes(reg *sqlb.Registry, name string, s Scopes) error {\n")

	for _, sf := range fields {
		fmt.Fprintf(b, "\tif s.%s == nil {\n", sf.goName)
		fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"%%s: Scopes.%s is nil, and %s confines %s\", name)\n",
			sf.goName, sf.column, andList(tableNames(sf.tables)))
		b.WriteString("\t}\n")
	}
	b.WriteString("\n")

	for _, sf := range fields {
		for _, t := range sf.tables {
			renderScopeFor(b, sf, t)
		}
	}
	b.WriteString("\treturn nil\n}\n")
}

// renderScopeFor writes the four registrations for one table.
func renderScopeFor(b *bytes.Buffer, sf scopeField, t *schema.TableDef) {
	typ := TypeName(t)
	col := sf.columns[t.Name()]
	fmt.Fprintf(b, "\t// %s\n", t.Name())
	fmt.Fprintf(b, "\tsqlb.On[%s](reg).Scope(name).\n", typ)
	fmt.Fprintf(b, "\t\tBeforeQuery(func(ctx context.Context, q *sqlb.Builder[%s]) error {\n", typ)
	fmt.Fprintf(b, "\t\t\tv, err := s.%s(ctx)\n\t\t\tif err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", sf.goName)
	fmt.Fprintf(b, "\t\t\tq.Where(sqlb.F(%q).Eq(v))\n\t\t\treturn nil\n\t\t}).\n", col)
	// The predicate is added rather than checked, so a write naming a row in
	// another tenant matches nothing and answers 404 — the same answer an id
	// that does not exist gets, which is the answer it should get.
	fmt.Fprintf(b, "\t\tBeforeUpdate(func(ctx context.Context, u *sqlb.Update[%s]) error {\n", typ)
	fmt.Fprintf(b, "\t\t\tv, err := s.%s(ctx)\n\t\t\tif err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", sf.goName)
	fmt.Fprintf(b, "\t\t\tu.Where(sqlb.F(%q).Eq(v))\n\t\t\treturn nil\n\t\t}).\n", col)
	fmt.Fprintf(b, "\t\tBeforeDelete(func(ctx context.Context, d *sqlb.Delete[%s]) error {\n", typ)
	fmt.Fprintf(b, "\t\t\tv, err := s.%s(ctx)\n\t\t\tif err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", sf.goName)
	fmt.Fprintf(b, "\t\t\td.Where(sqlb.F(%q).Eq(v))\n\t\t\treturn nil\n\t\t})\n", col)
	// Not under Scope(name): a stamp has nothing for a reader to be released
	// from, and a create that skipped it would write a row with no tenant
	// rather than see more of them. ScopedHooks refuses BeforeCreate outright.
	fmt.Fprintf(b, "\tsqlb.On[%s](reg).BeforeCreate(func(ctx context.Context, row *%s) error {\n", typ, typ)
	fmt.Fprintf(b, "\t\tv, err := s.%s(ctx)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n", sf.goName)
	fmt.Fprintf(b, "\t\trow.%s = v\n\t\treturn nil\n\t})\n\n", GoName(col))
}

func tableNames(ts []*schema.TableDef) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
