package schema

// A declared body: properties in the field vocabulary that are not columns.
//
// Three declarations use it. [Action.Body] is the oldest — a domain verb's
// request body, which has no columns to derive itself from. [REST.CreateInput]
// is the create body's non-column half, where the body *is* derived from the
// columns and this is the part of it that is not (#309). [Action.Returns] is a
// verb's response, for the verb whose answer is not a row (#312).
//
// One vocabulary, one validator, one set of rules about what a property may
// claim, because the three differ in where they are declared and in which
// direction they travel — and in nothing else.

// Body builds a request body from field declarations.
//
//	Body: schema.Body(
//	    schema.Text("note").Nullable(),
//	    schema.Timestamp("completed_at"),
//	)
//
// The vocabulary is the column vocabulary, deliberately: it is the one the
// emitters already know how to turn into a TypeScript type, a Dart class, a CLI
// flag and an OpenAPI schema. Only what describes a *value* applies here —
// name, type, nullability, enum values, default, comment, and the format rules
// [Field.Pattern], [Field.Min] and [Field.Max]. The capabilities
// that describe a column's place in a table (Filterable, PrimaryKey, Ref,
// Computed, and the rest) have no meaning in a request body and are refused by
// Validate rather than ignored.
func Body(specs ...FieldSpec) []*Field {
	var out []*Field
	for _, s := range specs {
		if s == nil {
			continue
		}
		out = append(out, s.fields()...)
	}
	return out
}

// Result builds an action's response body from field declarations.
//
// It is [Body] under a second name, and the second name is the whole of the
// difference: `Returns: schema.Body(...)` reads as a request, and a reader who
// has to check which side of the exchange a declaration is on is one the
// vocabulary has failed. What both build is a list of declared properties, and
// one validator checks them.
func Result(specs ...FieldSpec) []*Field { return Body(specs...) }

// validateBody refuses the claims a declared body cannot make.
//
// where names the declaration in the diagnostic — `action "complete"`,
// `action "submit-quiz": result`, `CreateInput` — so that one rule reads
// correctly wherever it fires.
func (r *Registry) validateBody(t *TableDef, where string, body []*Field, report func(string, string, string, ...any)) {
	seen := make(map[string]bool, len(body))
	for _, f := range body {
		d := f.Desc()
		if !isIdent(d.Name) {
			report(t.name, d.Name, "%s: property name is not a valid identifier", where)
		}
		if seen[d.Name] {
			report(t.name, d.Name, "%s: property declared twice", where)
		}
		seen[d.Name] = true

		// The format rules a property may carry, checked by the same function
		// a column's are: one vocabulary, one validator.
		r.validateConstraints(t.name, d, where, report)

		// A body property is a value, not a column. Every capability below
		// describes a column's place in a table, and silently ignoring one
		// would leave a declaration that reads as though it did something.
		for _, c := range []struct {
			claimed bool
			what    string
		}{
			{d.PrimaryKey, "PrimaryKey"},
			{d.Unique, "Unique"},
			{d.Filterable, "Filterable"},
			{d.Sortable, "Sortable"},
			{d.Searchable, "Searchable"},
			{d.ReadOnly, "ReadOnly"},
			{d.Immutable, "Immutable"},
			{d.Hidden, "Hidden"},
			{d.WriteOnly, "WriteOnly"},
			{d.Scoped, "Scoped"},
			{d.Ref != nil, "Ref"},
			{d.Computed(), "Computed"},
		} {
			if c.claimed {
				report(t.name, d.Name, "%s: property claims %s, which describes a column rather than a declared property", where, c.what)
			}
		}
	}
}

// validateCreateInput checks the non-column half of a create body.
//
// Two rules beyond the ones every body has, and both close something that is
// otherwise silent. A property declared where no create is exposed is a
// declaration with no body to belong to: nothing is emitted, nothing refuses
// it, and the schema reads as though the endpoint accepted it. And a property
// sharing a column's name is one JSON key with two meanings — the generated
// body would carry the column and the property under one tag, which is a Go
// struct whose marshalling is a coin flip and a client that cannot say which it
// meant.
//
// The collision is checked against the *wire* spelling as well as the column's
// own, because the wire is where the two would actually meet: under
// WireCase(Camel) a property named `createdAt` and the column `created_at` are
// one key, and comparing only the raw names would let it through.
func (r *Registry) validateCreateInput(t *TableDef, report func(string, string, string, ...any)) {
	if len(t.rest.CreateInput) == 0 {
		return
	}
	if !t.rest.Ops.Has(OpCreate) {
		report(t.name, "", "declares %d CreateInput propert%s but does not expose OpCreate; "+
			"a create body is the only thing they could be part of",
			len(t.rest.CreateInput), plural(len(t.rest.CreateInput), "y", "ies"))
		return
	}
	r.validateBody(t, "CreateInput", t.rest.CreateInput, report)

	wire := r.Wire()
	columns := make(map[string]string, len(t.fields)*2)
	for _, f := range t.Fields() {
		name := f.Desc().Name
		columns[name] = name
		columns[wire.WireName(name)] = name
	}
	for _, f := range t.rest.CreateInput {
		name := f.Desc().Name
		col, clash := columns[name]
		if !clash {
			continue
		}
		if col == name {
			report(t.name, name, "CreateInput: property %q is already a column of this table, "+
				"and one key cannot be both; name the property for the value the request sends — "+
				"a `pin` beside a `pin_hash` column — and let the hook derive the column from it", name)
			continue
		}
		report(t.name, name, "CreateInput: property %q is how column %q is spelled on the wire, "+
			"so the two are one key in the request body; rename the property", name, col)
	}
}

// plural picks the suffix for n, which is the whole of what a diagnostic needs
// and less than a dependency.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
