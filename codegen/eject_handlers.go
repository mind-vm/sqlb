package codegen

// The endpoint half of the exit: net/http handlers over the statements in
// store.go, with the same paths, the same status codes and the same JSON the
// generated resource served.
//
// Two things are carried across deliberately, because they are properties
// rather than conveniences. Capabilities stay opt-in — a column that never
// declared Filterable cannot be filtered here either, which is what stops a
// hidden or unindexed column from being probed through the grammar. And the
// obligation stays compulsory: a table that declared Scoped or SoftDelete
// refuses to register without the hook that confines it, which is ADR-0030
// surviving the loss of everything it was implemented in.

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// ejectHandlers emits the request bodies, the handlers and Register.
func ejectHandlers(opts EjectOptions, exposed []*schema.TableDef) ([]byte, error) {
	b := new(bytes.Buffer)
	fmt.Fprintln(b, `
// The endpoints. Every handler is a function you can read top to bottom: parse,
// confine, run one statement, write JSON.`)

	ejectOptionsType(b, exposed)
	ejectRegister(b, exposed)
	for _, t := range exposed {
		ejectResource(b, t, opts.Registry.Wire())
	}
	return ejectFile("handlers.go", opts.pkg(), b)
}

// resourceName is the Go name a resource is known by in Options.
func resourceName(t *schema.TableDef) string { return GoName(t.LocalName()) }

// obligations describes what a table declared that a hook has to satisfy.
type obligations struct {
	scope string // the Scoped column, or ""
	soft  string // the soft-delete column, or ""
}

func obligationsOf(t *schema.TableDef) obligations {
	var o obligations
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Scoped {
			o.scope = d.Name
		}
		if d.SoftDelete {
			o.soft = d.Name
		}
	}
	return o
}

// because renders the reasons a hook is required, for the error the refusal
// prints at startup.
func (o obligations) because() string {
	var parts []string
	if o.scope != "" {
		parts = append(parts, o.scope+" is Scoped")
	}
	if o.soft != "" {
		parts = append(parts, o.soft+" declares a soft delete")
	}
	return strings.Join(parts, "; ")
}

func (o obligations) any() bool { return o.scope != "" || o.soft != "" }

func ejectOptionsType(b *bytes.Buffer, exposed []*schema.TableDef) {
	fmt.Fprintln(b, `
// Options carries the seams the handlers cannot supply for themselves.
//
// In sqlb these were hooks on a registry; here they are function fields, which
// is the same seam with the machinery removed. A resource whose table declared
// Scoped or SoftDelete will not register until they are set — see Register.`)
	fmt.Fprintln(b, "type Options struct {")
	for _, t := range exposed {
		fmt.Fprintf(b, "\t%s %sHooks\n", resourceName(t), resourceName(t))
	}
	fmt.Fprintln(b, "}")

	for _, t := range exposed {
		name := resourceName(t)
		o := obligationsOf(t)
		fmt.Fprintf(b, "\n// %sHooks are the seams for %s.\n", name, t.Rest().Path)
		if o.any() {
			fmt.Fprintf(b, "//\n// Confine is required here (%s), and returns the conditions every read\n", o.because())
			fmt.Fprintln(b, "// and write is narrowed by — the predicate a BeforeQuery hook used to add.")
		}
		if o.scope != "" && t.Rest().Ops.Has(schema.OpCreate) {
			fmt.Fprintf(b, "// Assign is required too, and supplies %s on create: the column is\n", o.scope)
			fmt.Fprintln(b, "// read-only, so no request body can carry it and nothing else would.")
		}
		fmt.Fprintf(b, "type %sHooks struct {\n", name)
		fmt.Fprintln(b, "\t// Confine narrows every statement this resource issues. Nil means")
		fmt.Fprintln(b, "\t// unconfined, which is only allowed when the schema declared nothing.")
		fmt.Fprintln(b, "\tConfine func(*http.Request) ([]Condition, error)")
		fmt.Fprintln(b, "\t// Assign supplies column values a create must set that no request body")
		fmt.Fprintln(b, "\t// carries. It runs before the insert and its values win.")
		fmt.Fprintln(b, "\tAssign func(*http.Request) (map[string]any, error)")
		fmt.Fprintln(b, "}")
	}
}

func ejectRegister(b *bytes.Buffer, exposed []*schema.TableDef) {
	fmt.Fprintln(b, `
// Register mounts every resource the schema exposed.
//
// It returns an error rather than panicking, and it returns one for a missing
// obligation before it registers anything: a resource that declared a tenant
// column and has nothing to confine it with would serve every tenant's rows
// with a 200 next to them, and that is the failure this check exists for.`)
	fmt.Fprintln(b, "func Register(mux *http.ServeMux, db DB, opts Options) error {")
	for _, t := range exposed {
		fmt.Fprintf(b, "\tif err := register%s(mux, db, opts.%s); err != nil {\n\t\treturn err\n\t}\n",
			resourceName(t), resourceName(t))
	}
	fmt.Fprintln(b, "\treturn nil\n}")
}

// ejectResource emits one resource: its bodies, its obligation check and its
// handlers.
func ejectResource(b *bytes.Buffer, t *schema.TableDef, wire schema.WireCase) {
	typeName := TypeName(t)
	name := resourceName(t)
	lower := unexportedGoName(typeName)
	rest := t.Rest()
	o := obligationsOf(t)
	pk := t.PrimaryKey()

	ejectLimits(b, t, lower)
	hasDefaults := false
	if rest.Ops.Has(schema.OpCreate) {
		hasDefaults = ejectInsertDefaults(b, t, lower)
		ejectBodyDecoder(b, t, lower, forCreate, wire)
	}
	if rest.Ops.Has(schema.OpUpdate) {
		ejectBodyDecoder(b, t, lower, forUpdate, wire)
	}

	fmt.Fprintf(b, "\n// register%s mounts %s.\n", name, rest.Path)
	fmt.Fprintf(b, "func register%s(mux *http.ServeMux, db DB, h %sHooks) error {\n", name, name)
	if o.any() {
		fmt.Fprintf(b, "\tif h.Confine == nil {\n")
		fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"ejected: %%s: Confine is required (%%s)\", %q, %q)\n",
			rest.Path, o.because())
		fmt.Fprintf(b, "\t}\n")
	}
	if o.scope != "" && rest.Ops.Has(schema.OpCreate) {
		fmt.Fprintf(b, "\tif h.Assign == nil {\n")
		fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"ejected: %%s: Assign is required (%%s is Scoped and read-only, so a create body cannot carry it)\", %q, %q)\n",
			rest.Path, o.scope)
		fmt.Fprintf(b, "\t}\n")
	}

	// A singleton's operations are the same handlers with the id block removed,
	// on the collection path. See eject_singleton.go.
	single := ejectSingleton(t)
	if rest.Ops.Has(schema.OpList) {
		ejectListHandler(b, t, typeName, lower)
	}
	if rest.Ops.Has(schema.OpSingleton) {
		ejectSingletonReadHandler(b, t, typeName)
	}
	if rest.Ops.Has(schema.OpRead) && pk != nil {
		ejectReadHandler(b, t, typeName, lower)
	}
	if rest.Ops.Has(schema.OpCreate) {
		ejectCreateHandler(b, t, typeName, lower, hasDefaults)
	}
	if rest.Ops.Has(schema.OpUpdate) {
		switch {
		case single:
			ejectSingletonUpdateHandler(b, t, typeName)
		case pk != nil:
			ejectUpdateHandler(b, t, typeName, lower)
		}
	}
	if rest.Ops.Has(schema.OpDelete) {
		switch {
		case single:
			ejectSingletonDeleteHandler(b, t, typeName)
		case pk != nil:
			ejectDeleteHandler(b, t, typeName, lower)
		}
	}
	fmt.Fprintln(b, "\treturn nil\n}")
}

// ejectInsertDefaults emits the values an insert has to supply for the columns
// no request body carries.
//
// sqlb's insert wrote every mapped column of the row struct, so a NOT NULL
// column that is Hidden or ReadOnly and has no database default was written as
// its Go zero value. Reproducing that here is what keeps a create working after
// the exit — without it, the first POST fails on a not-null constraint for a
// column the caller has never heard of. It is a map rather than a literal in
// the statement so that it is the obvious place to change the answer, which for
// a password hash it certainly is.
func ejectInsertDefaults(b *bytes.Buffer, t *schema.TableDef, lower string) bool {
	type def struct{ name, value string }
	var defs []def
	for _, f := range t.StoredFields() {
		d := f.Desc()
		if d.Nullable || d.DatabaseSupplied() || d.PrimaryKey {
			continue
		}
		// A column the body can carry is the caller's to supply, and a missing
		// required property is already a 400.
		if !d.ReadOnly && !d.Hidden {
			continue
		}
		if v := ejectZeroLiteral(d); v != "" {
			defs = append(defs, def{name: d.Name, value: v})
		}
	}
	if len(defs) == 0 {
		return false
	}
	fmt.Fprintf(b, "\n// %sInsertDefaults are the columns an insert must set that no request body\n", lower)
	fmt.Fprintln(b, "// carries and the database has no default for. sqlb wrote the row's zero value;")
	fmt.Fprintln(b, "// so does this, and this is where to write something better.")
	fmt.Fprintf(b, "var %sInsertDefaults = map[string]any{\n", lower)
	for _, d := range defs {
		fmt.Fprintf(b, "\t%q: %s,\n", d.name, d.value)
	}
	fmt.Fprintln(b, "}")
	return true
}

// ejectZeroLiteral is the Go zero value for a column type, or "" for the types
// whose zero is not a value the database would accept anyway — a NOT NULL jsonb
// or bytea with no default is a row sqlb could not insert either.
func ejectZeroLiteral(d *schema.FieldDesc) string {
	if d.Array {
		return ""
	}
	switch d.Type {
	case schema.TypeText, schema.TypeVarchar, schema.TypeUUID, schema.TypeEnum:
		return `""`
	case schema.TypeSmallInt:
		return "int16(0)"
	case schema.TypeInt:
		return "int32(0)"
	case schema.TypeBigInt:
		return "int64(0)"
	case schema.TypeReal:
		return "float32(0)"
	case schema.TypeFloat, schema.TypeNumeric:
		return "float64(0)"
	case schema.TypeBool:
		return "false"
	case schema.TypeTimestamp, schema.TypeDate, schema.TypeTime:
		return "time.Time{}"
	}
	return ""
}

// ejectLimits emits the resource's declared ceilings, so the exit refuses the
// same oversized requests the API did.
func ejectLimits(b *bytes.Buffer, t *schema.TableDef, lower string) {
	r := t.Rest()
	fmt.Fprintf(b, "\n// %sLimits are the ceilings %s declared.\n", lower, t.Name())
	fmt.Fprintf(b, "var %sLimits = Limits{DefaultPageSize: %d, MaxPageSize: %d, MaxFilters: %d, MaxSortTerms: %d, MaxOffset: %d",
		lower, r.DefaultPageSize, r.MaxPageSize, r.MaxFilters, r.MaxSortTerms, r.MaxOffset)
	// A ceiling emits its zero, because zero is a value the parser reads as
	// "take the default". An absent ordering has nothing to say, and
	// `DefaultSort: nil` on every resource that declared none is noise in a file
	// meant to be read.
	if lit := ejectSortLiteral(r.DefaultSort); lit != "" {
		fmt.Fprintf(b, ", DefaultSort: %s", lit)
	}
	b.WriteString("}\n")
}

// ejectSortLiteral renders a declared default ordering as the Order slice the
// exit's parser appends. Resolved here rather than parsed there: the exit reads
// what a resource decided, and re-parsing a term at request time would be a
// second implementation of a grammar the schema already validated.
func ejectSortLiteral(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		name, desc, err := schema.SortTerm(term)
		if err != nil {
			// Validate has already reported it, and emitting a term nothing
			// could parse would put a compile error in the exit.
			continue
		}
		parts = append(parts, fmt.Sprintf("{Column: %q, Desc: %t}", name, desc))
	}
	if len(parts) == 0 {
		return ""
	}
	return "[]Order{" + strings.Join(parts, ", ") + "}"
}

// ejectBodyDecoder emits the JSON decoder for a create or patch body.
//
// A map of raw messages rather than a struct, because that is what answers the
// question a PATCH asks: `{}` and `{"title": null}` decode identically into a
// struct of pointers, and they mean different things.
//
// The two spellings meet here, in the same shape the column table uses for the
// read side: a property is matched and reported by its wire name, and the map
// that comes out is keyed by column, because every consumer of it — the insert,
// the update, the Assign hook, the insert defaults — builds SQL from its keys.
func ejectBodyDecoder(b *bytes.Buffer, t *schema.TableDef, lower string, kind bodyKind, wire schema.WireCase) {
	fields := bodyFields(t, kind)
	verb, suffix := "create", "Create"
	if kind == forUpdate {
		verb, suffix = "patch", "Patch"
	}

	allowed := make([]string, 0, len(fields))
	for _, f := range fields {
		allowed = append(allowed, fmt.Sprintf("%q", wire.WireName(f.Desc().Name)))
	}

	fmt.Fprintf(b, "\n// decode%s%s reads a %s body for %s: which columns it named, and\n",
		TypeName(t), suffix, verb, t.Name())
	fmt.Fprintln(b, "// what each one carried. An unknown property is refused with the list of the")
	fmt.Fprintln(b, "// ones that would have worked.")
	fmt.Fprintf(b, "func decode%s%s(data []byte) (map[string]any, error) {\n", TypeName(t), suffix)
	fmt.Fprintf(b, "\tallowed := []string{%s}\n", strings.Join(allowed, ", "))
	fmt.Fprintln(b, "\tvar raw map[string]json.RawMessage")
	fmt.Fprintln(b, "\tif err := json.Unmarshal(data, &raw); err != nil {")
	fmt.Fprintln(b, "\t\treturn nil, badRequest(\"body\", \"request body is not a JSON object: \"+err.Error(), allowed)")
	fmt.Fprintln(b, "\t}")
	fmt.Fprintln(b, "\tout := map[string]any{}")
	fmt.Fprintln(b, "\tfor name, msg := range raw {")
	fmt.Fprintln(b, "\t\tswitch name {")

	for _, f := range fields {
		d := f.Desc()
		goType := ejectGoType(d)
		pointer := strings.HasPrefix(goType, "*")
		target := goType
		if !pointer {
			// Decoded through a pointer even for a non-nullable column, so an
			// explicit null is a rejection rather than a silent zero.
			target = "*" + goType
		}
		fmt.Fprintf(b, "\t\tcase %q:\n", wire.WireName(d.Name))
		fmt.Fprintf(b, "\t\t\tvar v %s\n", target)
		fmt.Fprintln(b, "\t\t\tif err := json.Unmarshal(msg, &v); err != nil {")
		fmt.Fprintf(b, "\t\t\t\treturn nil, badRequest(\"body.\"+name, err.Error(), nil)\n")
		fmt.Fprintln(b, "\t\t\t}")
		if pointer {
			fmt.Fprintf(b, "\t\t\tout[%q] = v\n", d.Name)
		} else {
			fmt.Fprintln(b, "\t\t\tif v == nil {")
			fmt.Fprintln(b, "\t\t\t\treturn nil, badRequest(\"body.\"+name, \"this column is not nullable\", nil)")
			fmt.Fprintln(b, "\t\t\t}")
			fmt.Fprintf(b, "\t\t\tout[%q] = *v\n", d.Name)
		}
	}

	fmt.Fprintln(b, "\t\tdefault:")
	fmt.Fprintln(b, "\t\t\treturn nil, badRequest(\"body.\"+name, \"unknown property\", allowed)")
	fmt.Fprintln(b, "\t\t}")
	fmt.Fprintln(b, "\t}")

	if kind == forCreate {
		var required []string
		for _, f := range fields {
			if d := f.Desc(); !optionalOnCreate(d) {
				required = append(required, fmt.Sprintf("{%q, %q}", d.Name, wire.WireName(d.Name)))
			}
		}
		if len(required) > 0 {
			// Both spellings, because this check reads a map keyed by column
			// and writes an error that has to name the property the caller
			// would have sent.
			fmt.Fprintf(b, "\tfor _, want := range []struct{ column, wire string }{%s} {\n",
				strings.Join(required, ", "))
			fmt.Fprintln(b, "\t\tif _, ok := out[want.column]; !ok {")
			fmt.Fprintln(b, "\t\t\treturn nil, badRequest(\"body.\"+want.wire, \"this property is required\", allowed)")
			fmt.Fprintln(b, "\t\t}")
			fmt.Fprintln(b, "\t}")
		}
	}
	fmt.Fprintln(b, "\treturn out, nil")
	fmt.Fprintln(b, "}")
}

func ejectListHandler(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	fmt.Fprintf(b, `
	// GET %s — filter, sort and page. The operators are the ones that are a
	// single SQL fragment; ?cursor, ?select, ?expand and ?filter are refused by
	// name rather than ignored.
	mux.HandleFunc("GET %s", func(w http.ResponseWriter, r *http.Request) {
		req, err := ParseList(r.URL.Query(), %sColumns, %sLimits)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		req.Query.Where = append(req.Query.Where, confine...)

		rows, err := List%s(r.Context(), db, req.Query)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// One row past the page was fetched so that has_more costs a row
		// rather than a count.
		hasMore := len(rows) > req.PerPage
		if hasMore {
			rows = rows[:req.PerPage]
		}
		page := Page[%s]{Items: rows, Page: req.Page, PerPage: req.PerPage, HasMore: hasMore}
		if rows == nil {
			page.Items = []%s{}
		}
		if req.Count {
			total, err := Count%s(r.Context(), db, req.Query.Where)
			if err != nil {
				WriteProblem(w, err)
				return
			}
			page.Total = &total
		}
		WriteJSON(w, http.StatusOK, page)
	})
`, t.Rest().Path, t.Rest().Path, lower, lower, typeName, typeName, typeName, typeName)
}

func ejectReadHandler(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	pk := t.PrimaryKey().Desc().Name
	fmt.Fprintf(b, `
	// GET %s/{id}
	mux.HandleFunc("GET %s/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), %sColumns, %q)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		row, err := Get%s(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound(%q))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
`, t.Rest().Path, t.Rest().Path, lower, pk, typeName, t.LocalName())
}

func ejectCreateHandler(b *bytes.Buffer, t *schema.TableDef, typeName, lower string, hasDefaults bool) {
	defaults := ""
	if hasDefaults {
		defaults = fmt.Sprintf(`
		// The columns no body carries and the database does not default.
		for k, v := range %sInsertDefaults {
			if _, named := values[k]; !named {
				values[k] = v
			}
		}
`, lower)
	}
	fmt.Fprintf(b, `
	// POST %s
	mux.HandleFunc("POST %s", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		values, err := decode%sCreate(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
%s		assigned, err := assignFor(r, h.Assign)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// The assignment wins: it is the server's own statement about the row,
		// and a request that named the same column was never allowed to.
		for k, v := range assigned {
			values[k] = v
		}

		row, err := Insert%s(r.Context(), db, values)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, row)
	})
`, t.Rest().Path, t.Rest().Path, typeName, defaults, typeName)
}

func ejectUpdateHandler(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	pk := t.PrimaryKey().Desc().Name
	fmt.Fprintf(b, `
	// PATCH %s/{id}
	mux.HandleFunc("PATCH %s/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), %sColumns, %q)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		changes, err := decode%sPatch(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}

		row, err := Update%s(r.Context(), db, id, changes, confine)
		switch {
		case errors.Is(err, ErrNoChanges):
			WriteProblem(w, badRequest("body", "the request body changed nothing", nil))
			return
		case errors.Is(err, ErrNotFound):
			WriteProblem(w, notFound(%q))
			return
		case err != nil:
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
`, t.Rest().Path, t.Rest().Path, lower, pk, typeName, typeName, t.LocalName())
}

func ejectDeleteHandler(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	pk := t.PrimaryKey().Desc().Name
	fmt.Fprintf(b, `
	// DELETE %s/{id}
	mux.HandleFunc("DELETE %s/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), %sColumns, %q)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		err = Delete%s(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound(%q))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusNoContent, nil)
	})
`, t.Rest().Path, t.Rest().Path, lower, pk, typeName, t.LocalName())
}
