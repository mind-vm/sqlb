package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// This file emits the TypeScript client described by ADR-0028: row types,
// typed request parameters, a URL encoder for the filter grammar, a key
// factory, and TanStack Query option factories.
//
// It is generated from the same registry the Go emitters read rather than from
// the emitted OpenAPI document, because the document is lossy exactly where the
// value is. `?status=eq.published` documents as `array<string>` with the
// operator vocabulary in prose, so a generic generator emits `status?: string[]`
// and `status=bogus.x` compiles. The registry knows the operators each column
// type accepts, so the client can refuse it at the TypeScript compile step —
// which is the typed column facade's property (ADR-0009) carried across the
// wire.
//
// Two files, because the layers are meant to be usable separately:
//
//   - client.gen.ts   types, encoder, transport functions, key factory. No
//     imports at all, so it typechecks in any project.
//   - queries.gen.ts  queryOptions, infiniteQueryOptions and mutationOptions,
//     which take @tanstack/react-query as a peer dependency.
//
// What is deliberately not emitted: the client shell, hooks, optimistic updates
// and any write policy — a mutation gets its `mutationFn` and no `onSuccess`,
// because what a write invalidates is not derivable from a schema. Auth,
// refresh, retry and redirect-on-401 are not derivable either, so the transport
// is injected — the same seam argument `rest` makes by mounting onto a huma.API
// the application built.

// renderTSClient emits layers 1 to 3, plus the key factory.
func renderTSClient(opts Options) ([]byte, error) {
	resources, err := tsResources(opts.Registry)
	if err != nil {
		return nil, err
	}

	// The body first, so the import list can be computed from what it actually
	// references rather than guessed. noUnusedLocals makes a guess a build
	// failure, and the set genuinely varies by schema (#110).
	var body bytes.Buffer

	// Row types for every table, not only the exposed ones: an expansion can
	// name a table that has no endpoint of its own, and the row still has to
	// have a type. This is the same call `.Expandable()` already makes on the
	// server.
	for _, t := range opts.Registry.Tables() {
		tsRowTypes(&body, opts.Registry, t)
	}

	for _, r := range resources {
		tsResourceSection(&body, r)
	}

	tsChangeFeed(&body, resources)

	if name := tsUnexportedUse(body.String()); name != "" {
		return nil, fmt.Errorf(
			"codegen: the TypeScript client calls %q, which the runtime does not export — "+
				"export it from tsRuntime, or the emitted client will not compile", name)
	}

	if err := tsCollision(opts.Registry, "the TypeScript client", body.String()); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	b.WriteString(tsClientHeader)
	b.WriteString(tsRuntimeImports(body.String(), opts.tsRuntimeImport()))
	b.Write(body.Bytes())
	return b.Bytes(), nil
}

// renderTSRuntime emits the schema-independent half on its own.
//
// It takes no options because it depends on nothing: two projects rendering it
// produce identical bytes, which is what lets several modules share one file
// and keeps `check` meaningful for each of them (#110).
func renderTSRuntime() []byte {
	var b bytes.Buffer
	b.WriteString(tsRuntimeHeader)
	b.WriteString(tsRuntime)
	return b.Bytes()
}

// renderTSQueries emits layer 4. It returns nil when nothing is exposed, so a
// schema with no REST surface does not acquire a file that imports TanStack
// Query for the sake of an empty object.
func renderTSQueries(opts Options) ([]byte, error) {
	all, err := tsResources(opts.Registry)
	if err != nil {
		return nil, err
	}
	// A resource that can neither be read nor written has nothing to offer
	// options for, and a file of empty factories is worse than no file.
	var resources []tsResource
	for _, r := range all {
		if r.hasQueries() || r.hasMutations() {
			resources = append(resources, r)
		}
	}
	if len(resources) == 0 {
		return nil, nil
	}

	// The body first, so the TanStack import can be computed from what it
	// actually references rather than fixed in the header. A read-only schema
	// that imported mutationOptions would fail its own build under
	// `noUnusedLocals`, which is the same reason the client next door computes
	// its runtime import (#110).
	var body bytes.Buffer

	// Types and values are imported separately, because a project with
	// `verbatimModuleSyntax` needs a type import to say so — and because it
	// makes it visible that this file adds behaviour to types it does not own.
	types := []string{"Transport"}
	values := []string{}
	for _, r := range resources {
		fmt.Fprintf(&body, "\n// %s\n", tsRule(r.path))

		if r.hasQueries() {
			tsQueriesSection(&body, r)
			// Keys belong to the read half: a mutation here carries no
			// `onSuccess`, so a write-only resource must not import a factory
			// nothing in the file names.
			values = append(values, r.ident+"Keys")
			if r.hasExpand() {
				types = append(types, r.typeName+"Expand")
			}
			if r.ops.Has(schema.OpList) {
				types = append(types, r.typeName+"Column", r.typeName+"ListParams")
				values = append(values, "list"+r.plural)
			}
			if r.readsOne() {
				types = append(types, r.typeName+"GetParams")
				values = append(values, "get"+r.typeName)
			}
		}

		if r.hasMutations() {
			tsMutationsSection(&body, r)
			if r.canCreate() {
				types = append(types, r.typeName+"Create")
				values = append(values, "create"+r.typeName)
			}
			if r.canUpdate() {
				types = append(types, r.typeName+"Patch")
				values = append(values, "update"+r.typeName)
			}
			if r.canDelete() {
				values = append(values, "delete"+r.typeName)
			}
			for _, a := range r.table.Actions() {
				values = append(values, tsActionName(r.table, a))
				if len(a.Body) > 0 {
					types = append(types, tsActionInput(r.table, a))
				}
			}
		}
	}

	if err := tsCollision(opts.Registry, "the TypeScript queries file", body.String()); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	fmt.Fprint(&b, tsQueriesHeader)
	fmt.Fprint(&b, tsTanStackImport(body.String()))
	tsImportList(&b, "import type", types, opts.tsClientImport())
	tsImportList(&b, "import", values, opts.tsClientImport())
	b.Write(body.Bytes())
	return b.Bytes(), nil
}

// tsTanStackImport names only the option helpers the body actually calls.
func tsTanStackImport(body string) string {
	var used []string
	for _, name := range []string{"infiniteQueryOptions", "mutationOptions", "queryOptions"} {
		if usesSymbol(body, name) {
			used = append(used, name)
		}
	}
	if len(used) == 0 {
		return ""
	}
	return "import { " + strings.Join(used, ", ") + " } from '@tanstack/react-query';\n"
}

// tsImportList writes one import statement, sorted and one name per line, so
// that adding a table produces a one-line diff rather than a reflowed one.
func tsImportList(b *bytes.Buffer, keyword string, names []string, from string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(b, "%s {\n", keyword)
	for _, name := range sortedSet(uniqueSet(names)) {
		fmt.Fprintf(b, "  %s,\n", name)
	}
	fmt.Fprintf(b, "} from %s;\n", tsString(from))
}

// tsResource is everything about one exposed table the emitter needs, resolved
// once so the templates below read as output rather than as lookups.
type tsResource struct {
	table    *schema.TableDef
	typeName string // Task
	ident    string // task
	plural   string // Tasks
	path     string
	ops      schema.Op

	filterable []*schema.FieldDesc
	sortable   []string
	selectable []string
	searchable bool
	relations  []tsRelation
	pk         string

	// needsColumns are the selectable computed columns that declare Needs. A
	// write has no per-request bind to resolve their expression with, so
	// mutate.go's RETURNING and the JSON response both leave the key out
	// (ADR-0041, #163) — a read still carries it. This is what makes a write's
	// response type different from a read's whenever it is non-empty.
	needsColumns []string

	// wire is the schema's wire case, carried on the resource so that every
	// name this file emits goes through one function rather than each site
	// remembering to. A client is generated *against* the wire, so the column's
	// own name appears nowhere in it except in a doc comment.
	wire schema.WireCase
}

// n spells one of this resource's column names the way the wire does.
func (r *tsResource) n(name string) string { return r.wire.WireName(name) }

// singleton reports whether this resource is the caller's one row, in which
// case every function it emits drops the `id` argument and addresses the
// collection path itself (#166).
func (r *tsResource) singleton() bool { return r.ops.Has(schema.OpSingleton) }

// readsOne reports whether the resource serves a single row by either shape, so
// the parameter type and the `get` function are emitted for both.
func (r *tsResource) readsOne() bool { return r.ops.Has(schema.OpRead) || r.singleton() }

// itemRoute is what a doc comment calls the single-row route.
func (r *tsResource) itemRoute() string {
	if r.singleton() {
		return r.path
	}
	return r.path + "/{id}"
}

// tsRelation is one entry of a resource's ?expand vocabulary, in the direction
// it is served.
type tsRelation struct {
	name     string // wire name, e.g. "list"
	target   string // TypeScript type of the expanded rows
	forward  bool   // a reference on this table, rather than one pointing at it
	nullable bool   // the reference column is nullable, so the relation may be null
	oneToOne bool   // an inverse relation backed by a unique FK — one row or null
}

func (r tsResource) hasExpand() bool { return len(r.relations) > 0 }

// The four predicates below decide what layer 4 offers, and each mirrors the
// condition the transport function next door was emitted under — an option
// object naming a function that does not exist is a generator that compiles
// into a build failure.
func (r tsResource) hasQueries() bool {
	return r.ops.Has(schema.OpList) || r.readsOne()
}

func (r tsResource) canCreate() bool { return r.ops.Has(schema.OpCreate) }

// A patch body with no writable columns leaves `update` unemitted, so there is
// nothing to wrap.
func (r tsResource) canUpdate() bool {
	return r.ops.Has(schema.OpUpdate) && len(bodyFields(r.table, forUpdate)) > 0
}

func (r tsResource) canDelete() bool { return r.ops.Has(schema.OpDelete) }

// needsWriteResult reports whether create/update cannot answer with the plain
// row type: there is a Needs column in it, and at least one of the two
// endpoints that would omit it is actually emitted.
func (r tsResource) needsWriteResult() bool {
	return len(r.needsColumns) > 0 && (r.canCreate() || r.canUpdate())
}

// writeResultType is what create%s and update%s return: the row type itself,
// unless a Needs column forces a narrower one.
func (r tsResource) writeResultType() string {
	if !r.needsWriteResult() {
		return r.typeName
	}
	return r.typeName + "WriteResult"
}

func (r tsResource) hasMutations() bool {
	return r.canCreate() || r.canUpdate() || r.canDelete() || len(r.table.Actions()) > 0
}

func tsResources(reg *schema.Registry) ([]tsResource, error) {
	var out []tsResource
	for _, t := range reg.Tables() {
		rest := t.Rest()
		if rest == nil {
			continue
		}
		r := tsResource{
			table:    t,
			typeName: TypeName(t),
			ident:    tsIdent(lowerFirst(TypeName(t))),
			plural:   GoName(t.LocalName()),
			path:     rest.Path,
			ops:      rest.Ops,
			wire:     reg.Wire(),
		}
		for _, f := range t.Fields() {
			d := f.Desc()
			if d.Hidden || d.WriteOnly {
				continue
			}
			if d.PrimaryKey {
				r.pk = r.n(d.Name)
			}
			r.selectable = append(r.selectable, r.n(d.Name))
			if d.Filterable {
				r.filterable = append(r.filterable, d)
			}
			if d.Sortable {
				r.sortable = append(r.sortable, r.n(d.Name))
			}
			if d.Searchable {
				r.searchable = true
			}
			if d.Computed() && len(d.Needs) > 0 {
				r.needsColumns = append(r.needsColumns, r.n(d.Name))
			}
		}
		// The expandable set comes from the columns, exactly as the generated
		// rest.Options does, so the client cannot offer a relation the server
		// would reject or miss one it would serve.
		for _, name := range expandableRelations(reg, t) {
			rel, err := tsRelationOf(reg, t, name)
			if err != nil {
				return nil, err
			}
			r.relations = append(r.relations, rel)
		}
		out = append(out, r)
	}
	return out, nil
}

func tsRelationOf(reg *schema.Registry, t *schema.TableDef, name string) (tsRelation, error) {
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Expandable && d.Ref != nil && !d.Ref.External && d.Ref.Name == name {
			if d.Ref.Table == nil {
				return tsRelation{}, fmt.Errorf("codegen: table %s: relation %q has no target table", t.Name(), name)
			}
			return tsRelation{
				name:     name,
				target:   TypeName(d.Ref.Table),
				forward:  true,
				nullable: d.Nullable,
			}, nil
		}
	}
	for _, inv := range reg.Inverses(t) {
		if inv.Expandable && inv.Name == name {
			return tsRelation{
				name:     name,
				target:   TypeName(inv.Table),
				oneToOne: inv.OneToOne,
			}, nil
		}
	}
	return tsRelation{}, fmt.Errorf("codegen: table %s: no relation named %q", t.Name(), name)
}

// tsRowTypes emits the enums, the row interface and the request bodies for one
// table.
func tsRowTypes(b *bytes.Buffer, reg *schema.Registry, t *schema.TableDef) {
	wire := reg.Wire()
	typeName := TypeName(t)
	fmt.Fprintf(b, "\n// %s\n", tsRule(t.Name()))

	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Type != schema.TypeEnum || len(d.EnumValues) == 0 || d.Hidden {
			continue
		}
		fmt.Fprintf(b, "\n/** The %s.%s column's value set. */\n", t.Name(), d.Name)
		fmt.Fprintf(b, "export type %s =%s;\n", tsEnumName(typeName, d), tsUnion(d.EnumValues))
	}

	fmt.Fprintln(b)
	if c := t.Comment(); c != "" {
		fmt.Fprintf(b, "/** %s */\n", c)
	} else {
		fmt.Fprintf(b, "/** A row of %s. */\n", t.Name())
	}
	fmt.Fprintf(b, "export interface %s {\n", typeName)

	// Property names are the `json` tag spelling, which is what the schema's
	// WireCase computed. Both sides come from that one function, so there is no
	// mapping layer between the response and the caller — which is the property
	// ADR-0036 protects and the reason the case is per schema, not per column.
	rels := tsForwardRelations(t)
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Hidden || d.WriteOnly {
			// Absent from the row type entirely, as it is from the response.
			// A hidden column also has no spelling a client could use
			// anywhere; a write-only one still has one, in the generated
			// create/update body, just not here.
			continue
		}
		tsDoc(b, "  ", d.Comment)
		if d.Type == schema.TypeBigInt {
			// Stated where the type is read, because the loss is silent:
			// JSON.parse turns 9007199254740993 into 9007199254740992 with no
			// error anywhere.
			fmt.Fprintf(b, "  /** bigint. Values above 2^53 lose precision in JSON. */\n")
		}
		fmt.Fprintf(b, "  %s: %s;\n", tsProp(wire.WireName(d.Name)), tsType(typeName, d))
		if rel, ok := rels[d.Name]; ok {
			fmt.Fprintf(b, "  /** Filled in by `expand: ['%s']`, absent otherwise. */\n", rel.name)
			fmt.Fprintf(b, "  %s?: %s;\n", tsProp(rel.name), tsRelationType(rel))
		}
	}
	for _, inv := range reg.Inverses(t) {
		if !inv.Expandable {
			continue
		}
		if inv.OneToOne {
			fmt.Fprintf(b, "  /** Filled in by `expand: ['%s']`, absent otherwise. */\n", inv.Name)
			fmt.Fprintf(b, "  %s?: %s | null;\n", tsProp(inv.Name), TypeName(inv.Table))
			continue
		}
		// One block, not two adjacent ones: two adjacent `/** */` comments only
		// let the last one attach in TS tooling, silently dropping "Filled in
		// by `expand`" from hover docs for every ordinary collection relation.
		fmt.Fprintf(b, "  /** Filled in by `expand: ['%s']`, absent otherwise. Capped at %d rows. */\n", inv.Name, inv.Cap())
		fmt.Fprintf(b, "  %s?: Collection<%s>;\n", tsProp(inv.Name), TypeName(inv.Table))
	}
	fmt.Fprintln(b, "}")

	tsBodyTypes(b, t, typeName, wire)
	tsActionBodies(b, t, typeName)
}

// tsForwardRelations is the expandable references declared on t, keyed by the
// column they hang off.
func tsForwardRelations(t *schema.TableDef) map[string]tsRelation {
	out := map[string]tsRelation{}
	for _, f := range t.Fields() {
		d := f.Desc()
		if !d.Expandable || d.Ref == nil || d.Ref.External || d.Ref.Table == nil {
			continue
		}
		out[d.Name] = tsRelation{
			name:     d.Ref.Name,
			target:   TypeName(d.Ref.Table),
			forward:  true,
			nullable: d.Nullable,
		}
	}
	return out
}

// tsBodyTypes emits the create and patch bodies, over the same column sets the
// Go bodies use, so the two cannot disagree about what a request may write.
func tsBodyTypes(b *bytes.Buffer, t *schema.TableDef, typeName string, wire schema.WireCase) {
	rest := t.Rest()
	if rest == nil {
		return
	}

	if rest.Ops.Has(schema.OpCreate) {
		fields := bodyFields(t, forCreate)
		fmt.Fprintf(b, "\n/**\n * The request body for creating a %s.\n *\n", typeName)
		fmt.Fprint(b, " * Read-only columns are absent: the database or a BeforeCreate hook owns\n")
		fmt.Fprint(b, " * them. A column with a default is optional.\n */\n")
		fmt.Fprintf(b, "export interface %sCreate {\n", typeName)
		for _, f := range fields {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s%s: %s;\n", tsProp(wire.WireName(d.Name)), tsOptional(optionalOnCreate(d)), tsType(typeName, d))
		}
		fmt.Fprintln(b, "}")
	}

	if rest.Ops.Has(schema.OpUpdate) {
		fields := bodyFields(t, forUpdate)
		if len(fields) == 0 {
			return
		}
		fmt.Fprintf(b, "\n/**\n * The request body for patching a %s.\n *\n", typeName)
		fmt.Fprint(b, " * Every property is optional, so a request writes only the columns it\n")
		fmt.Fprint(b, " * names. Immutable columns are absent: they are settable once, at create.\n")
		fmt.Fprint(b, " * Sending `null` for a nullable column writes NULL; omitting it changes\n")
		fmt.Fprint(b, " * nothing, which is the distinction the server reads off the JSON body.\n */\n")
		fmt.Fprintf(b, "export interface %sPatch {\n", typeName)
		for _, f := range fields {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s?: %s;\n", tsProp(wire.WireName(d.Name)), tsType(typeName, d))
		}
		fmt.Fprintln(b, "}")
	}
}

// tsResourceSection emits the query vocabulary, the transport functions and the
// key factory for one exposed resource.
func tsResourceSection(b *bytes.Buffer, r tsResource) {
	fmt.Fprintf(b, "\n// %s\n", tsRule(r.path))

	// Layer 2: the vocabulary. Each of these is a closed union, so a column
	// that did not opt in to a capability has no spelling that compiles.
	fmt.Fprintf(b, "\n/** Columns `select` may name. The primary key is always returned. */\n")
	fmt.Fprintf(b, "export type %sColumn =%s;\n", r.typeName, tsUnion(r.selectable))

	if len(r.sortable) > 0 && !r.singleton() {
		terms := make([]string, 0, len(r.sortable)*2)
		for _, name := range r.sortable {
			terms = append(terms, name, "-"+name)
		}
		fmt.Fprintf(b, "\n/** Sortable columns, with their descending forms. */\n")
		fmt.Fprintf(b, "export type %sSort =%s;\n", r.typeName, tsUnion(terms))
	}

	if r.hasExpand() {
		names := make([]string, len(r.relations))
		for i, rel := range r.relations {
			names[i] = rel.name
		}
		fmt.Fprintf(b, "\n/** Relations `expand` may name. */\n")
		fmt.Fprintf(b, "export type %sExpand =%s;\n", r.typeName, tsUnion(names))
	}

	// A singleton has no collection, so it has no filter vocabulary either: its
	// one GET rejects every query parameter but ?expand. Emitting the union
	// anyway would offer a client a typed way to write a request that 400s.
	if !r.singleton() {
		fmt.Fprintf(b, "\n/**\n * Filter conditions, one property per filterable column.\n *\n")
		fmt.Fprint(b, " * A bare value is equality; an object names operators. The operator set is\n")
		fmt.Fprint(b, " * narrowed by column type, so a pattern match against a number and a null\n")
		fmt.Fprint(b, " * test against a non-nullable column do not compile.\n */\n")
		// A type alias rather than an interface, so that it satisfies the encoder's
		// Record<string, unknown>: TypeScript gives an object type alias an
		// implicit index signature and an interface none.
		fmt.Fprintf(b, "export type %sWhere = {\n", r.typeName)
		for _, d := range r.filterable {
			fmt.Fprintf(b, "  %s?: %s;\n", tsProp(r.n(d.Name)), tsCondType(r.typeName, d))
		}
		fmt.Fprintln(b, "};")
	}

	tsParamTypes(b, r)
	tsRowType(b, r)
	tsWriteResultType(b, r)
	tsTransport(b, r)
	tsKeys(b, r)
}

func tsParamTypes(b *bytes.Buffer, r tsResource) {
	if r.singleton() {
		tsItemParamType(b, r)
		return
	}
	fmt.Fprintf(b, "\n/** Parameters for `GET %s`. */\n", r.path)
	fmt.Fprintf(b, "export interface %sListParams%s {\n", r.typeName, tsNarrowingParams(r))
	fmt.Fprintf(b, "  where?: %sWhere;\n", r.typeName)
	if r.searchable {
		fmt.Fprint(b, "  /** Case-insensitive substring match, fanned out over the searchable columns. */\n")
		fmt.Fprint(b, "  search?: string;\n")
	}
	if len(r.sortable) > 0 {
		fmt.Fprintf(b, "  /** Ordering, most significant first. */\n  sort?: %sSort | readonly %sSort[];\n", r.typeName, r.typeName)
	}
	fmt.Fprint(b, "  /** Columns to return. Omitted columns are absent from the response, and the\n")
	fmt.Fprint(b, "   * response type narrows to match. */\n")
	fmt.Fprint(b, "  select?: readonly S[];\n")
	if r.hasExpand() {
		fmt.Fprint(b, "  expand?: readonly E[];\n")
	}
	fmt.Fprint(b, "  page?: number;\n  per_page?: number;\n")
	if r.pk != "" {
		fmt.Fprint(b, "  /** Resume after a `next_cursor` from a previous response. Cannot be combined\n")
		fmt.Fprint(b, "   * with `page`, and is only valid for the `sort` it was issued under. */\n")
		fmt.Fprint(b, "  cursor?: string;\n")
	}
	fmt.Fprint(b, "  /** Ask for a total row count, which costs a second query. */\n  count?: 'exact';\n")
	fmt.Fprint(b, "  /** Parameters this vocabulary cannot express, appended verbatim. Reaching for\n")
	fmt.Fprint(b, "   * it often means the typed layer is in the wrong place — ADR-0028 says so and\n")
	fmt.Fprint(b, "   * names it as the signal to watch. */\n")
	fmt.Fprint(b, "  params?: Record<string, string | readonly string[]>;\n")
	fmt.Fprintln(b, "}")

	if r.readsOne() {
		tsItemParamType(b, r)
	}
}

// tsItemParamType emits the single-row parameter type, which is the same for
// both single-row shapes: the item endpoint takes ?expand or nothing at all.
func tsItemParamType(b *bytes.Buffer, r tsResource) {
	fmt.Fprintf(b, "\n/**\n * Parameters for `GET %s`.\n *\n", r.itemRoute())
	fmt.Fprint(b, " * There is no `select` here: the item endpoint rejects unknown query\n")
	fmt.Fprint(b, " * parameters and does not declare one.\n */\n")
	if r.hasExpand() {
		fmt.Fprintf(b, "export interface %sGetParams<E extends %sExpand = never> {\n", r.typeName, r.typeName)
		fmt.Fprint(b, "  expand?: readonly E[];\n}\n")
		return
	}
	fmt.Fprintf(b, "export type %sGetParams = Record<string, never>;\n", r.typeName)
}

// tsNarrowingParams is the generic parameter list a params type carries: the
// projection, and the expansion where there is one.
func tsNarrowingParams(r tsResource) string {
	if r.hasExpand() {
		return fmt.Sprintf("<S extends %sColumn = %sColumn, E extends %sExpand = never>",
			r.typeName, r.typeName, r.typeName)
	}
	return fmt.Sprintf("<S extends %sColumn = %sColumn>", r.typeName, r.typeName)
}

func tsNarrowingArgs(r tsResource, s, e string) string {
	if r.hasExpand() {
		return "<" + s + ", " + e + ">"
	}
	return "<" + s + ">"
}

// tsRowType emits the response type, narrowed by the projection and widened by
// whatever was expanded.
func tsRowType(b *bytes.Buffer, r tsResource) {
	fmt.Fprintf(b, "\n/**\n * A %s as one request asked for it: the selected columns, plus the relations\n", r.typeName)
	fmt.Fprint(b, " * it expanded.\n */\n")
	fmt.Fprintf(b, "export type %sRow%s =\n", r.typeName, tsNarrowingParams(r))

	pick := fmt.Sprintf("Pick<%s, S>", r.typeName)
	if r.pk != "" {
		// The server adds the primary key back to any projection that dropped
		// it, since a row that cannot address itself is of little use. The type
		// says the same.
		pick = fmt.Sprintf("Pick<%s, S | %s>", r.typeName, tsString(r.pk))
	}
	fmt.Fprintf(b, "  %s", pick)
	for _, rel := range r.relations {
		fmt.Fprintf(b, "\n  & (%s extends E ? { %s: %s } : unknown)",
			tsString(rel.name), tsProp(rel.name), tsRelationType(rel))
	}
	fmt.Fprintln(b, ";")
}

// tsWriteResultType emits the type create<Row> and update<Row> return, when
// it is not the plain row type.
//
// A read and a write stopped being the same shape the moment a column
// declared Needs: mutate.go has no per-request bind to resolve that column's
// expression with, so its RETURNING and the JSON response both leave the key
// out (ADR-0041, #163) — but the row type still declares it NotNull, which is
// a claim only a read can make good on. Typing create/update as returning the
// row type therefore types a key as present that is not there, and
// TypeScript reports nothing, because the type itself is the thing that
// lied (#188).
//
// A distinct name rather than `Omit<Row, 'needsColumn'>` at each call site:
// the call sites would have to be told by hand which keys to drop, and a
// column that later drops Needs would leave a stale Omit silently widening
// the response type instead of the compile error a mismatch should be. This
// type is generated from the same declarations the row type is, so it can
// only ever agree with them.
func tsWriteResultType(b *bytes.Buffer, r tsResource) {
	if !r.needsWriteResult() {
		return
	}
	fmt.Fprintf(b, "\n/**\n * A %s as create or update leaves it: every column the resource serves,\n", r.typeName)
	fmt.Fprint(b, " * minus the ones behind `Needs(...)`. A write has no per-request bind to\n")
	fmt.Fprint(b, " * resolve those with, so the key is absent from the response — the type\n")
	fmt.Fprint(b, " * says so instead of promising a value that is not there (ADR-0041).\n */\n")
	fmt.Fprintf(b, "export type %sWriteResult = Omit<%s,%s>;\n", r.typeName, r.typeName, tsUnion(r.needsColumns))
}

func tsTransport(b *bytes.Buffer, r tsResource) {
	name := r.typeName

	if r.ops.Has(schema.OpList) {
		fmt.Fprintf(b, "\n/** `GET %s` — the filtered, sorted, paged collection. */\n", r.path)
		fmt.Fprintf(b, "export function list%s%s(\n", r.plural, tsNarrowingParams(r))
		fmt.Fprint(b, "  request: Transport,\n")
		fmt.Fprintf(b, "  params: %sListParams%s = {},\n", name, tsNarrowingArgs(r, "S", "E"))
		fmt.Fprint(b, "  signal?: AbortSignal,\n")
		fmt.Fprintf(b, "): Promise<Page<%sRow%s>> {\n", name, tsNarrowingArgs(r, "S", "E"))
		fmt.Fprintf(b, "  return request({ method: 'GET', path: %s, query: encodeListQuery(params), signal });\n}\n", tsString(r.path))
	}

	if r.readsOne() {
		generic, args, params := "", "", ""
		if r.hasExpand() {
			generic = fmt.Sprintf("<E extends %sExpand = never>", name)
			args = fmt.Sprintf("<%sColumn, E>", name)
			params = fmt.Sprintf("  params: %sGetParams<E> = {},\n", name)
		} else {
			args = fmt.Sprintf("<%sColumn>", name)
			params = fmt.Sprintf("  params: %sGetParams = {},\n", name)
		}
		if r.singleton() {
			fmt.Fprintf(b, "\n/** `GET %s` — the caller's own row. There is no id to pass: the resource\n", r.path)
			fmt.Fprint(b, " * holds one row per caller and the server settles which. */\n")
		} else {
			fmt.Fprintf(b, "\n/** `GET %s/{id}` — one row by primary key. */\n", r.path)
		}
		fmt.Fprintf(b, "export function get%s%s(\n", name, generic)
		fmt.Fprint(b, "  request: Transport,\n")
		if !r.singleton() {
			fmt.Fprint(b, "  id: string | number,\n")
		}
		fmt.Fprint(b, params)
		fmt.Fprint(b, "  signal?: AbortSignal,\n")
		fmt.Fprintf(b, "): Promise<%sRow%s> {\n", name, args)
		if r.singleton() {
			fmt.Fprintf(b, "  return request({ method: 'GET', path: %s, query: encodeItemQuery(params), signal });\n}\n", tsString(r.path))
		} else {
			fmt.Fprintf(b, "  return request({ method: 'GET', path: itemPath(%s, id), query: encodeItemQuery(params), signal });\n}\n", tsString(r.path))
		}
	}

	if r.ops.Has(schema.OpCreate) {
		fmt.Fprintf(b, "\n/** `POST %s` — create a row. */\n", r.path)
		fmt.Fprintf(b, "export function create%s(request: Transport, body: %sCreate, signal?: AbortSignal): Promise<%s> {\n",
			name, name, r.writeResultType())
		fmt.Fprintf(b, "  return request({ method: 'POST', path: %s, body, signal });\n}\n", tsString(r.path))
	}

	if r.ops.Has(schema.OpUpdate) && len(bodyFields(r.table, forUpdate)) > 0 {
		fmt.Fprintf(b, "\n/** `PATCH %s` — write the columns the body names, and no others. */\n", r.itemRoute())
		if r.singleton() {
			fmt.Fprintf(b, "export function update%s(request: Transport, body: %sPatch, signal?: AbortSignal): Promise<%s> {\n",
				name, name, r.writeResultType())
			fmt.Fprintf(b, "  return request({ method: 'PATCH', path: %s, body, signal });\n}\n", tsString(r.path))
		} else {
			fmt.Fprintf(b, "export function update%s(request: Transport, id: string | number, body: %sPatch, signal?: AbortSignal): Promise<%s> {\n",
				name, name, r.writeResultType())
			fmt.Fprintf(b, "  return request({ method: 'PATCH', path: itemPath(%s, id), body, signal });\n}\n", tsString(r.path))
		}
	}

	if r.ops.Has(schema.OpDelete) {
		fmt.Fprintf(b, "\n/** `DELETE %s`. */\n", r.itemRoute())
		if r.singleton() {
			fmt.Fprintf(b, "export function delete%s(request: Transport, signal?: AbortSignal): Promise<void> {\n", name)
			fmt.Fprintf(b, "  return request({ method: 'DELETE', path: %s, signal });\n}\n", tsString(r.path))
		} else {
			fmt.Fprintf(b, "export function delete%s(request: Transport, id: string | number, signal?: AbortSignal): Promise<void> {\n", name)
			fmt.Fprintf(b, "  return request({ method: 'DELETE', path: itemPath(%s, id), signal });\n}\n", tsString(r.path))
		}
	}

	tsActionFunctions(b, r)
}

// tsKeys emits the query-key factory.
//
// It is here rather than in the TanStack file because a key is a plain array
// and a change-feed subscriber needs one without needing a query client. The
// reason it exists at all is that two hand-written lists of invalidation keys
// drift, and one list cannot.
func tsKeys(b *bytes.Buffer, r tsResource) {
	fmt.Fprintf(b, "\n/**\n * Cache keys for %s. Deriving them is what keeps a mutation's invalidation\n", r.path)
	fmt.Fprint(b, " * and a change-feed subscriber's invalidation from being two lists that can\n")
	fmt.Fprint(b, " * disagree.\n */\n")
	fmt.Fprintf(b, "export const %sKeys = {\n", r.ident)
	fmt.Fprintf(b, "  all: () => [%s] as const,\n", tsString(r.table.Name()))
	// A singleton has one row and no collection, so `list`, `infinite` and a
	// keyed `detail` would all name routes it does not serve. `all` stays,
	// because that is the key a change-feed subscriber invalidates by table.
	if r.singleton() {
		fmt.Fprintf(b, "  single: (params: unknown = {}) => [%s, 'single', params] as const,\n", tsString(r.table.Name()))
		fmt.Fprintln(b, "};")
		return
	}
	fmt.Fprintf(b, "  lists: () => [%s, 'list'] as const,\n", tsString(r.table.Name()))
	fmt.Fprintf(b, "  list: (params: unknown = {}) => [%s, 'list', params] as const,\n", tsString(r.table.Name()))
	// The prefix factories come in pairs — lists/list, details/detail — and
	// `infinite` was the one that had no prefix sibling. The subscriber below
	// needs it: invalidating a table's infinite walks without it means reaching
	// for `all`, which also throws away every other row's detail query.
	fmt.Fprintf(b, "  infinites: () => [%s, 'infinite'] as const,\n", tsString(r.table.Name()))
	fmt.Fprintf(b, "  infinite: (params: unknown = {}) => [%s, 'infinite', params] as const,\n", tsString(r.table.Name()))
	fmt.Fprintf(b, "  details: () => [%s, 'detail'] as const,\n", tsString(r.table.Name()))
	fmt.Fprintf(b, "  detail: (id: string | number, params: unknown = {}) => [%s, 'detail', String(id), params] as const,\n", tsString(r.table.Name()))
	fmt.Fprintln(b, "};")
}

// tsChangeFeed emits the subscriber: the table index, the derivation from one
// change to the keys it invalidates, and the EventSource wiring that turns the
// two into a refetch.
//
// This is the half of the change feed that lives in the client. The feed sends
// the address of a change and never the row ([ADR-0045]), so a subscriber's
// whole job is to map {table, key} onto the cached queries that read it — and
// that map is derivable from the schema, where the invalidation a *mutation*
// performs is not. So this is generated and `onSuccess` is not, which is the
// same line the queries file draws.
//
// [ADR-0045]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#the-stream-is-a-seam
func tsChangeFeed(b *bytes.Buffer, resources []tsResource) {
	if len(resources) == 0 {
		return
	}
	fmt.Fprintf(b, "\n// %s\n", tsRule("change feed"))
	fmt.Fprint(b, "\n/**\n * Key factories by table name, for a subscriber that receives a table and a\n")
	fmt.Fprint(b, " * row key and has to decide what to refetch.\n */\n")
	fmt.Fprint(b, "export const keysByTable = {\n")
	for _, r := range resources {
		fmt.Fprintf(b, "  %s: %sKeys,\n", tsProp(r.table.Name()), r.ident)
	}
	fmt.Fprint(b, "} as const;\n")
	fmt.Fprint(b, "\n/** A table this client serves. */\n")
	fmt.Fprint(b, "export type TableName = keyof typeof keysByTable;\n")

	// One entry per table rather than one rule applied to all of them, because
	// the rule is not the same for all of them: a singleton has no collection
	// and no row to address, so the only key it has is its own.
	fmt.Fprint(b, "\n/**\n * The keys one change invalidates, per table.\n *\n")
	fmt.Fprint(b, " * A keyed event names one row, so it invalidates that row's detail queries\n")
	fmt.Fprint(b, " * plus the lists and infinite walks it may have moved in or out of — not\n")
	fmt.Fprint(b, " * every other row's detail. A keyless one invalidates the table, which is\n")
	fmt.Fprint(b, " * what an event nobody could attribute to a single row asks for.\n */\n")
	fmt.Fprint(b, "const changeKeysByTable: Record<TableName, (key: string) => readonly (readonly unknown[])[]> = {\n")
	for _, r := range resources {
		if r.singleton() {
			fmt.Fprintf(b, "  %s: () => [%sKeys.all()],\n", tsProp(r.table.Name()), r.ident)
			continue
		}
		fmt.Fprintf(b, "  %s: (key) =>\n", tsProp(r.table.Name()))
		fmt.Fprintf(b, "    key === ''\n")
		fmt.Fprintf(b, "      ? [%sKeys.all()]\n", r.ident)
		fmt.Fprintf(b, "      : [%sKeys.lists(), %sKeys.infinites(), %sKeys.detail(key)],\n", r.ident, r.ident, r.ident)
	}
	fmt.Fprint(b, "};\n")

	fmt.Fprint(b, "\n/** One change to a table this client serves. */\n")
	fmt.Fprint(b, "export interface TableChange {\n")
	fmt.Fprint(b, "  /** The table that changed, narrowed to the ones this client serves. */\n")
	fmt.Fprint(b, "  table: TableName;\n")
	fmt.Fprint(b, "  /** The row's primary key, or empty when the whole table is invalidated. */\n")
	fmt.Fprint(b, "  key: string;\n")
	fmt.Fprint(b, "  op: ChangeOp;\n")
	fmt.Fprint(b, "  /** The cache keys that read what changed. Hand each to invalidateQueries. */\n")
	fmt.Fprint(b, "  keys: readonly (readonly unknown[])[];\n")
	fmt.Fprint(b, "}\n")

	fmt.Fprint(b, "\n/** Whether a table name off the wire is one this client serves. */\n")
	fmt.Fprint(b, "export function isTableName(table: string): table is TableName {\n")
	fmt.Fprint(b, "  return Object.hasOwn(changeKeysByTable, table);\n")
	fmt.Fprint(b, "}\n")

	fmt.Fprint(b, "\n/**\n * The cache keys one change invalidates — empty for a table this client does\n")
	fmt.Fprint(b, " * not serve.\n *\n")
	fmt.Fprint(b, " * Exported for a subscriber whose events arrive by some other route than the\n")
	fmt.Fprint(b, " * endpoint: a socket a gateway relays, a service worker, a test.\n */\n")
	fmt.Fprint(b, "export function changeKeys(event: ChangeEvent): readonly (readonly unknown[])[] {\n")
	fmt.Fprint(b, "  return isTableName(event.table) ? changeKeysByTable[event.table](event.key) : [];\n")
	fmt.Fprint(b, "}\n")

	fmt.Fprint(b, "\n/**\n * Subscribes to the change feed, resolving each event to the keys it\n")
	fmt.Fprint(b, " * invalidates. Returns the function that closes the stream.\n *\n")
	fmt.Fprint(b, " *     const stop = subscribeChanges('/events', {\n")
	fmt.Fprint(b, " *       onChange: ({ keys }) => keys.forEach((queryKey) => qc.invalidateQueries({ queryKey })),\n")
	fmt.Fprint(b, " *       onReset: () => qc.invalidateQueries(),\n")
	fmt.Fprint(b, " *     });\n *\n")
	fmt.Fprint(b, " * An event naming a table this client does not serve is dropped: a client\n")
	fmt.Fprint(b, " * generated from one module of a schema receives the other modules' events\n")
	fmt.Fprint(b, " * too, and nothing here displays them. A reset is not dropped — it means the\n")
	fmt.Fprint(b, " * stream could not be resumed, so what is on display is stale whatever it is\n")
	fmt.Fprint(b, " * showing.\n */\n")
	fmt.Fprint(b, "export function subscribeChanges(\n")
	fmt.Fprint(b, "  url: string,\n")
	fmt.Fprint(b, "  options: ChangeStreamOptions<TableChange> = {},\n")
	fmt.Fprint(b, "): () => void {\n")
	fmt.Fprint(b, "  const onChange = options.onChange;\n")
	fmt.Fprint(b, "  return subscribeEvents(url, {\n")
	fmt.Fprint(b, "    ...options,\n")
	fmt.Fprint(b, "    onChange:\n")
	fmt.Fprint(b, "      onChange === undefined\n")
	fmt.Fprint(b, "        ? undefined\n")
	fmt.Fprint(b, "        : (event) => {\n")
	fmt.Fprint(b, "            if (!isTableName(event.table)) return;\n")
	fmt.Fprint(b, "            onChange({ ...event, table: event.table, keys: changeKeys(event) });\n")
	fmt.Fprint(b, "          },\n")
	fmt.Fprint(b, "  });\n")
	fmt.Fprint(b, "}\n")
}

// tsQueriesSection emits the TanStack factories for one resource.
func tsQueriesSection(b *bytes.Buffer, r tsResource) {
	name := r.typeName
	fmt.Fprintf(b, "\n/**\n * Read options for %s, bound to a transport.\n *\n", r.path)
	fmt.Fprint(b, " * `queryOptions` objects rather than hooks: an options object is spread and\n")
	fmt.Fprint(b, " * overridden — `{ ...queries.list(p), staleTime: 30_000 }` — where a hook is\n")
	fmt.Fprint(b, " * copied out and edited, which is the signal that a seam is in the wrong\n")
	fmt.Fprint(b, " * place.\n */\n")
	fmt.Fprintf(b, "export function %sQueries(request: Transport) {\n", r.ident)
	fmt.Fprint(b, "  return {\n")

	if r.ops.Has(schema.OpList) {
		fmt.Fprintf(b, "    list: %s(params: %sListParams%s = {}) =>\n",
			tsNarrowingParams(r), name, tsNarrowingArgs(r, "S", "E"))
		fmt.Fprint(b, "      queryOptions({\n")
		fmt.Fprintf(b, "        queryKey: %sKeys.list(params),\n", r.ident)
		fmt.Fprintf(b, "        queryFn: ({ signal }) => list%s(request, params, signal),\n", r.plural)
		fmt.Fprint(b, "      }),\n")

		if r.pk != "" {
			// Cursor paging is what infiniteQueryOptions already wants:
			// getNextPageParam returns next_cursor, and undefined when the
			// response omits it. `page` and `cursor` are two answers to where a
			// page starts, so the params type here has neither — the factory
			// owns the paging.
			fmt.Fprintf(b, "    infinite: %s(params: Omit<%sListParams%s, 'page' | 'cursor'> = {}) =>\n",
				tsNarrowingParams(r), name, tsNarrowingArgs(r, "S", "E"))
			fmt.Fprint(b, "      infiniteQueryOptions({\n")
			fmt.Fprintf(b, "        queryKey: %sKeys.infinite(params),\n", r.ident)
			fmt.Fprintf(b, "        queryFn: ({ pageParam, signal }) => list%s(request, { ...params, cursor: pageParam }, signal),\n", r.plural)
			fmt.Fprint(b, "        initialPageParam: undefined as string | undefined,\n")
			fmt.Fprint(b, "        getNextPageParam: (last) => last.next_cursor,\n")
			fmt.Fprint(b, "      }),\n")
		}
	}

	// A singleton's read is named `single` rather than `detail`, because there is
	// nothing to detail *from*: no list precedes it and no id selects it.
	if r.singleton() {
		if r.hasExpand() {
			fmt.Fprintf(b, "    single: <E extends %sExpand = never>(params: %sGetParams<E> = {}) =>\n", name, name)
		} else {
			fmt.Fprintf(b, "    single: (params: %sGetParams = {}) =>\n", name)
		}
		fmt.Fprint(b, "      queryOptions({\n")
		fmt.Fprintf(b, "        queryKey: %sKeys.single(params),\n", r.ident)
		fmt.Fprintf(b, "        queryFn: ({ signal }) => get%s(request, params, signal),\n", name)
		fmt.Fprint(b, "      }),\n")
	} else if r.ops.Has(schema.OpRead) {
		if r.hasExpand() {
			fmt.Fprintf(b, "    detail: <E extends %sExpand = never>(id: string | number, params: %sGetParams<E> = {}) =>\n", name, name)
		} else {
			fmt.Fprintf(b, "    detail: (id: string | number, params: %sGetParams = {}) =>\n", name)
		}
		fmt.Fprint(b, "      queryOptions({\n")
		fmt.Fprintf(b, "        queryKey: %sKeys.detail(id, params),\n", r.ident)
		fmt.Fprintf(b, "        queryFn: ({ signal }) => get%s(request, id, params, signal),\n", name)
		fmt.Fprint(b, "      }),\n")
	}

	fmt.Fprint(b, "  };\n}\n")
}

// tsMutationsSection emits the TanStack mutation options for one resource.
//
// Each entry carries `mutationFn` and nothing else. The mechanical half of a
// write is derivable — the route, the body type, the response — and the half
// that matters is not: what a write invalidates depends on which views the
// application keeps, and a computed view is not a table, so its key cannot be
// generated at all (ADR-0028). A generated `onSuccess` would therefore be a
// guess, and a guess in generated code is the thing that gets copied out and
// edited. `keysByTable` next door is the mechanical half of invalidation; the
// choice of what to invalidate stays with the caller.
//
// These are values rather than functions, unlike the read factories: a query's
// parameters are known when the options are built, where a mutation's variables
// arrive at `mutate()`, so there is nothing to pass and `()` would be noise.
func tsMutationsSection(b *bytes.Buffer, r tsResource) {
	name := r.typeName
	fmt.Fprintf(b, "\n/**\n * Write options for %s, bound to a transport.\n *\n", r.path)
	fmt.Fprint(b, " * `mutationFn` and nothing else: what a write should invalidate is a policy\n")
	fmt.Fprint(b, " * this file cannot derive, so it is spread in rather than edited out —\n")
	fmt.Fprintf(b, " * `useMutation({ ...%sMutations(request).create, onSuccess })`.\n */\n", r.ident)
	fmt.Fprintf(b, "export function %sMutations(request: Transport) {\n", r.ident)
	fmt.Fprint(b, "  return {\n")

	if r.canCreate() {
		tsMutation(b, "create", fmt.Sprintf("(body: %sCreate)", name),
			fmt.Sprintf("create%s(request, body)", name))
	}
	if r.canUpdate() {
		// A singleton has no id to carry, so its variables are the body alone
		// — the same drop the transport function makes (#166). Otherwise one
		// variables object rather than two arguments, because `mutate` takes
		// exactly one.
		if r.singleton() {
			tsMutation(b, "update", fmt.Sprintf("(body: %sPatch)", name),
				fmt.Sprintf("update%s(request, body)", name))
		} else {
			tsMutation(b, "update", fmt.Sprintf("({ id, body }: { id: string | number; body: %sPatch })", name),
				fmt.Sprintf("update%s(request, id, body)", name))
		}
	}
	if r.canDelete() {
		if r.singleton() {
			tsMutation(b, "delete", "()", fmt.Sprintf("delete%s(request)", name))
		} else {
			tsMutation(b, "delete", "(id: string | number)",
				fmt.Sprintf("delete%s(request, id)", name))
		}
	}

	// A declared verb is a write with a route the schema knows (ADR-0043), so
	// it belongs here rather than being the one mutation left hand-written.
	for _, a := range r.table.Actions() {
		fn := tsActionName(r.table, a)
		var params, args []string
		if !a.IsCollection() {
			params = append(params, "id: string | number")
			args = append(args, "id")
		}
		if len(a.Body) > 0 {
			params = append(params, "body: "+tsActionInput(r.table, a))
			args = append(args, "body")
		}

		call := fmt.Sprintf("%s(request%s)", fn, prefixJoin(", ", args))
		switch len(params) {
		case 0:
			// No variables at all, so `mutate()` takes none.
			tsMutation(b, tsActionProp(a), "()", call)
		case 1:
			tsMutation(b, tsActionProp(a), "("+params[0]+")", call)
		default:
			tsMutation(b, tsActionProp(a),
				fmt.Sprintf("({ id, body }: { %s })", strings.Join(params, "; ")), call)
		}
	}

	fmt.Fprint(b, "  };\n}\n")
}

// tsMutation writes one entry of a mutations object.
func tsMutation(b *bytes.Buffer, prop, params, call string) {
	fmt.Fprintf(b, "    %s: mutationOptions({\n", tsProp(prop))
	fmt.Fprintf(b, "      mutationFn: %s => %s,\n", params, call)
	fmt.Fprint(b, "    }),\n")
}

// prefixJoin joins with sep and leads with it, so an empty list contributes
// nothing to an argument list that already has a leading argument.
func prefixJoin(sep string, parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return sep + strings.Join(parts, sep)
}

// tsType is the TypeScript type of a column in a row or a request body.
func tsType(typeName string, d *schema.FieldDesc) string {
	base := tsBaseType(typeName, d)
	if d.Nullable {
		return base + " | null"
	}
	return base
}

// tsBaseType is the TypeScript type of one column's value. An array column is
// its element type followed by [], which is the whole reason arrays are not
// declared as jsonb: `unknown` is what a jsonb column has to emit, and
// `string[]` is what this one can (ADR-0033).
func tsBaseType(typeName string, d *schema.FieldDesc) string {
	if d.Array {
		return tsElemType(typeName, d) + "[]"
	}
	return tsElemType(typeName, d)
}

// tsElemType is the type of a single value of the column's declared type,
// ignoring the array flag.
func tsElemType(typeName string, d *schema.FieldDesc) string {
	if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
		return tsEnumName(typeName, d)
	}
	switch d.Type {
	case schema.TypeSmallInt, schema.TypeInt, schema.TypeBigInt, schema.TypeReal,
		schema.TypeFloat, schema.TypeNumeric:
		// bigint is `number` with a known limit: JSON.parse loses precision
		// above 2^53, so a counter is fine and a bigint surrogate key is not.
		// Typing it `string` would be correct for the key and wrong for every
		// arithmetic use, and `bigint` does not survive JSON.parse either —
		// so the honest fix is a per-column choice the schema cannot yet
		// express. Until it can, the generated column comment says so.
		return "number"
	case schema.TypeBool:
		return "boolean"
	case schema.TypeJSON:
		return "unknown"
	default:
		// Text, varchar, uuid, bytea and the three time types all arrive as
		// strings. A timestamp is RFC 3339; typing it as Date would be a lie
		// about what JSON.parse returns.
		return "string"
	}
}

// tsCondType is the filter condition a column accepts: the operator set
// narrowed by type, which is the part an OpenAPI document cannot say.
func tsCondType(typeName string, d *schema.FieldDesc) string {
	value := tsElemType(typeName, d)
	// A timestamp is a string on the wire and a Date in most application code,
	// and the encoder accepts either, so both compile here.
	switch d.Type {
	case schema.TypeTimestamp, schema.TypeDate, schema.TypeTime:
		value += " | Date"
	}

	var extras []string
	if d.Nullable {
		extras = append(extras, "NullCheck")
	}

	// An array column takes the containment operators and none of the ordering
	// or pattern ones, which is the same set the server accepts — so
	// `{ contains: "x" }` on a tag array fails to compile rather than
	// producing a request the server answers with a 400.
	if d.Array {
		if len(extras) == 0 {
			return fmt.Sprintf("ArrayCond<%s>", value)
		}
		return fmt.Sprintf("ArrayCond<%s, %s>", value, strings.Join(extras, " & "))
	}

	// Pattern operators need a text column: the server refuses them on
	// anything else, and an enum is a string in SQL but compared by equality in
	// practice, so it is excluded here as it is in the typed facade.
	if d.Type == schema.TypeText || d.Type == schema.TypeVarchar {
		// Text keeps NullCheck last, as it was before arrays existed.
		extras = append([]string{"TextMatch"}, extras...)
	}
	// A timestamp column, and not a date one: `day` is the operator that asks
	// the question equality cannot, and on a date column equality already
	// answers it (#241).
	if d.Type == schema.TypeTimestamp {
		extras = append([]string{"DayMatch"}, extras...)
	}
	if len(extras) == 0 {
		return fmt.Sprintf("Cond<%s>", value)
	}
	return fmt.Sprintf("Cond<%s, %s>", value, strings.Join(extras, " & "))
}

func tsRelationType(rel tsRelation) string {
	if !rel.forward {
		if rel.oneToOne {
			return rel.target + " | null"
		}
		return "Collection<" + rel.target + ">"
	}
	if rel.nullable {
		return rel.target + " | null"
	}
	return rel.target
}

func tsEnumName(typeName string, d *schema.FieldDesc) string {
	return typeName + GoName(d.Name)
}

// tsProp quotes a property name that is not a plain identifier. Column names
// are snake_case and almost never need it; a name that does would otherwise
// emit invalid TypeScript.
func tsProp(name string) string {
	if isTSIdent(name) {
		return name
	}
	return tsString(name)
}

func isTSIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// tsIdent avoids emitting a reserved word as a binding name.
func tsIdent(s string) string {
	switch s {
	case "await", "break", "case", "catch", "class", "const", "continue", "debugger",
		"default", "delete", "do", "else", "enum", "export", "extends", "false",
		"finally", "for", "function", "if", "import", "in", "instanceof", "new",
		"null", "return", "super", "switch", "this", "throw", "true", "try",
		"typeof", "var", "void", "while", "with", "yield":
		return s + "_"
	}
	return s
}

// tsUnion renders the right-hand side of a union of string literals, including
// the leading space or line break, so that a long union breaks one member per
// line and a short one does not.
func tsUnion(values []string) string {
	if len(values) == 0 {
		return " never"
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = tsString(v)
	}
	if len(quoted) <= 4 {
		return " " + strings.Join(quoted, " | ")
	}
	return "\n  | " + strings.Join(quoted, "\n  | ")
}

// tsString renders a TypeScript single-quoted string literal.
func tsString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`)
	return "'" + r.Replace(s) + "'"
}

func tsOptional(optional bool) string {
	if optional {
		return "?"
	}
	return ""
}

// tsDoc emits a doc comment for a column, when the schema wrote one.
func tsDoc(b *bytes.Buffer, indent, comment string) {
	if comment == "" {
		return
	}
	fmt.Fprintf(b, "%s/** %s */\n", indent, comment)
}

// tsRule is a section divider, padded so the generated file has visible seams.
func tsRule(label string) string {
	const width = 72
	rule := strings.Repeat("-", max(3, width-len(label)-1))
	return rule + " " + label
}

func uniqueSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

const tsClientHeader = `// Code generated by github.com/jryannel/sqlb. DO NOT EDIT.
//
// The typed client for this schema: row types, request parameters, the encoder
// for the filter grammar, transport functions and cache keys.
//
// It imports nothing. The transport is injected, because session storage,
// token refresh and redirect-on-401 are the application's and are not
// derivable from a schema — the same seam the server takes by mounting onto a
// router the application built.
//
// Property names are the wire spelling, which is the column's own name unless
// the schema declared a WireCase. There is no mapping layer either way: the
// spelling is computed once, at generation time, so there is nothing between
// the response and the caller.
//
// ADR-0028, ADR-0036.
`

const tsRuntimeHeader = `// Code generated by github.com/jryannel/sqlb. DO NOT EDIT.
//
// The part of the client that does not depend on any schema: the response
// envelopes, the problem document, the transport signature and the encoder for
// the filter grammar.
//
// It is a file of its own so that an application with more than one generated
// module has one copy rather than N — one Page, one Problem, one Transport to
// wire — and so a shared pager or error boundary can be written once. A client
// re-exports everything here, so importing it from the client keeps working
// (ADR-0028, issue #110).
//
// It imports nothing, and it is derived from nothing schema-specific: two
// projects generating it produce the same bytes.
`

// tsRuntime is the part of the emitted file that does not depend on the
// schema: the response envelopes, the operator vocabulary, the transport
// interface and the URL encoder.
//
// It is inlined rather than imported from a published package, for the reason
// the models are: a client generated against the server it talks to cannot be a
// version behind it.
const tsRuntime = `
// ------------------------------------------------------------------- runtime

/** A capped set of expanded child rows. ` + "`has_more`" + ` reports truncation, which a
 * bare array could not, so a caller reading twenty of two hundred can tell. */
export interface Collection<T> {
  items: T[];
  has_more: boolean;
}

/** The body of every list response: a collection, plus where in the walk it is.
 *
 * It extends Collection rather than restating it, because an expansion returns
 * a strict subset of these fields and two hand-written shapes would be free to
 * drift. */
export interface Page<T> extends Collection<T> {
  page: number;
  per_page: number;
  /** The position to resume from, present whenever a next page exists. Prefer it
   * to ` + "`page`" + `: it costs the same at any depth and cannot skip or repeat a row
   * when the table is written to mid-walk. */
  next_cursor?: string;
  /** Total matching rows, present only when ` + "`count: 'exact'`" + ` was asked for. */
  total?: number;
}

/** One rejected parameter or field. */
export interface ProblemDetail {
  message: string;
  /** Where the problem is, e.g. ` + "`query.sort`" + `. */
  location?: string;
  value?: unknown;
  /** What would have been accepted instead, where the set is finite. This is the
   * half of an error that turns a dead end into a fix, and it is why the client
   * carries an error type at all. */
  allowed?: string[];
}

/** The RFC 9457 problem document every rejection returns. */
export interface Problem {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  errors?: ProblemDetail[];
}

/** Narrows an unknown error body to a Problem. */
export function isProblem(value: unknown): value is Problem {
  if (typeof value !== 'object' || value === null) return false;
  const p = value as Problem;
  return typeof p.status === 'number' || Array.isArray(p.errors);
}

/** The allowed values a rejection named for one parameter, e.g.
 * ` + "`allowedFor(problem, 'query.sort')`" + `. */
export function allowedFor(problem: Problem, location: string): string[] {
  return problem.errors?.find((e) => e.location === location)?.allowed ?? [];
}

/** One request, as the generated functions describe it. */
export interface ApiRequest {
  method: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  /** Path from the API root, already encoded, e.g. ` + "`/tasks/1`" + `. */
  path: string;
  /** Encoded query string without the leading ` + "`?`" + `. */
  query?: string;
  body?: unknown;
  signal?: AbortSignal;
}

/**
 * The application's request function.
 *
 * Everything not derivable from the schema lives behind this: the base URL,
 * the auth header, refresh, retry, and what a 401 does. A minimal one:
 *
 *     const request: Transport = async ({ method, path, query, body, signal }) => {
 *       const res = await fetch(` + "`${base}${path}${query ? '?' + query : ''}`" + `, {
 *         method,
 *         headers: { ...(body ? { 'content-type': 'application/json' } : {}), ...auth() },
 *         body: body === undefined ? undefined : JSON.stringify(body),
 *         signal,
 *       });
 *       if (!res.ok) throw await res.json();
 *       return res.status === 204 ? (undefined as never) : res.json();
 *     };
 */
export type Transport = <T>(request: ApiRequest) => Promise<T>;

/** Operators every column type accepts. */
export interface Comparison<V> {
  eq?: V;
  ne?: V;
  gt?: V;
  gte?: V;
  lt?: V;
  lte?: V;
  in?: readonly V[];
  nin?: readonly V[];
  between?: readonly [V, V];
}

/** Null tests, offered only by nullable columns. */
export interface NullCheck {
  isnull?: boolean;
  notnull?: boolean;
}

/** The whole-day operator, offered only by timestamp columns.
 *
 * ` + "`{ day: '2026-09-01' }`" + ` matches every row whose timestamp falls on that
 * calendar date in the database's time zone. It exists because equality cannot
 * ask this: a bare date is midnight, and a stored timestamp is almost never
 * exactly midnight, so ` + "`{ eq: '2026-09-01' }`" + ` matched nothing and said
 * nothing. The server now refuses that spelling and names this one. */
export interface DayMatch {
  day?: string;
}

/** Pattern operators, offered only by text columns. */
export interface TextMatch {
  like?: string;
  ilike?: string;
  /** Case-insensitive substring. The value is escaped, so ` + "`50%`" + ` matches that
   * literal string rather than everything. */
  contains?: string;
  startswith?: string;
  endswith?: string;
}

/** Containment operators, offered only by array columns. The substring
 * operator is absent on purpose: it belongs to text, and one name meaning two
 * things depending on the column would put back the ambiguity this client
 * exists to remove. */
export interface ArrayComparison<E> {
  /** The whole array, compared element by element. */
  eq?: readonly E[];
  ne?: readonly E[];
  /** The array contains this element. */
  has?: E;
  /** The array shares at least one element with these. */
  hasany?: readonly E[];
  /** The array contains all of these. */
  hasall?: readonly E[];
  /** The array does not contain this element.
   *
   * The ` + "`n`" + `-prefixed operators are negations, not complements: a row whose
   * column is null matches neither ` + "`has`" + ` nor ` + "`nhas`" + `. Pair one with
   * ` + "`isnull`" + ` when the null rows should be included. */
  nhas?: E;
  /** The array shares no element with these. */
  nhasany?: readonly E[];
  /** The array is missing at least one of these. */
  nhasall?: readonly E[];
}

/** One column's filter: a bare value for equality, or an operator object. */
export type Cond<V, Extra = unknown> = V | (Comparison<V> & Extra);

/** One array column's filter: a bare array for whole-array equality, or an
 * operator object. */
export type ArrayCond<E, Extra = unknown> = readonly E[] | (ArrayComparison<E> & Extra);

type Scalar = string | number | boolean | Date;

function encodeScalar(v: Scalar): string {
  return v instanceof Date ? v.toISOString() : String(v);
}

/** A member of a comma-separated list is quoted when it carries a comma or a
 * quote, which is how the server's parser reads it back whole. */
function encodeMember(v: Scalar): string {
  const s = encodeScalar(v);
  return /[,"]/.test(s) ? '"' + s.replace(/"/g, '\\"') + '"' : s;
}

function appendCond(out: URLSearchParams, column: string, cond: unknown): void {
  if (cond === undefined) return;
  // A bare null is read as a null test rather than as an equality against NULL,
  // which is not a comparison SQL would answer true to anyway.
  if (cond === null) {
    out.append(column, 'isnull');
    return;
  }
  // A bare array is a whole-array equality, the same way a bare scalar is a
  // scalar one. It is checked before the object branch because an array is an
  // object.
  if (Array.isArray(cond)) {
    out.append(column, 'eq.' + (cond as Scalar[]).map(encodeMember).join(','));
    return;
  }
  if (typeof cond !== 'object' || cond instanceof Date) {
    out.append(column, 'eq.' + encodeScalar(cond as Scalar));
    return;
  }
  // Repeating a parameter conjoins its conditions, so an object with two
  // operators becomes two parameters rather than one compound value.
  for (const [op, value] of Object.entries(cond as Record<string, unknown>)) {
    if (value === undefined || value === null) continue;
    switch (op) {
      case 'isnull':
      case 'notnull':
        if (value) out.append(column, op);
        break;
      case 'in':
      case 'nin':
      case 'hasany':
      case 'hasall':
      case 'nhasany':
      case 'nhasall':
        out.append(column, op + '.' + (value as Scalar[]).map(encodeMember).join(','));
        break;
      case 'between': {
        const [lo, hi] = value as [Scalar, Scalar];
        out.append(column, 'between.' + encodeMember(lo) + ',' + encodeMember(hi));
        break;
      }
      default:
        // eq and ne against an array column carry the whole array.
        if (Array.isArray(value)) {
          out.append(column, op + '.' + (value as Scalar[]).map(encodeMember).join(','));
          break;
        }
        out.append(column, op + '.' + encodeScalar(value as Scalar));
    }
  }
}

/** The shape encodeListQuery reads. Each resource's params type is one of
 * these with its columns and operators pinned. */
export interface ListQuery {
  where?: Record<string, unknown>;
  search?: string;
  sort?: string | readonly string[];
  select?: readonly string[];
  expand?: readonly string[];
  page?: number;
  per_page?: number;
  cursor?: string;
  count?: 'exact';
  params?: Record<string, string | readonly string[]>;
}

/**
 * Encodes list parameters into the server's query grammar.
 *
 * This is the piece hand-written clients open-code and get subtly wrong: the
 * operator prefixes, the repeated parameters that conjoin, and the quoting
 * inside a value list.
 */
export function encodeListQuery(query: ListQuery = {}): string {
  const out = new URLSearchParams();
  for (const [column, cond] of Object.entries(query.where ?? {})) appendCond(out, column, cond);
  if (query.search) out.set('search', query.search);
  if (query.sort) out.set('sort', typeof query.sort === 'string' ? query.sort : query.sort.join(','));
  if (query.select?.length) out.set('select', query.select.join(','));
  if (query.expand?.length) out.set('expand', query.expand.join(','));
  if (query.page !== undefined) out.set('page', String(query.page));
  if (query.per_page !== undefined) out.set('per_page', String(query.per_page));
  if (query.cursor !== undefined) out.set('cursor', query.cursor);
  if (query.count !== undefined) out.set('count', query.count);
  for (const [key, value] of Object.entries(query.params ?? {})) {
    for (const item of Array.isArray(value) ? value : [value as string]) out.append(key, item);
  }
  // Sorted, so that the same parameters always produce the same string — which
  // is what makes a URL comparable in a test and cacheable by a proxy.
  out.sort();
  return out.toString();
}

/** The item endpoint declares no parameters but ` + "`expand`" + `, and rejects any
 * other, so this encoder is deliberately not the list one. */
export function encodeItemQuery(query: { expand?: readonly string[] } = {}): string {
  const out = new URLSearchParams();
  if (query.expand?.length) out.set('expand', query.expand.join(','));
  return out.toString();
}

/** The path of one row: the collection, then the id, encoded.
 *
 * Exported because the per-schema client calls it, and it is the same helper
 * the Go client exports as ItemPath. */
export function itemPath(collection: string, id: string | number): string {
  return collection + '/' + encodeURIComponent(String(id));
}

// ---------------------------------------------------------- the change feed

/** What happened to one row. */
export type ChangeOp = 'create' | 'update' | 'delete';

/** One invalidation: the address of a change, never the row.
 *
 * The feed carries no row data on purpose. A payload would have to be built
 * per subscriber under that subscriber's own scope, or the query hook that
 * confines every other read of the table would not run on it — so what arrives
 * is where to look, and the refetch goes through the ordinary GET endpoints. */
export interface ChangeEvent {
  /** The SQL table the change happened in, which is what keysByTable is keyed
   * by. */
  table: string;
  /** The row's primary key, spelled the way the URL spells it — or empty,
   * which means the whole table is invalidated rather than one row. */
  key: string;
  op: ChangeOp;
}

/** The stream could not be resumed, so nothing on display can be trusted:
 * refetch everything.
 *
 * It arrives when a reconnection's position predates the retained history,
 * when that position cannot be read, and when it is ahead of the stream —
 * which is what a client from before a server restart looks like. */
export interface ResetEvent {
  reason: string;
}

/** The part of EventSource this client uses.
 *
 * An interface rather than the DOM type, so the runtime still typechecks in a
 * project without the DOM lib and a test or a polyfill can stand in for the
 * real one. */
export interface EventStream {
  addEventListener(type: string, listener: (event: { data: string }) => void): void;
  close(): void;
}

/** Opens the stream.
 *
 * Injected for the reason Transport is, and one more: EventSource cannot carry
 * an Authorization header, so a deployment that authenticates with a bearer
 * token needs a polyfill that can, and this is where it goes. A cookie session
 * needs a factory that sets withCredentials, which is the same one line. */
export type OpenStream = (url: string) => EventStream;

/** What a subscriber does with what arrives.
 *
 * Change is the event type the caller sees: the generated client narrows it to
 * the tables that client serves, and subscribeEvents leaves it as it came off
 * the wire. */
export interface ChangeStreamOptions<Change = ChangeEvent> {
  /** A row changed. Invalidate what displays it; the refetch is a GET. */
  onChange?: (event: Change) => void;
  /** The stream could not be resumed. Refetch everything on display. */
  onReset?: (event: ResetEvent) => void;
  /** A frame that did not parse, and whatever the stream itself reports.
   *
   * For EventSource the second kind includes an ordinary reconnection, which
   * is not a failure: it fires an error on every disconnect and reconnects
   * on its own. Treat it as "connection lost", not as "give up". */
  onError?: (error: unknown) => void;
  /** How to open the stream. Defaults to the platform's own EventSource. */
  open?: OpenStream;
}

/**
 * Subscribes to a sqlb change feed, and returns the function that closes it.
 *
 *     const stop = subscribeEvents('/events', {
 *       onChange: (e) => console.log(e.table, e.key, e.op),
 *       onReset: () => refetchEverything(),
 *     });
 *
 * Reconnection is EventSource's, not this function's: it resends the last id
 * it saw as Last-Event-ID, so a brief disconnection is replayed rather than
 * lost, and a long one answers with a reset event. Nothing here has to
 * remember the position.
 */
export function subscribeEvents(url: string, options: ChangeStreamOptions = {}): () => void {
  const stream = (options.open ?? openEventSource)(url);

  // One reader for both event types: the only difference is the shape the
  // payload is read as, and a second copy of the try/catch is a second place
  // for a parse failure to go unreported.
  const read = <T>(handler: ((event: T) => void) | undefined) => (frame: { data: string }) => {
    if (handler === undefined) return;
    let event: T;
    try {
      event = JSON.parse(frame.data) as T;
    } catch (error) {
      options.onError?.(error);
      return;
    }
    handler(event);
  };

  stream.addEventListener('change', read<ChangeEvent>(options.onChange));
  stream.addEventListener('reset', read<ResetEvent>(options.onReset));
  stream.addEventListener('error', (frame) => options.onError?.(frame));

  return () => stream.close();
}

/** The default OpenStream: whatever EventSource the platform has.
 *
 * Reached through globalThis rather than named directly, because naming it
 * would make the DOM lib a requirement of this file. A runtime that has none
 * is told what to pass instead rather than failing as an undefined call. */
function openEventSource(url: string): EventStream {
  const ctor = (globalThis as { EventSource?: new (url: string) => EventStream }).EventSource;
  if (ctor === undefined) {
    throw new Error(
      'sqlb: this runtime has no EventSource. Pass one in: ' +
        'subscribeEvents(url, { open: (u) => new Polyfill(u), ... })',
    );
  }
  return new ctor(url);
}
`

const tsQueriesHeader = `// Code generated by github.com/jryannel/sqlb. DO NOT EDIT.
//
// TanStack Query option factories, one per exposed resource.
//
// Option objects rather than hooks: a hook bakes in a framework and is the
// thing people copy out and edit, where an options object is spread and
// overridden. Deleting this file breaks only the call sites that used it — the
// types, the encoder and the keys next door do not depend on it.
//
// A mutation carries mutationFn and no onSuccess: what a write invalidates is
// not derivable from a schema. keysByTable is the mechanical half.
//
// ADR-0028.

`
