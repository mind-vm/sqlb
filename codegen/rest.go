package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// renderREST emits the request body types and the registration function for
// every table the schema exposes.
//
// The handlers themselves are not generated. rest.Resource is one generic
// function that serves any model, and the OpenAPI document it produces is
// already per-resource, because the parameters come from the model's
// capabilities rather than from a Go struct. What a generator is still needed
// for is the part generics cannot express: the *shape of a request body*, which
// differs from the row in three ways — read-only columns are absent, defaulted
// ones are optional, and a PATCH must distinguish an omitted field from one set
// to null.
//
// Returns nil when the schema exposes nothing, so a package that declares no
// REST surface does not acquire a dependency on huma.
func renderREST(opts Options) ([]byte, error) {
	var exposed []*schema.TableDef
	for _, t := range opts.Registry.Tables() {
		if t.Rest() != nil {
			exposed = append(exposed, t)
		}
	}
	if len(exposed) == 0 {
		return nil, nil
	}

	ov, err := newOverrides(opts.Types, opts.Registry)
	if err != nil {
		return nil, err
	}

	// Every import past these three comes from a body that is actually
	// emitted — from the same createBody/patchBody the renderers are driven
	// by, and per field rather than per table. Register names nothing but the
	// model types, which live in this package, so a resource exposed for
	// reads only needs no import at all beyond the three. The looser shape
	// this replaced derived the set from the create field list of every
	// exposed table regardless of whether a body was emitted, which left a
	// list-only resource with a timestamp column importing "time" for a file
	// that never says time.Time.
	imports := map[string]bool{
		"github.com/danielgtaylor/huma/v2": true,
		"github.com/jryannel/sqlb":         true,
		"github.com/jryannel/sqlb/rest":    true,
	}
	for _, t := range exposed {
		create, _ := createBody(t)
		for _, f := range create {
			bodyImports(imports, t.Name(), f.Desc(), ov)
		}
		patch, hasPatch := patchBody(t)
		if hasPatch {
			// UnmarshalJSON decodes the body twice, the second time into a
			// map[string]json.RawMessage, which is how presence is recorded.
			imports["encoding/json"] = true
		}
		for _, f := range patch {
			d := f.Desc()
			bodyImports(imports, t.Name(), d, ov)
			if !d.Nullable {
				// Changes() refuses an explicit null on a column that cannot
				// hold one. A patch body whose every column is nullable emits
				// no such branch, and so names errors nowhere.
				imports["errors"] = true
			}
		}
	}

	acts := actionsOf(exposed)
	qs := queriesOf(exposed)
	if len(acts) > 0 || len(qs) > 0 {
		// Actions and Queries both name their funcs by signature, and every
		// one of those signatures says context.Context.
		imports["context"] = true
	}
	if len(acts) > 0 {
		actionBodyImports(imports, acts)
	}
	if len(qs) > 0 {
		queryParamImports(imports, qs)
	}

	b := header(opts.Package, sortedSet(imports))

	for _, t := range exposed {
		renderCreateBody(b, t, ov)
		renderUpdateBody(b, t, ov)
	}
	for _, a := range acts {
		renderActionInput(b, a)
	}
	if len(acts) > 0 {
		renderActions(b, acts)
	}
	for _, q := range qs {
		renderQueryParams(b, q)
	}
	if len(qs) > 0 {
		renderQueries(b, qs)
	}
	renderRegister(b, opts.Registry, exposed, len(acts) > 0, len(qs) > 0)

	return gofmt(opts.restFile(), b.Bytes())
}

// bodyKind selects which columns a request body carries.
type bodyKind int

const (
	forCreate bodyKind = iota
	forUpdate
)

// bodyFields is the column set a request body may write.
//
// Read-only columns belong to the database or to a hook; hidden columns never
// cross the boundary in either direction; and an immutable column is settable
// once, at create, so it is absent from the update body rather than rejected by
// the handler.
func bodyFields(t *schema.TableDef, kind bodyKind) []*schema.Field {
	var out []*schema.Field
	for _, f := range t.Fields() {
		d := f.Desc()
		switch {
		case d.ReadOnly || d.Hidden || d.PrimaryKey:
		case kind == forUpdate && d.Immutable:
		default:
			out = append(out, f)
		}
	}
	return out
}

// createBody and patchBody are the one answer to "does this table emit this
// body, and over which columns" — asked by the renderer that writes the body,
// by the registration that names its type, and by the import block that has to
// account for the types it mentions.
//
// They exist because those three re-derived it separately and disagreed. The
// import block computed a create field list for a table exposed for reads only
// and imported "time" for a body that was never written; registration and
// renderUpdateBody each had their own copy of the empty-patch rule. A body's
// existence is one fact, so it is computed in one place.

// createBody reports the columns a create body carries, and whether one is
// emitted. Unlike a patch, an empty create body is still emitted: a table whose
// every column is read-only is created by POSTing `{}`, and rest.Resource needs
// a body type to bind.
func createBody(t *schema.TableDef) ([]*schema.Field, bool) {
	if !t.Rest().Ops.Has(schema.OpCreate) {
		return nil, false
	}
	return bodyFields(t, forCreate), true
}

// patchBody reports the columns a patch body carries, and whether one is
// emitted.
//
// An empty one is not: when every column is read-only, hidden or immutable
// there is nothing a patch could write, and registration passes rest.None
// rather than a body type with no fields.
func patchBody(t *schema.TableDef) ([]*schema.Field, bool) {
	if !t.Rest().Ops.Has(schema.OpUpdate) {
		return nil, false
	}
	fields := bodyFields(t, forUpdate)
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// bodyImports records the packages one body field's type names.
//
// An override replaces the type outright, so what the field needs is the
// override's own import and never the default mapping's — a column overridden
// to uuid.UUID says nothing about time or encoding/json. That is also why the
// override imports are collected here rather than from ov.imports over the
// whole registry, as the models and columns emitters do: those render every
// column of every table, and this file renders the body columns of the exposed
// ones, so a registry-wide set is an unused import waiting for a schema whose
// only uuid column is a primary key.
func bodyImports(imports map[string]bool, table string, d *schema.FieldDesc, ov *overrides) {
	if r := ov.rule(table, d); r != nil {
		if r.Import != "" {
			imports[r.Import] = true
		}
		return
	}
	// schema.Type.GoType names three qualified types and no others: time.Time,
	// json.RawMessage and sqlb.Vector. The third needs no case, because sqlb is
	// imported unconditionally and a vector column is hidden by construction so
	// no body carries one. A fourth would need a case here, in the models
	// emitter and in the columns emitter — TestGeneratedGoCompiles is what says
	// so, since a type named without its import is valid source that only fails
	// at the consumer's compiler.
	switch goType := d.GoType(); {
	case strings.Contains(goType, "time.Time"):
		imports["time"] = true
	case strings.Contains(goType, "json.RawMessage"):
		imports["encoding/json"] = true
	}
}

// enumTag renders Huma's enum struct tag for an enum column.
//
// Without it the generated body types the column as a string alias, Huma
// documents it as a plain string, and an invalid value passes validation,
// reaches the INSERT and comes back as whatever the database said. The
// TypeScript client and the CLI both enforce the value set already, which left
// the server — the only one that has to — as the weakest of the three.
func enumTag(d *schema.FieldDesc) string {
	if d.Type != schema.TypeEnum || len(d.EnumValues) == 0 {
		return ""
	}
	return fmt.Sprintf(" enum:%q", strings.Join(d.EnumValues, ","))
}

// optionalOnCreate reports whether a create body may omit the column: a
// nullable column is absent as NULL, and one the database supplies — a default,
// a sequence or an identity — is absent so the database fills it.
func optionalOnCreate(d *schema.FieldDesc) bool {
	return d.Nullable || d.DatabaseSupplied()
}

func renderCreateBody(b *bytes.Buffer, t *schema.TableDef, ov *overrides) {
	fields, ok := createBody(t)
	if !ok {
		return
	}
	typeName := TypeName(t)
	name := typeName + "Create"

	fmt.Fprintf(b, "\n// %s is the request body for creating a %s.\n", name, typeName)
	fmt.Fprintf(b, "//\n// Read-only columns are absent: the database or a BeforeCreate hook owns them.\n")
	fmt.Fprintf(b, "// A column with a default is optional, so leaving it out means the database\n")
	fmt.Fprintf(b, "// supplies the value rather than the zero value overwriting it.\n")
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range fields {
		d := f.Desc()
		fmt.Fprintf(b, "\t%s %s `json:\"%s%s\"%s`", GoName(d.Name), bodyType(typeName, t.Name(), d, forCreate, ov), d.Name, omitEmpty(optionalOnCreate(d)), enumTag(d))
		if c := d.Comment; c != "" {
			fmt.Fprintf(b, " // %s", c)
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintln(b, "}")

	fmt.Fprintf(b, "\n// Row builds the row to insert. It satisfies rest.CreateBody.\n")
	fmt.Fprintf(b, "func (c %s) Row() (*%s, error) {\n", name, typeName)
	fmt.Fprintf(b, "\trow := &%s{}\n", typeName)
	for _, f := range fields {
		d := f.Desc()
		field := GoName(d.Name)
		// Whether the *model* field is a pointer, not whether the column is
		// nullable. The two part company on the slice-typed columns: a nullable
		// bytea stays []byte and an array stays []T whether or not it is
		// nullable, because a nil slice already says NULL (schema.FieldDesc.GoType).
		// Keying on d.Nullable there would assign a *[]byte body field to a
		// []byte model field. bodyType makes the body a pointer for any optional
		// non-pointer model type, so those go through the deref branch.
		modelIsPointer := strings.HasPrefix(goType(typeName, t.Name(), d, ov), "*")
		switch {
		case modelIsPointer:
			// The model field is a pointer, so absent and null are the same
			// thing and both mean NULL.
			fmt.Fprintf(b, "\trow.%s = c.%s\n", field, field)
		case optionalOnCreate(d):
			fmt.Fprintf(b, "\tif c.%s != nil {\n\t\trow.%s = *c.%s\n\t}\n", field, field, field)
		default:
			fmt.Fprintf(b, "\trow.%s = c.%s\n", field, field)
		}
	}
	fmt.Fprintln(b, "\treturn row, nil\n}")
}

func renderUpdateBody(b *bytes.Buffer, t *schema.TableDef, ov *overrides) {
	fields, ok := patchBody(t)
	if !ok {
		return
	}
	typeName := TypeName(t)
	name := typeName + "Patch"

	fmt.Fprintf(b, "\n// %s is the request body for patching a %s.\n", name, typeName)
	fmt.Fprintf(b, "//\n// Every field is a pointer and every field is optional, so a request writes\n")
	fmt.Fprintf(b, "// only the columns it names. Immutable columns are absent: they are settable\n")
	fmt.Fprintf(b, "// once, at create.\n")
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range fields {
		d := f.Desc()
		fmt.Fprintf(b, "\t%s %s `json:\"%s,omitempty\"%s`", GoName(d.Name), bodyType(typeName, t.Name(), d, forUpdate, ov), d.Name, enumTag(d))
		if c := d.Comment; c != "" {
			fmt.Fprintf(b, " // %s", c)
		}
		fmt.Fprintln(b)
	}
	// Presence cannot be read off the decoded struct: a nil pointer means both
	// "absent" and "explicitly null", and those must write different SQL.
	fmt.Fprintf(b, "\n\t// present records which properties the request body actually carried.\n")
	fmt.Fprintln(b, "\tpresent map[string]bool")
	fmt.Fprintln(b, "}")

	fmt.Fprintf(b, "\n// UnmarshalJSON decodes the body and remembers which properties were present.\n")
	fmt.Fprintf(b, "//\n// Without this a nil pointer would be ambiguous: `{}` and `{\"%s\": null}` decode\n", fields[0].Desc().Name)
	fmt.Fprintf(b, "// identically, but the first must change nothing and the second must write NULL.\n")
	fmt.Fprintf(b, "func (u *%s) UnmarshalJSON(data []byte) error {\n", name)
	fmt.Fprintf(b, "\ttype plain %s\n", name)
	fmt.Fprintf(b, "\tif err := json.Unmarshal(data, (*plain)(u)); err != nil {\n\t\treturn err\n\t}\n")
	fmt.Fprintln(b, "\tvar keys map[string]json.RawMessage")
	fmt.Fprintf(b, "\tif err := json.Unmarshal(data, &keys); err != nil {\n\t\treturn err\n\t}\n")
	fmt.Fprintln(b, "\tu.present = make(map[string]bool, len(keys))")
	fmt.Fprintln(b, "\tfor k := range keys {\n\t\tu.present[k] = true\n\t}")
	fmt.Fprintln(b, "\treturn nil\n}")

	fmt.Fprintf(b, "\n// Changes reports the columns the request named. It satisfies rest.UpdateBody.\n")
	fmt.Fprintf(b, "func (u %s) Changes() (map[string]any, error) {\n", name)
	fmt.Fprintf(b, "\tout := make(map[string]any, len(u.present))\n")
	for _, f := range fields {
		d := f.Desc()
		field := GoName(d.Name)
		fmt.Fprintf(b, "\tif u.present[%q] {\n", d.Name)
		if d.Nullable {
			// A nil pointer that was present is an explicit null.
			fmt.Fprintf(b, "\t\tout[%q] = u.%s\n", d.Name, field)
		} else {
			fmt.Fprintf(b, "\t\tif u.%s == nil {\n", field)
			fmt.Fprintf(b, "\t\t\treturn nil, errors.New(%q)\n", d.Name+" is not nullable and cannot be set to null")
			fmt.Fprintf(b, "\t\t}\n")
			fmt.Fprintf(b, "\t\tout[%q] = *u.%s\n", d.Name, field)
		}
		fmt.Fprintln(b, "\t}")
	}
	fmt.Fprintln(b, "\treturn out, nil\n}")
}

// bodyType is the Go type of a body field: a pointer wherever the field is
// optional, so that absent is distinguishable from zero.
func bodyType(typeName, table string, d *schema.FieldDesc, kind bodyKind, ov *overrides) string {
	base := goType(typeName, table, d, ov)
	if strings.HasPrefix(base, "*") {
		return base // nullable columns are already pointers
	}
	if kind == forUpdate || optionalOnCreate(d) {
		return "*" + base
	}
	return base
}

func omitEmpty(optional bool) string {
	if optional {
		return ",omitempty"
	}
	return ""
}

func renderRegister(b *bytes.Buffer, reg *schema.Registry, exposed []*schema.TableDef, hasActions, hasQueries bool) {
	fmt.Fprintf(b, "\n// Register mounts every exposed resource on api.\n")
	fmt.Fprintf(b, "//\n// The handlers are rest.Resource, instantiated per model. Registration is\n")
	fmt.Fprintf(b, "// generic rather than reflective because query hooks are keyed by type: a\n")
	fmt.Fprintf(b, "// BeforeQuery hook registered on a model applies to its REST reads too, which\n")
	fmt.Fprintf(b, "// is how tenant scoping stops being something each handler must remember.\n")
	var params []string
	if hasActions {
		fmt.Fprintf(b, "//\n// The schema declares actions, so this takes the funcs that run inside\n")
		fmt.Fprintf(b, "// their envelopes. That parameter is the compiler's half of the bargain:\n")
		fmt.Fprintf(b, "// an action added to the schema fails the build here rather than serving a\n")
		fmt.Fprintf(b, "// route nobody wired.\n")
		params = append(params, "actions Actions")
	}
	if hasQueries {
		fmt.Fprintf(b, "//\n// The schema declares queries too, so this also takes the funcs that\n")
		fmt.Fprintf(b, "// answer them. Unlike Actions there is no envelope behind a query — see\n")
		fmt.Fprintf(b, "// Queries' own doc comment for what that means.\n")
		params = append(params, "queries Queries")
	}
	sig := "func Register(api huma.API, db sqlb.Executor"
	for _, p := range params {
		sig += ", " + p
	}
	fmt.Fprintln(b, sig+") error {")
	for _, t := range exposed {
		typeName := TypeName(t)
		r := t.Rest()
		create, update := "rest.None["+typeName+"]", "rest.None["+typeName+"]"
		if _, ok := createBody(t); ok {
			create = typeName + "Create"
		}
		if _, ok := patchBody(t); ok {
			update = typeName + "Patch"
		}

		// A table with verbs binds its options to a name, because the resource
		// and every action or query on it are one exposure and must not be
		// able to disagree about the path, the tag or the transaction policy.
		acts := actionsOf([]*schema.TableDef{t})
		qs := queriesOf([]*schema.TableDef{t})
		optsVar := ""
		if len(acts) > 0 || len(qs) > 0 {
			optsVar = unexportedGoName(t.LocalName()) + "Options"
			fmt.Fprintf(b, "\t%s := rest.Options{\n", optsVar)
		} else {
			fmt.Fprintf(b, "\tif err := rest.Resource[%s, %s, %s](api, db, rest.Options{\n", typeName, create, update)
		}
		fmt.Fprintf(b, "\t\tPath: %q,\n", r.Path)
		fmt.Fprintf(b, "\t\tName: %q,\n", Singular(t.LocalName()))
		fmt.Fprintf(b, "\t\tTag:  %q,\n", r.Tag)
		fmt.Fprintf(b, "\t\tOps:  %s,\n", opsExpr(r.Ops))
		if c := t.Comment(); c != "" {
			fmt.Fprintf(b, "\t\tDescription: %q,\n", c)
		}
		if r.DefaultPageSize > 0 {
			fmt.Fprintf(b, "\t\tDefaultPageSize: %d,\n", r.DefaultPageSize)
		}
		if r.MaxPageSize > 0 {
			fmt.Fprintf(b, "\t\tMaxPageSize: %d,\n", r.MaxPageSize)
		}
		if r.MaxFilters > 0 {
			fmt.Fprintf(b, "\t\tMaxFilters: %d,\n", r.MaxFilters)
		}
		if r.MaxSortTerms > 0 {
			fmt.Fprintf(b, "\t\tMaxSortTerms: %d,\n", r.MaxSortTerms)
		}
		if r.MaxOffset > 0 {
			fmt.Fprintf(b, "\t\tMaxOffset: %d,\n", r.MaxOffset)
		}
		if len(r.DefaultSort) > 0 {
			quoted := make([]string, len(r.DefaultSort))
			for i, term := range r.DefaultSort {
				quoted[i] = fmt.Sprintf("%q", term)
			}
			fmt.Fprintf(b, "\t\tDefaultSort: []string{%s},\n", strings.Join(quoted, ", "))
		}
		// Expandable comes from the columns rather than from REST, because
		// .Expandable() is already the opt-in and a second one on the resource
		// would only be a way to disagree with the first.
		if rel := expandableRelations(reg, t); len(rel) > 0 {
			quoted := make([]string, len(rel))
			for i, name := range rel {
				quoted[i] = fmt.Sprintf("%q", name)
			}
			fmt.Fprintf(b, "\t\tExpandable: []string{%s},\n", strings.Join(quoted, ", "))
		}
		// Computed likewise comes from the columns. A computed column declared
		// on an exposed table is one the schema meant this resource to serve,
		// so the generated mount serves all of them and its responses are
		// unchanged by the opt-in. What the opt-in changes is everything
		// *else* reading the model: a hand-written query no longer inherits a
		// list screen's correlated subqueries, or its per-request binds (#92).
		if names := computedColumns(t); len(names) > 0 {
			quoted := make([]string, len(names))
			for i, name := range names {
				quoted[i] = fmt.Sprintf("%q", name)
			}
			fmt.Fprintf(b, "\t\tComputed: []string{%s},\n", strings.Join(quoted, ", "))
		}
		if len(acts) == 0 && len(qs) == 0 {
			fmt.Fprintf(b, "\t}); err != nil {\n\t\treturn err\n\t}\n")
			continue
		}
		fmt.Fprintf(b, "\t}\n")
		fmt.Fprintf(b, "\tif err := rest.Resource[%s, %s, %s](api, db, %s); err != nil {\n\t\treturn err\n\t}\n",
			typeName, create, update, optsVar)
		renderActionCalls(b, optsVar, acts)
		renderQueryCalls(b, optsVar, qs)
	}
	fmt.Fprintln(b, "\treturn nil\n}")
}

// opsExpr renders an operation mask as the rest constants that make it up, so
// the generated file reads like the schema declaration it came from.
func opsExpr(ops schema.Op) string {
	var parts []string
	for _, e := range []struct {
		op   schema.Op
		name string
	}{
		{schema.OpCreate, "rest.OpCreate"}, {schema.OpRead, "rest.OpRead"},
		{schema.OpUpdate, "rest.OpUpdate"}, {schema.OpDelete, "rest.OpDelete"},
		{schema.OpList, "rest.OpList"}, {schema.OpSingleton, "rest.OpSingleton"},
	} {
		if ops.Has(e.op) {
			parts = append(parts, e.name)
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " | ")
}

// computedColumns names the table's derived columns, in declaration order.
//
// Hidden ones are skipped for the reason they are skipped everywhere: a column
// that never leaves the process is not part of a response, and listing it would
// make the resource pay for an expression nothing reads.
func computedColumns(t *schema.TableDef) []string {
	var out []string
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Computed() && !d.Hidden {
			out = append(out, d.Name)
		}
	}
	return out
}
