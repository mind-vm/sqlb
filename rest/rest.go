package rest

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
)

// Op is a bitmask of the operations a resource exposes.
//
// It mirrors schema.Op deliberately rather than importing it. Nothing on the
// request path may import the schema package — that is what keeps the runtime
// usable without the DSL — so the exposure decision crosses the line as a
// value, not as a type.
type Op uint8

const (
	OpCreate Op = 1 << iota
	OpRead      // GET /resource/{id}
	OpUpdate
	OpDelete
	OpList // GET /resource with filter, sort, search, pagination

	// OpSingleton is GET /resource — the caller's one row, with no {id}
	// segment anywhere on the resource.
	//
	// A table keyed by its own scope column has one row per caller, and the two
	// ops on offer were both wrong for it: OpList answered a one-element
	// envelope every client unwrapped forever, and OpRead asked the client to
	// send back the tenant id the server already holds, where a mismatch is a
	// 404 meaning "you typed your own name wrong" (#166).
	//
	// It changes the resource rather than adding a route. The item path loses
	// its {id}, so OpUpdate becomes PATCH /resource and OpDelete becomes DELETE
	// /resource; OpCreate is POST /resource either way. The row all of them
	// address is the one the scope hook leaves — there is no key in the path and
	// no predicate in the statement — which is why this is refused on a model
	// with no Scoped column. Without one the read answers an arbitrary row and
	// the write reaches every row in the table, and that is the default-open
	// outcome ADR-0030 exists to close.
	//
	// OpList and OpRead are refused alongside it: the first is the same route,
	// and the second is the id-shaped question this exists to delete.
	OpSingleton
)

// CRUD is the conventional single-row operation set. Combine it with OpList
// for a fully exposed collection.
const CRUD = OpCreate | OpRead | OpUpdate | OpDelete

// Reads is the read-only resource: generated reads, hand-written writes.
//
// This is the shape an adopting application actually reaches for, and it is
// deliberate rather than unfinished. An app already has its writes, and the
// reasons they stay hand-written do not go away — in the port that motivated
// this constant, six of seven resources withheld something and no two withheld
// for the same reason: a create that writes bytes to object storage before the
// row, a row that is born in one domain verb and closed in another, a column
// whose transition *is* the publish the org gets notified about, per-field
// authorization a hook can constrain but not express.
//
// Naming it matters because with only CRUD named, this reads as a CRUD
// resource with two thirds switched off — as a library that could not serve
// the writes, rather than the most common and most deliberate mount there is
// (issue #101).
const Reads = OpRead | OpList

// Has reports whether the mask contains op.
func (o Op) Has(op Op) bool { return o&op != 0 }

// CreateBody is what a POST body must be able to do: turn itself into a row.
//
// The conversion is the body type's job rather than the handler's because only
// the body knows which of its fields were meant for which column. Codegen emits
// one of these per creatable resource; a hand-written model supplies its own.
type CreateBody[T any] interface {
	// Row builds the row to insert. Returning an error rejects the request as
	// a 422, which is where cross-field validation belongs.
	Row() (*T, error)
}

// CreateInput is the optional half of [CreateBody]: a create body carrying
// properties that are not columns reports them here.
//
// A body type implementing it is type-asserted for, the way every optional
// interface in this project is, so nothing changes for the bodies that do not.
// Codegen implements it on the body of a resource whose schema.REST declares
// CreateInput; a hand-written body implements it to reach the same seam without
// the DSL.
//
// What the handler does with the value is put it in the context, where
// [sqlb.CreateInputFrom] hands it to the BeforeCreate hook — which is where a
// plaintext PIN becomes a bcrypt digest, or a list of company ids becomes rows
// of another table. It never reaches the row: Row builds that, and a property
// that is not a column has no field on it to reach (#309).
type CreateInput interface {
	// Input returns the declared non-column properties, as whatever type the
	// body chose to carry them in. A generated body returns the generated
	// Create<Model>Input struct.
	Input() any
}

// CreateExplicit is the second optional half of [CreateBody]: a create body
// that can say which columns the request actually carried reports them here.
//
// It exists because [sqlb.Insert] omits a defaulted column holding its zero
// value — which is what stops a generated id being overwritten with zero, and
// is wrong for exactly the columns whose default disagrees with their zero.
// `Bool(...).Default(Value(true))` is that shape: a request sending false was
// answered with true, because false is also "not set" (#314). The body already
// distinguishes absent from zero in order to build the row, and this is that
// knowledge reaching the write instead of being dropped one line before it is
// needed.
//
// A body type implementing it is type-asserted for, so nothing changes for the
// bodies that do not. Codegen implements it on every body with an optional
// column; a hand-written body implements it to reach the same seam.
type CreateExplicit interface {
	// Explicit names the columns the request set, whose zero values are
	// therefore meant. Columns it does not name keep the default-omitting
	// behaviour, so a BeforeCreate hook can still fill one in.
	Explicit() []string
}

// UpdateBody is what a PATCH body must be able to do: report which columns the
// request actually named.
//
// A typed struct cannot distinguish "absent" from "zero", which is the whole
// difficulty of PATCH, so the body type reports the change set explicitly.
// Codegen emits fields as pointers and returns only the non-nil ones.
type UpdateBody interface {
	// Changes maps column name to new value for the fields the request
	// carried. An empty map is rejected as a 400 rather than run as a no-op
	// update, because it almost always means the client sent the wrong shape.
	Changes() (map[string]any, error)
}

// None stands in for a body type on a resource that does not expose the
// corresponding operation. Its methods are never called, because the operation
// is never registered.
type None[T any] struct{}

// Row satisfies CreateBody and always fails, since a resource using None does
// not expose create.
func (None[T]) Row() (*T, error) {
	return nil, errors.New("rest: this resource does not expose create")
}

// Changes satisfies UpdateBody and always fails, since a resource using None
// does not expose update.
func (None[T]) Changes() (map[string]any, error) {
	return nil, errors.New("rest: this resource does not expose update")
}

// Options describes how one resource is exposed. It restates what the schema
// declared in schema.REST, and codegen writes it from that declaration.
type Options struct {
	// Path is the collection path, e.g. "/posts". Required.
	Path string

	// Ops is the set of exposed operations. Required: a resource exposing
	// nothing is a mistake rather than a way to hide one.
	Ops Op

	// Name is the singular resource name used in operation IDs and summaries,
	// e.g. "post" gives list-posts and get-post. Defaults to the path with its
	// leading slash removed.
	Name string

	// Tag groups the operations in the OpenAPI document. Defaults to Name.
	Tag string

	// Description documents the resource. It comes from the table's comment.
	Description string

	// Pagination and filter limits. Zero means the filter package's default.
	// MaxPageSize is a hard ceiling, not a hint: a client asking for more gets
	// the maximum rather than an error.
	DefaultPageSize int
	MaxPageSize     int
	MaxFilters      int
	MaxSortTerms    int
	// MaxOffset bounds how deep ?page= and ?offset= may reach into the result
	// set. Offset paging is the one dimension of a request whose cost grows
	// with the number the client sent, so it has a ceiling like the others; a
	// request past it is refused with a message pointing at ?cursor=.
	MaxOffset int

	// DefaultSort is the ordering a list request that names no ?sort gets:
	// column names, a leading "-" for descending, most significant first.
	//
	//	DefaultSort: []string{"-pinned", "-published_at", "-created_at"}
	//
	// The direction syntax is ?sort's; the names are column names, as
	// Computed's and Columns' are.
	//
	// The other five limits above bound every dimension of a list request except
	// the one that decides what the list *is*. For many resources the ordering is
	// part of the collection's meaning rather than a client preference: a feed is
	// pinned first, then newest, and a feed in primary-key order is not the feed.
	// Without this the answer is primary-key order — declared nowhere, so every
	// caller restates the real ordering on every request, forever, and the caller
	// that forgets gets a well-formed 200 that is quietly the wrong product
	// (#165).
	//
	// Empty keeps that behaviour, so this changes only what silence means. A
	// request that sends ?sort replaces it rather than adding to it, and the
	// primary-key tiebreak is appended exactly as it is to an explicit sort, so
	// cursors are unaffected. It is not charged against MaxSortTerms: that bounds
	// what an untrusted request may ask for, and this is the resource's own.
	//
	// Every term must name a column this resource serves and that declares
	// Sortable, checked at startup for the reason Expandable and Computed are —
	// at request time a default naming an undeclared column would answer 400 to a
	// client that sent nothing wrong.
	DefaultSort []string

	// Expandable lists the relation names ?expand may name. Each must be a
	// relation the model declares — a `expands=` field beside an `expand`
	// column — and is checked at startup, because at request time an unknown
	// name would parse cleanly and answer 200 with the relation missing.
	//
	// Leaving it empty offers no ?expand at all, which is the right default: a
	// join is a cost, and a relation the schema happens to declare is not the
	// same thing as one this resource wants to serve.
	Expandable []string

	// Computed lists the computed columns this resource selects. Each must be a
	// column the model computes, and it is checked at startup for the reason
	// Expandable is.
	//
	// Leaving it empty offers none, which is the right default for the same
	// reason it is right for Expandable: a computed column is a cost, and one
	// the schema happens to declare is not the same thing as one this resource
	// wants to serve. A model is shared — the same Project is read by a list
	// screen that wants four aggregates and by an existence check that wants
	// none — so projecting every declared column charged every reader for the
	// most expensive one, and a column carrying a Needs bind failed the
	// cheapest readers outright (#92).
	//
	// A column not listed is not reachable from this resource: not in the
	// response, not filterable, not sortable, not nameable in ?select. The
	// obligation follows the selection — a resource that selects a column
	// declaring Needs still refuses to mount without a hook to supply the bind,
	// and one that does not select it no longer has to care.
	//
	// # It decides the write path too
	//
	// One list, both paths. A create and an update evaluate exactly the columns
	// named here in their RETURNING, so a resource that asked for none sends an
	// INSERT over stored columns and nothing else. Until #164 that half was not
	// narrowed by anything: every write evaluated every bind-free computed column
	// the model declared, so a store that never reads an aggregate still paid for
	// it on each patch, a create returned a value that was structurally wrong
	// because the rows it counts are written later in the same transaction, and a
	// subquery naming another module's table made the table unwritable without
	// that module present.
	//
	// A column declaring Needs is the exception on this path and is left out of a
	// write's RETURNING whether or not it is named here — a mutation has nowhere
	// to take a bind from (ADR-0041). It is left out of the write's *response*
	// too, so the key is absent rather than present holding a zero that reads as
	// a real answer (#163); the next read carries the value.
	Computed []string

	// Columns narrows this resource to the columns it names. Empty — the
	// default, and what a generated resource emits — is every column the model
	// has.
	//
	// This is the answer to two surfaces over one table (#148). A storefront and
	// an admin panel read the same products, and they differ in which columns
	// each may see: `cost_price_minor` and `internal_notes` are the reason the
	// admin resource exists and must not be within a mile of the public one.
	// Hidden cannot express that, because Hidden is a property of the model and
	// there is one model per table; Computed already established that
	// reachability is a property of the *mount*, and this is the same idea
	// applied to stored columns.
	//
	// A column not listed is not reachable from this resource at all: absent
	// from the response, absent from the SELECT the database sees, not
	// filterable, not sortable, not searched, not nameable in ?select, not
	// settable by a create or update body, and not named in the list a rejection
	// offers — that last one because a narrowed resource that advertised the
	// column it is about to refuse would leak the schema it was narrowed to
	// hide.
	//
	// Every name must be a column of the model, and the list must include the
	// primary key: it addresses rows, settles the ordering, and is what a cursor
	// is built from, so a resource without it cannot page. Both are checked at
	// startup, where the failure is a resource that will not mount rather than
	// one serving a surface nobody meant.
	//
	// What this does not do is generate the second resource. Codegen emits one
	// mount per exposed table, so the narrowed half is a hand-written
	// rest.Resource call over the generated model — the models, the typed column
	// facade, the manifest and the drift gate all still cover it, and only the
	// mount is yours. The alternative, a second model over the same table, gives
	// up all four.
	//
	// # Two things it does not narrow, and why
	//
	// **The response schema in the OpenAPI document.** It is the model's Go type,
	// registered once as a component and shared by every mount of it, so it
	// still lists the columns this resource does not serve. Runtime responses
	// omit them and every parameter follows this list; what a client generated
	// from the document gets is optional fields that are always absent. Narrowing
	// it needs a per-resource Go type, which is the generated second resource
	// that is a larger change than this one.
	//
	// **The create and update body types.** They are the caller's — C and U — so
	// a narrowed mount reusing the wide resource's bodies documents fields it
	// will not write. It will not write them: a column outside this list is
	// cleared off the row a body produced, the same way a ReadOnly one is, and a
	// PATCH naming one is refused as unknown. But the document says otherwise,
	// so a resource narrowed for disclosure usually wants Ops without the write
	// operations, or body types of its own.
	//
	// Both are worth reading as the shape of the boundary: this narrows what a
	// resource *does*, and the parts of the document that come from a Go type
	// still describe that type.
	Columns []string

	// Unscoped releases the named hook scopes for this resource, so that two
	// mounts over one model can differ in which *rows* each may reach.
	//
	// Columns above is the disclosure half of two surfaces over one table
	// (#148). This is the other half. A rule registered under a name —
	// `sqlb.On[Product](reg).Scope("storefront").BeforeQuery(publishedOnly)` —
	// confines every reader of that model, which is the point of it; naming it
	// is what lets one mount say it is the surface the rule is not about. The
	// admin panel that exists to show drafts mounts with
	// `Unscoped: []string{"storefront"}` over the same generated model, and
	// keeps the model, the typed column facade, the manifest and the drift gate
	// that a second Go type over the same table gives up (#177).
	//
	// Only a named scope can be released. An ordinary BeforeQuery has no name,
	// nothing here can reach it, and that is what keeps this from being a way to
	// turn scoping off: the author of a rule decides whether the rule is
	// negotiable, by choosing how to spell it.
	//
	// The release reaches every model this resource's statements touch,
	// including an ?expand target's own hooks, because a scope name spans the
	// models its rule spans.
	//
	// # What it does not get past
	//
	// The obligation check, which runs after the release rather than before it.
	// A model declared Scoped whose every confining rule this resource released
	// has nothing confining it and does not mount — the ADR-0030 error, naming
	// what was released. That ordering is the reason this is safe to offer:
	// ADR-0030 declined an escape hatch on the grounds that "an unused escape
	// hatch is the thing most likely to be reached for reflexively", and a hatch
	// that still refuses the case the check exists for is not that hatch.
	//
	// A name no registration claims is refused at startup, for the reason every
	// allowlist is checked: a release that quietly does nothing leaves a mount
	// that looks narrowed and serves the wide rule.
	//
	// The executor must be a *sqlb.DB. A raw pool carries no registry, so there
	// would be nothing to release and no name to check against.
	Unscoped []string

	// DisableSearch rejects ?search even when columns are searchable.
	DisableSearch bool

	// DisableTransactions runs generated writes under autocommit.
	//
	// The default — wrapping each create, update and delete in a transaction —
	// is what makes sqlb.AfterCommit reachable from a generated write. Without
	// it there is no commit for a hook to be after, so a documented feature is
	// unreachable from the writes most applications actually issue
	// ([ADR-0021](../docs/architecture.md#hooks-receive-an-event)).
	//
	// The cost is a BEGIN/COMMIT round trip per write, and a server-side
	// connection held for longer. Behind PgBouncer in transaction pooling mode
	// that is a change in occupancy rather than only in latency
	// ([ADR-0019](../docs/architecture.md#pgbouncer-in-the-path)), so this exists
	// for anyone who measures it and decides against.
	//
	// Turning it on silently stops any AfterCommit callback the resource's
	// hooks register. Read that as the reason it is phrased as a disable rather
	// than as an enable: the safe value is the zero value.
	DisableTransactions bool

	// Security is the OpenAPI security requirement every operation of this
	// resource carries — the same shape huma.Operation.Security takes, so it is
	// a list of alternatives and each alternative names schemes and their
	// scopes:
	//
	//	Security: []map[string][]string{{"bearerAuth": {}}}
	//
	// It documents; it does not enforce. Authentication is middleware on the
	// router, and it runs whether or not this is set — leaving it empty produces
	// operations that are protected and do not say so, which is what every
	// consumer of the document has to guess about.
	//
	// The generated clients do not read this, and that is not an oversight: they
	// are generated from the schema rather than from the document, and they take
	// the credential from the transport the consuming project supplies. What
	// this is for is /docs, an agent reading the spec, and anything else driven
	// by the document.
	//
	// The scheme itself is declared once on the API, not here:
	//
	//	api.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
	//	    "bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
	//	}
	Security []map[string][]string
}

func (o Options) name() string {
	if o.Name != "" {
		return o.Name
	}
	return strings.TrimPrefix(o.Path, "/")
}

func (o Options) tag() string {
	if o.Tag != "" {
		return o.Tag
	}
	return o.name()
}

func (o Options) validate() error {
	switch {
	case o.Path == "":
		return errors.New("rest: Options.Path is required")
	case !strings.HasPrefix(o.Path, "/"):
		return fmt.Errorf("rest: Options.Path %q must start with a slash", o.Path)
	case o.Ops == 0:
		return fmt.Errorf("rest: Options.Ops is empty for %s; a resource that exposes nothing should not be mounted", o.Path)
	case o.Ops.Has(OpSingleton) && o.Ops.Has(OpList):
		return fmt.Errorf("rest: %s exposes both OpSingleton and OpList, which are the same route: "+
			"GET %s cannot be the caller's row and the collection at once", o.Path, o.Path)
	case o.Ops.Has(OpSingleton) && o.Ops.Has(OpRead):
		return fmt.Errorf("rest: %s exposes both OpSingleton and OpRead; OpSingleton removes the {id} "+
			"segment from this resource, so a read by id is the question it exists to delete — drop OpRead", o.Path)
	case o.Ops == CRUD:
		return fmt.Errorf("rest: %s exposes exactly CRUD (create, read, update, delete) with no OpList, "+
			"so GET %s has no collection route and 405s — CRUD is the conventional single-row set, not "+
			"the fully-exposed collection its name suggests; combine it with OpList: CRUD|OpList", o.Path, o.Path)
	}
	return nil
}

// singleton reports whether this resource is the caller's one row.
func (o Options) singleton() bool { return o.Ops.Has(OpSingleton) }

// Resource registers the exposed operations for model T on api.
//
// T is the row type, C the create body and U the update body. A resource that
// exposes neither create nor update passes rest.None[T] for both; the types are
// still instantiated, but Huma never sees them because the operations are not
// registered, so they stay out of the OpenAPI components.
//
// Registration is the startup path, so failures are returned rather than
// panicked: a mistake here should name the resource that caused it.
func Resource[T any, C CreateBody[T], U UpdateBody](api huma.API, db sqlb.Executor, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if db == nil {
		return fmt.Errorf("rest: %s has no Executor", opts.Path)
	}

	// Released before anything else reads the handle, so that every check below
	// and every handler registered further down sees the rules this resource
	// will actually serve under — the obligation check most of all.
	db, err := release(db, opts)
	if err != nil {
		return err
	}

	b, err := bind[T](opts)
	if err != nil {
		return err
	}

	// Every single-row operation addresses a row by primary key, so a table
	// without one can only be listed. Saying so at startup is better than four
	// handlers that cannot be reached.
	//
	// A singleton is the exception: its row comes from the scope hook, so no
	// operation of it puts a key in the path or a key predicate in the
	// statement, and a table keyed only by its tenant column can be one.
	if !opts.singleton() && b.model.PK == nil && opts.Ops&(OpRead|OpUpdate|OpDelete) != 0 {
		return fmt.Errorf("rest: %s exposes %s but %s declares no primary key",
			opts.Path, opts.Ops&(OpRead|OpUpdate|OpDelete), b.model.Type)
	}

	// The whole safety argument for a singleton runs through the scope column:
	// with one, every operation is confined by the hook ADR-0030 already makes
	// compulsory; without one, the same statements are unconfined. Checked here
	// as well as in `sqlb generate`, because rest is usable without the DSL.
	if opts.singleton() && b.model.Scope == nil {
		return fmt.Errorf("rest: %s exposes %s but %s declares no Scoped column; "+
			"a singleton addresses the caller's row through the scope hook and nothing else, "+
			"so without one the read answers an arbitrary row and a write reaches every row — "+
			"tag the tenant column `sqlb:\"scope\"`, or expose OpRead and OpList instead",
			opts.Path, opts.Ops, b.model.Type)
	}

	// A schema that says these rows are confined has to be met by something
	// that confines them. See scope.go for what this does and does not prove.
	if err := checkObligations[T](b.model, db, opts); err != nil {
		return err
	}

	// Resolved once, at startup, so that an executor which cannot begin a
	// transaction is reported here rather than by the first write.
	w, err := newWriter(db, opts)
	if err != nil {
		return err
	}

	if opts.Ops.Has(OpList) {
		registerList(api, db, b)
	}
	if opts.Ops.Has(OpRead) {
		registerRead(api, db, b)
	}
	if opts.Ops.Has(OpSingleton) {
		registerSingleton(api, db, b)
	}
	if opts.Ops.Has(OpCreate) {
		registerCreate[T, C](api, w, b)
	}
	// A singleton's write operations address the same row its read does, so they
	// take the collection path and no key. See singleton.go.
	if opts.Ops.Has(OpUpdate) {
		if opts.singleton() {
			registerSingletonUpdate[T, U](api, w, b)
		} else {
			registerUpdate[T, U](api, w, b)
		}
	}
	if opts.Ops.Has(OpDelete) {
		if opts.singleton() {
			registerSingletonDelete(api, w, b)
		} else {
			registerDelete(api, w, b)
		}
	}
	return nil
}

// Must panics if err is non-nil. Generated registration code uses it, since a
// resource that cannot be mounted is a startup failure either way.
func Must(err error) {
	if err != nil {
		panic(err)
	}
}

// String renders the mask for diagnostics.
func (o Op) String() string {
	var parts []string
	for _, e := range []struct {
		op   Op
		name string
	}{
		{OpCreate, "create"}, {OpRead, "read"}, {OpUpdate, "update"},
		{OpDelete, "delete"}, {OpList, "list"}, {OpSingleton, "singleton"},
	} {
		if o.Has(e.op) {
			parts = append(parts, e.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// itemPath is the single-row path, e.g. "/posts/{id}". The template is always
// {id}, whatever the primary key column is called: the URL names the resource's
// identity, and renaming a column should not break every client.
//
// A singleton's is the collection path itself. There is one row per caller and
// the server already knows which, so there is nothing for a segment to say
// (#166).
func (o Options) itemPath() string {
	if o.singleton() {
		return o.Path
	}
	return o.Path + "/{id}"
}

const (
	statusCreated = http.StatusCreated
	statusNoBody  = http.StatusNoContent
)
