package sqlb

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Hooks are the domain-logic seams around a model's queries and mutations.
//
// The most load-bearing one is BeforeQuery. It receives the query itself, so a
// single registration applies a constraint to every read of that model —
// including reads issued by the generated REST handlers, which is how tenant
// scoping stops being something each call site has to remember:
//
//	reg := sqlb.NewRegistry()
//	sqlb.On[Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Post]) error {
//	    org, ok := auth.OrgFrom(ctx)
//	    if !ok {
//	        return auth.ErrNoTenant
//	    }
//	    q.Where(sqlb.F("org_id").Eq(org))
//	    return nil
//	})
//	db := sqlb.New(pool).WithHooks(reg)
//
// Hooks are registered once at startup and run in registration order. A hook
// returning an error aborts the operation and the error reaches the caller
// unwrapped — but "reaches the caller" means something specific once that
// caller is [rest.Resource]: the status the request answers with is the
// status the error carries, and an error that carries none answers 500.
//
// # An error's status is its own responsibility
//
// rest classifies what it can — a [ConstraintError] by its SQLSTATE,
// [ErrBadCursor] as a bad request — and passes anything satisfying Huma's
// StatusError straight through, because that is a status application code
// already chose. Everything else is logged, not returned, and answered with a
// sentence that says only that a request could not be completed — an
// unclassified database error can name tables, columns and the statement that
// failed, which belongs in a log and nowhere a client can read it
// (docs/rest/errors.md). A hook's plain error falls into that last bucket. A
// tenant-scoping BeforeQuery that returns errors.New("no tenant") for a
// missing header is refusing the request correctly and answering 500, because
// nothing about a bare error says 400.
//
//	return huma.Error400BadRequest("X-Workspace-Id is required")
//
// Getting the status right costs one call instead of one: huma.Error400BadRequest,
// Error403Forbidden, Error404NotFound and their siblings all build a
// huma.StatusError. example/tasks/app/errors.go works the pattern through a
// package of its own, for the case where the same refusal is reachable from a
// hook, a background job and a test that never builds a router.
//
// Registration names the registry it writes to, and the handle names the
// registry it reads from. Neither reaches process-wide state, which is what
// makes the set of rules in force a property of how the application was
// assembled rather than of what happened to run an init function first.
type Hooks[T any] struct {
	mu           sync.RWMutex
	beforeQuery  []scopedFn[func(context.Context, *Builder[T]) error]
	beforeCreate []func(context.Context, *T) error
	afterCreate  []func(context.Context, *T) error
	beforeUpdate []scopedFn[func(context.Context, *Update[T]) error]
	afterUpdate  []func(context.Context, []T) error
	beforeDelete []scopedFn[func(context.Context, *Delete[T]) error]
	afterDelete  []func(context.Context, int64) error
	// afterDeleteRows is the reason a delete may run RETURNING at all. Kept
	// separate from afterDelete rather than replacing it so that the cost —
	// materialising every row a bulk delete removed — is paid only by a program
	// that asked for the rows.
	afterDeleteRows []func(context.Context, []T) error
}

// scopedFn is a registration and the name under which it may be released.
//
// An empty scope is the ordinary registration and is the reason this is a
// struct rather than a map: a hook nobody named cannot be released by anybody,
// so the zero value is the absolute rule and naming one is what makes it
// negotiable. See [Hooks.Scope].
type scopedFn[F any] struct {
	scope string
	fn    F
}

// keep reports whether this registration survives a handle that released the
// named scopes. An unnamed registration always survives.
func (s scopedFn[F]) keep(released map[string]struct{}) bool {
	if s.scope == "" || len(released) == 0 {
		return true
	}
	_, gone := released[s.scope]
	return !gone
}

// Registry holds the hook sets for a set of models, keyed by type.
//
// Every program names one. There is deliberately no process-wide default:
// hooks are the rules confining what a query may see, and a set of rules that
// arrives by ambient state is one nothing in the program is responsible for.
// Build a registry, register into it, and attach it with DB.WithHooks.
//
// This package used to have a default, and removing it was [ADR-0047]. The
// failure it existed to permit — registering hooks before building a handle,
// so every handle picks them up — is also the failure it caused: two handles
// in one process shared rules neither had asked for, and a module that stopped
// registering left the previous module's scoping silently in force.
//
// [ADR-0047]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#no-default-hook-registry
type Registry struct {
	m sync.Map // reflect.Type -> *Hooks[T]
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// noHooks is the registry a bare Executor resolves against — one that never
// has anything registered in it.
//
// It is not a default in the sense the removed one was: nothing can register
// into it, because nothing names it. It exists so that resolving hooks for an
// Executor that is not a *DB (a raw pool, a bare pgx.Tx) has an answer, and
// the answer is "no rules apply here" rather than "whatever the process
// accumulated".
//
// A statement issued against a bare pool is therefore unconfined, and that is
// visible at the call site: the alternative is a handle, and a handle carries
// its rules. Models whose scope is an obligation are protected at the other
// end — rest.Resource refuses to mount one no hook confines (ADR-0030).
var noHooks = NewRegistry()

// On returns the hook set for model T in r, creating it on first use.
//
// It takes the registry rather than reaching a default because the short,
// obvious spelling should be the safe one: a registration that does not say
// where it lands is a registration whose effect depends on what else the
// process did.
func On[T any](r *Registry) *Hooks[T] {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if v, found := r.m.Load(t); found {
		h, ok := v.(*Hooks[T])
		if !ok {
			panic(fmt.Sprintf("sqlb: hook registry holds %T for model %s", v, t))
		}
		return h
	}
	actual, _ := r.m.LoadOrStore(t, &Hooks[T]{})
	h, ok := actual.(*Hooks[T])
	if !ok {
		panic(fmt.Sprintf("sqlb: hook registry holds %T for model %s", actual, t))
	}
	return h
}

// BeforeQuery runs before every SELECT against T, including those issued by
// generated REST handlers. The hook may add predicates, joins or ordering.
//
// "Every SELECT against T" means every statement whose subject is T, and also
// every statement that reaches T as the target of another model's expansion:
// joining `lists` for `?expand=list` runs List's hooks, requalified onto the
// join alias, so a scope registered here constrains GET /lists *and* the `list`
// an expanded task carries.
//
// Two things about the expansion case are worth knowing before relying on it.
// Only the predicates are read — the hook runs against a throwaway builder, so
// an ordering or a limit it sets does not follow. And a predicate that cannot
// be requalified onto the alias, which means [RawPred] or a column belonging to
// a table the expansion did not join, fails the query rather than being
// dropped. See the expansion notes in expand.go.
//
// A refusal — no tenant, no permission — is the commonest error this hook
// returns, and a plain one answers 500 over REST; see the Hooks doc comment
// above for the status rule and the fix.
//
// # A scoping rule cannot use a subquery over another hooked model
//
// A nested SELECT does not run the hooks that confine the table it names, so it
// is refused rather than compiled unconfined (see [Subquery]). Everywhere else
// the answer is to resolve the inner query first with [Builder.Resolved], and
// here it is not available: this hook is handed the query and no executor.
//
// The two fixes that are available, in the order they are usually right:
//
//   - Denormalise the column the rule needs onto T, and make the rule a plain
//     predicate. This is often the better schema anyway — it is the same
//     argument that put the tenant column on the table in the first place.
//   - Register the rule on the other model instead, so its reads are confined
//     where they are issued rather than where they are referenced.
//
// The refusal says this too, and says it differently from the one a caller's
// own subquery gets, because the fix is not the same (#288).
func (h *Hooks[T]) BeforeQuery(fn func(context.Context, *Builder[T]) error) *Hooks[T] {
	h.register(scopedFn[func(context.Context, *Builder[T]) error]{fn: fn})
	return h
}

// Scope names the registrations made through it, so that a handle may release
// them by that name.
//
// It exists for the surface that reads the same table under a different rule:
// a storefront sees published rows and an admin panel exists to see the rest.
// Both read one model, and a BeforeQuery registered against that model confines
// every reader of it — so before this, the admin half had to be a second Go type
// over the same table, which gives up the generated model, the typed column
// facade, the manifest and the drift gate (#177).
//
//	sqlb.On[Product](reg).Scope("storefront").BeforeQuery(publishedOnly)
//	admin := db.WithoutScope("storefront")
//
// # Naming a scope is what makes it releasable
//
// An ordinary registration has no name and [DB.WithoutScope] cannot reach it,
// whatever it passes. That asymmetry is the design rather than an accident of
// it: the author of the rule decides whether the rule is negotiable, and the
// default — the short spelling, the one already in every codebase — stays
// absolute. A rule that should never be escaped is written the way it always
// was, and nothing at a mount can talk it out of applying.
//
// # The name is the rule, not the model
//
// Scope names live in the registry rather than under a type, so one name covers
// every model the rule spans. "A shopper sees the published catalog" is one rule
// over products, variants, categories and collections; registering it under one
// name on four models means a handle releases it once and the release reaches
// all four — including the ones a request arrives at through ?expand, whose
// hooks run requalified onto the join alias.
//
// # What still refuses
//
// Releasing does not get a resource past [ADR-0030]. `rest.Resource` runs its
// obligation check against the handle it will actually serve from, so a model
// declared Scoped whose every BeforeQuery has been released has nothing
// confining it and does not mount. Release one of two rules and the other still
// counts. The check is the same one, asked after the release rather than before,
// which is what keeps this from being the flag that record declined to add.
//
// [ADR-0030]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#declared-scope-is-required
func (h *Hooks[T]) Scope(name string) *ScopedHooks[T] {
	if name == "" {
		panic("sqlb: Scope called with an empty name; an unnamed registration is Hooks.BeforeQuery itself")
	}
	return &ScopedHooks[T]{hooks: h, name: name}
}

// ScopedHooks registers hooks under a scope name. See [Hooks.Scope].
//
// Only the three hooks that narrow which rows a statement addresses are here.
// BeforeCreate is not: it stamps a row on the way in rather than confining a
// set, so there is nothing for a reader to be released from, and a create that
// skipped it would write a row with no tenant rather than see more of them. A
// released read fails visibly; a released stamp fails silently.
//
// The cost of that is real and lands on the creates with no request behind them
// — a fixture, a seed, an import, a job — which cannot be released and so must
// satisfy the hook. The shape that works is a fallback inside the hook: take
// the tenant from the claims when there are claims, from the row when there are
// none, and refuse when there is neither. Returning nil unconditionally in the
// no-claims branch is the mistake it exists to avoid, and it is the one
// [#289] reports having to derive under time pressure. See the "create side"
// section of docs/queries/hooks.md for the worked version.
//
// [#289]: https://github.com/jryannel/sqlb/issues/289
type ScopedHooks[T any] struct {
	hooks *Hooks[T]
	name  string
}

// BeforeQuery registers a named [Hooks.BeforeQuery].
func (s *ScopedHooks[T]) BeforeQuery(fn func(context.Context, *Builder[T]) error) *ScopedHooks[T] {
	s.hooks.register(scopedFn[func(context.Context, *Builder[T]) error]{scope: s.name, fn: fn})
	return s
}

// BeforeUpdate registers a named [Hooks.BeforeUpdate].
func (s *ScopedHooks[T]) BeforeUpdate(fn func(context.Context, *Update[T]) error) *ScopedHooks[T] {
	s.hooks.mu.Lock()
	defer s.hooks.mu.Unlock()
	s.hooks.beforeUpdate = append(s.hooks.beforeUpdate,
		scopedFn[func(context.Context, *Update[T]) error]{scope: s.name, fn: fn})
	return s
}

// BeforeDelete registers a named [Hooks.BeforeDelete].
func (s *ScopedHooks[T]) BeforeDelete(fn func(context.Context, *Delete[T]) error) *ScopedHooks[T] {
	s.hooks.mu.Lock()
	defer s.hooks.mu.Unlock()
	s.hooks.beforeDelete = append(s.hooks.beforeDelete,
		scopedFn[func(context.Context, *Delete[T]) error]{scope: s.name, fn: fn})
	return s
}

// BeforeCreate does not exist to be called. [ScopedHooks] deliberately has no
// create hook — see the type's doc comment — but the absence of a method is
// not a message: `.BeforeCreate(...)` is the obvious fourth call to write
// beside [ScopedHooks.BeforeQuery]/[ScopedHooks.BeforeUpdate]/
// [ScopedHooks.BeforeDelete], and reaching for it either compiles into this
// panic or, on an older signature, fails with "has no field or method
// BeforeCreate" — a message that says the method is missing and not that it
// is missing on purpose ([#289]). This method exists only to carry that
// reasoning to the one place a reader is actually looking: fails at
// registration, which is startup, not at a request. See the "create side"
// section of docs/queries/hooks.md for the fallback that satisfies a
// trusted-path create instead.
//
// [#289]: https://github.com/jryannel/sqlb/issues/289
func (s *ScopedHooks[T]) BeforeCreate(func(context.Context, *T) error) *ScopedHooks[T] {
	panic("sqlb: BeforeCreate is not scopeable — a create with no request has no claims to " +
		"release, so a fixture, seed, import or job satisfies the hook itself instead; see the " +
		"\"create side\" section of docs/queries/hooks.md")
}

// register appends a BeforeQuery registration, named or not.
func (h *Hooks[T]) register(s scopedFn[func(context.Context, *Builder[T]) error]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeQuery = append(h.beforeQuery, s)
}

// BeforeCreate runs on each row before insert, and may modify it: normalising
// an email, deriving a slug, stamping an owner. It also refuses one, and an
// error with no status answers 500 over REST — see the Hooks doc comment.
func (h *Hooks[T]) BeforeCreate(fn func(context.Context, *T) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeCreate = append(h.beforeCreate, fn)
	return h
}

// AfterCreate runs on each inserted row, with database defaults populated.
// It runs inside the caller's transaction, so returning an error rolls the
// insert back.
//
// That makes it right for validation and wrong for anything the outside world
// can observe — publishing an event, enqueuing a job, invalidating a cache —
// because the transaction may still abort after the hook has announced a write
// that then never happened. Register those with [AfterCommit] instead.
func (h *Hooks[T]) AfterCreate(fn func(context.Context, *T) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterCreate = append(h.afterCreate, fn)
	return h
}

// BeforeUpdate runs before an update executes and receives the statement, so
// it can force columns (an updated_at stamp) or narrow the affected rows, or
// refuse it — with a status-bearing error, or it answers 500 over REST; see
// the Hooks doc comment.
func (h *Hooks[T]) BeforeUpdate(fn func(context.Context, *Update[T]) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeUpdate = append(h.beforeUpdate,
		scopedFn[func(context.Context, *Update[T]) error]{fn: fn})
	return h
}

// AfterUpdate receives the updated rows. Like AfterCreate it runs inside the
// transaction; side effects the outside world can see belong in [AfterCommit].
func (h *Hooks[T]) AfterUpdate(fn func(context.Context, []T) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterUpdate = append(h.afterUpdate, fn)
	return h
}

// BeforeDelete runs before a delete executes and receives the statement, and
// may refuse it — with a status-bearing error, or it answers 500 over REST;
// see the Hooks doc comment.
func (h *Hooks[T]) BeforeDelete(fn func(context.Context, *Delete[T]) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeDelete = append(h.beforeDelete,
		scopedFn[func(context.Context, *Delete[T]) error]{fn: fn})
	return h
}

// AfterDelete receives the number of rows removed. Like AfterCreate it runs
// inside the transaction; side effects the outside world can see belong in
// [AfterCommit].
//
// Use [Hooks.AfterDeleteRows] when the hook needs to know *which* rows went.
func (h *Hooks[T]) AfterDelete(fn func(context.Context, int64) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterDelete = append(h.afterDelete, fn)
	return h
}

// AfterDeleteRows receives the rows a delete removed, as they were immediately
// before it removed them.
//
// It is the delete's answer to [Hooks.AfterUpdate], and it exists because a
// count is not enough for the thing hooks are most often registered to do. A
// module that publishes a domain event on every mutation can say *how many*
// posts were deleted from an [Hooks.AfterDelete] and not *which*, and an event
// carrying no id is worse than no event: the subscriber invalidating a cache
// keyed on the row has nothing to key on, and the feed looks wired up (#144).
// The hook cannot recover the rows for itself either — a [Delete] is write-only
// for predicates, so [Hooks.BeforeDelete] has no way to ask what a statement
// addresses.
//
// # What it costs, and why it is a second name rather than a new signature
//
// The rows have to be materialised to be passed, so a delete with one of these
// registered runs `DELETE … RETURNING` and scans everything it matched. On a
// bulk delete that is real, and it is the whole reason [Hooks.AfterDelete]
// keeps its count form: nothing is added to the statement unless a hook of this
// kind is registered for T, so a program that only wants "did anything change"
// pays nothing.
//
// Like AfterCreate it runs inside the transaction, so returning an error rolls
// the delete back and side effects the outside world can see belong in
// [AfterCommit] — which is where a published event should go, and what
// [rest.PublishChanges] does with it.
//
// Both kinds run when both are registered, the count form first.
func (h *Hooks[T]) AfterDeleteRows(fn func(context.Context, []T) error) *Hooks[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterDeleteRows = append(h.afterDeleteRows, fn)
	return h
}

// RegisteredHooks reports which kinds of hook a model has, one bool per kind.
//
// It answers "did anyone write this" and deliberately not "does it do the right
// thing": a hook's body is a closure, and nothing here can tell a tenant
// predicate from a logging statement. That makes it useful for exactly one
// thing — refusing to serve a model whose schema declared an obligation that
// no registration could possibly be meeting, because there is no registration
// ([ADR-0030]).
//
// [ADR-0030]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#declared-scope-is-required
type RegisteredHooks struct {
	BeforeQuery  bool
	BeforeCreate bool
	BeforeUpdate bool
	BeforeDelete bool
}

// Registered reports which kinds of hook are registered for T.
func (h *Hooks[T]) Registered() RegisteredHooks {
	return h.registered(nil)
}

// registered is Registered against a handle that released the named scopes.
//
// A released registration does not count, which is the whole reason the
// obligation check is asked through an [Executor] rather than a registry: a
// mount that released every rule confining a Scoped model has nothing
// confining it, and reporting the registration it just switched off would let
// the release be the flag ADR-0030 declined to add.
func (h *Hooks[T]) registered(released map[string]struct{}) RegisteredHooks {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return RegisteredHooks{
		BeforeQuery:  anyKept(h.beforeQuery, released),
		BeforeCreate: len(h.beforeCreate) > 0,
		BeforeUpdate: anyKept(h.beforeUpdate, released),
		BeforeDelete: anyKept(h.beforeDelete, released),
	}
}

// anyKept reports whether any registration survives the released set.
func anyKept[F any](fns []scopedFn[F], released map[string]struct{}) bool {
	for _, s := range fns {
		if s.keep(released) {
			return true
		}
	}
	return false
}

// RegisteredFor reports which hooks are registered for T against whichever
// registry exec resolves to — the same resolution a query would get, so a
// handle carrying a scoped registry is asked about that registry rather than
// about the process default.
//
// It reads the registry at the moment it is called, which is why the check it
// exists for belongs where a resource is mounted: hooks registered afterwards
// are not visible to it, and a program that mounts before it registers is a
// program whose first request would have run unscoped anyway.
func RegisteredFor[T any](exec Executor) RegisteredHooks {
	return hooksFor[T](exec).registered(releasedFrom(exec))
}

// Reset removes every registered hook for T.
//
// It existed for tests against the process-default registry, which leaked
// registrations between cases. That registry is gone, and with it the reason:
// a test gets isolation by naming its own registry, which costs one line and
// cannot be forgotten in a teardown. Kept because clearing one model's rules
// from a registry that outlives a case is still occasionally what a test
// wants — but if you are reaching for it, a fresh NewRegistry is probably the
// answer.
func (h *Hooks[T]) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Assigning a zero Hooks would overwrite the mutex that is currently held,
	// so the slices are cleared individually.
	h.beforeQuery, h.beforeCreate, h.afterCreate = nil, nil, nil
	h.beforeUpdate, h.afterUpdate = nil, nil
	h.beforeDelete, h.afterDelete, h.afterDeleteRows = nil, nil, nil
}

func (h *Hooks[T]) runBeforeQuery(ctx context.Context, b *Builder[T], released map[string]struct{}) error {
	h.mu.RLock()
	fns := h.beforeQuery
	h.mu.RUnlock()
	for _, s := range fns {
		if !s.keep(released) {
			continue
		}
		if err := s.fn(ctx, b); err != nil {
			return err
		}
	}
	return b.err
}

// queryScoper is the type-erased view of a hook set, and it exists for exactly
// one caller: an expansion needs the target's BeforeQuery predicates, and it
// reaches the target through a *Model rather than through a type parameter, so
// it cannot name Hooks[Target] to call it.
//
// The erasure happens here rather than at the call site because this is where
// the type is still known. *Hooks[T] satisfies it for every T, so a registry
// lookup by reflect.Type can assert to it.
type queryScoper interface {
	// queryScope runs the BeforeQuery hooks against a throwaway builder and
	// returns the predicates they added, without executing anything.
	queryScope(ctx context.Context, released map[string]struct{}) ([]Pred, error)
	// scopeNames returns the scope names this hook set has registrations under,
	// which is what lets a registry enumerate them without a type parameter.
	scopeNames() []string
}

// queryScope collects what BeforeQuery would add to a query against T.
//
// The builder it runs against is discarded, so a hook that sets a limit, an
// ordering or a projection has no effect here — only its predicates are read.
// That is the right subset for an expansion: a join carries a condition, and
// the collection's order and cap are the schema's rather than a hook's.
func (h *Hooks[T]) queryScope(ctx context.Context, released map[string]struct{}) ([]Pred, error) {
	h.mu.RLock()
	fns := h.beforeQuery
	h.mu.RUnlock()
	if len(fns) == 0 {
		return nil, nil
	}
	b := Query[T]()
	for _, s := range fns {
		if !s.keep(released) {
			continue
		}
		if err := s.fn(ctx, b); err != nil {
			return nil, err
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	// filters() rather than where: a hook that called After() set a cursor
	// seek, which is a predicate about the target's own paging and has no
	// meaning inside a join. where is the set a scope is written into.
	return b.where, nil
}

// scopeNames returns the distinct scope names registered on this hook set.
func (h *Hooks[T]) scopeNames() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, s := range h.beforeQuery {
		add(s.scope)
	}
	for _, s := range h.beforeUpdate {
		add(s.scope)
	}
	for _, s := range h.beforeDelete {
		add(s.scope)
	}
	return out
}

// ScopeNames returns every scope name registered in r, across all models, in
// sorted order.
//
// It exists so that releasing a name that nothing registered can be refused
// where the refusal is useful. `rest.Resource` calls it at mount: a typo in
// Options.Unscoped would otherwise be silent, and a release that quietly does
// nothing is the failure mode of every allowlist that is not checked — the
// mount looks narrowed and serves the wide rule.
func (r *Registry) ScopeNames() []string {
	seen := map[string]bool{}
	var out []string
	r.m.Range(func(_, v any) bool {
		s, ok := v.(queryScoper)
		if !ok {
			return true
		}
		for _, name := range s.scopeNames() {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// scoperFor returns the type-erased hook set registered for t, or nil.
//
// It never creates one: an absent entry means no hook was registered, and
// materialising an empty Hooks[T] here would need the type parameter this
// function exists to do without.
func (r *Registry) scoperFor(t reflect.Type) queryScoper {
	v, found := r.m.Load(t)
	if !found {
		return nil
	}
	s, ok := v.(queryScoper)
	if !ok {
		return nil
	}
	return s
}

func (h *Hooks[T]) runBeforeCreate(ctx context.Context, row *T) error {
	h.mu.RLock()
	fns := h.beforeCreate
	h.mu.RUnlock()
	for _, fn := range fns {
		if err := fn(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hooks[T]) runAfterCreate(ctx context.Context, rows []T) error {
	h.mu.RLock()
	fns := h.afterCreate
	h.mu.RUnlock()
	if len(fns) == 0 {
		return nil
	}
	for i := range rows {
		for _, fn := range fns {
			if err := fn(ctx, &rows[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Hooks[T]) runBeforeUpdate(ctx context.Context, u *Update[T], released map[string]struct{}) error {
	h.mu.RLock()
	fns := h.beforeUpdate
	h.mu.RUnlock()
	for _, s := range fns {
		if !s.keep(released) {
			continue
		}
		if err := s.fn(ctx, u); err != nil {
			return err
		}
	}
	return u.err
}

func (h *Hooks[T]) runAfterUpdate(ctx context.Context, rows []T) error {
	h.mu.RLock()
	fns := h.afterUpdate
	h.mu.RUnlock()
	for _, fn := range fns {
		if err := fn(ctx, rows); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hooks[T]) runBeforeDelete(ctx context.Context, d *Delete[T], released map[string]struct{}) error {
	h.mu.RLock()
	fns := h.beforeDelete
	h.mu.RUnlock()
	for _, s := range fns {
		if !s.keep(released) {
			continue
		}
		if err := s.fn(ctx, d); err != nil {
			return err
		}
	}
	return d.err
}

func (h *Hooks[T]) runAfterDelete(ctx context.Context, n int64) error {
	h.mu.RLock()
	fns := h.afterDelete
	h.mu.RUnlock()
	for _, fn := range fns {
		if err := fn(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hooks[T]) runAfterDeleteRows(ctx context.Context, rows []T) error {
	h.mu.RLock()
	fns := h.afterDeleteRows
	h.mu.RUnlock()
	for _, fn := range fns {
		if err := fn(ctx, rows); err != nil {
			return err
		}
	}
	return nil
}

// wantsDeletedRows reports whether anything registered to receive the rows a
// delete removes, which is what decides whether the statement carries RETURNING.
//
// Read once per Exec rather than per hook, and read *after* BeforeDelete has
// run: a hook registered by a later goroutine mid-statement is a race the
// registry does not promise to resolve either way, and this is the moment the
// statement is compiled.
func (h *Hooks[T]) wantsDeletedRows() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.afterDeleteRows) > 0
}
