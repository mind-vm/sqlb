package rest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb"
)

// The obligation check.
//
// A generated handler is safe to trust because a BeforeQuery hook constrains
// every read it issues ([ADR-0008]). That argument has a hole in it, and the
// hole is the case where nobody registered the hook: add a table, expose it,
// forget the registration, and the resource serves every tenant's rows with a
// 200 next to them. Nothing fails, which is the worst available outcome —
// row-level security, the thing hooks are offered as an alternative to, is
// default-deny, and this was default-open.
//
// So a schema declaration that says rows are confined — schema.Field.Scoped,
// or schema.SoftDelete — becomes an obligation on the resource that exposes
// the table, and mounting one whose obligations have no hook behind them is a
// startup error ([ADR-0030]).
//
// What this checks is that a hook exists, not that it does anything. A
// BeforeQuery hook that logs and returns nil satisfies it. That is a real
// limit and it is the whole design: the alternative is executing the
// application's hooks at startup against a fabricated context to see what
// predicate falls out, which runs user code with a request that never happened
// in order to guess at its intent. One bit, honestly described, is worth more
// than a check that can be fooled in a way nobody expects.
//
// [ADR-0008]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#hooks-as-domain-seam
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required

// release applies Options.Unscoped to the handle this resource will serve from.
//
// It runs before every other check in Resource, which is the ordering that
// matters: the obligation check below asks the handle what confines the model,
// so asking it after the release is what makes a resource that released
// everything fail instead of pass. Doing it the other way round would have made
// Unscoped exactly the flag ADR-0030 declined to add.
//
// Two refusals, both at startup. A name nothing registered is a typo, and a
// release that silently does nothing leaves a mount that reads as narrowed and
// serves the wide rule. An executor that is not a *sqlb.DB carries no registry,
// so there is neither anything to release nor anything to check the name
// against, and honouring the field would be a promise nothing keeps.
func release(db sqlb.Executor, opts Options) (sqlb.Executor, error) {
	if len(opts.Unscoped) == 0 {
		return db, nil
	}
	handle, ok := db.(*sqlb.DB)
	if !ok {
		return nil, fmt.Errorf(
			"rest: %s sets Unscoped %v, but its Executor is %T rather than a *sqlb.DB;"+
				"\n  a scope is released from the registry a handle carries, and this one carries none —"+
				"\n  mount over sqlb.New(pool).WithHooks(reg)",
			opts.Path, opts.Unscoped, db)
	}
	known := map[string]bool{}
	for _, name := range handle.Hooks().ScopeNames() {
		known[name] = true
	}
	var unknown []string
	for _, name := range opts.Unscoped {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		have := handle.Hooks().ScopeNames()
		offer := "nothing is registered under a scope name at all"
		if len(have) > 0 {
			offer = "registered scopes are " + quoteAll(have)
		}
		return nil, fmt.Errorf(
			"rest: %s releases %s, which no hook registered;"+
				"\n  %s"+
				"\n  a released name that matches nothing narrows nothing, so this is refused rather than ignored;"+
				"\n  register the rule with sqlb.On[…](reg).Scope(%s), or drop it from Unscoped",
			opts.Path, quoteAll(unknown), offer, strconv.Quote(unknown[0]))
	}
	return handle.WithoutScope(opts.Unscoped...), nil
}

// obligation is one hook a resource's declarations require, and the reasons it
// is required. Reasons accumulate because two declarations can want the same
// hook, and an error naming both is more useful than an error naming one.
type obligation struct {
	hook    string
	ops     string
	because []string
}

// checkObligations reports the declarations of m that opts leaves unmet.
//
// The mapping from operation to hook is not decoration. A BeforeQuery hook
// constrains what a request can see and says nothing about what it can
// overwrite by id, so an exposed update needs its own registration — which is
// the arrangement example/tasks arrived at by hand, and this is that
// arrangement made compulsory.
func checkObligations[T any](m *sqlb.Model, exec sqlb.Executor, opts Options) error {
	return checkObligationsAs[T](m, exec, opts, opts.Ops.String())
}

// checkObligationsAs is checkObligations with the headline's description of the
// surface supplied separately.
//
// A resource is described by the operations it exposes, and for a resource that
// is the same string the requirements are derived from. An action is not: it is
// one POST whose obligations are a read's plus, when it declares a write set, an
// update's — so `Ops` there is a statement about what the envelope *does*, and
// saying "exposes read|update" of a verb route would describe a surface that is
// not mounted. The requirement and the sentence part company, so they are two
// arguments (#308).
func checkObligationsAs[T any](m *sqlb.Model, exec sqlb.Executor, opts Options, exposes string) error {
	bound := computedNeeds(m, opts.Computed)
	if m.Scope == nil && m.Soft == nil && len(bound) == 0 {
		return nil
	}

	var reads []string
	if m.Scope != nil {
		reads = append(reads, fmt.Sprintf("%s is Scoped", m.Scope.Name))
	}
	if m.Soft != nil {
		reads = append(reads, fmt.Sprintf("%s declares a soft delete", m.Soft.Name))
	}
	// A computed column whose expression takes a bind wants the same hook, and
	// for the same shape of reason: nothing writes the value, so a resource
	// mounted without one renders the expression against a bind that never
	// arrives. The query fails rather than answering falsely — which is better
	// than the scope case was — but it fails on every list, so a startup error
	// naming the column is the cheaper place to find out (ADR-0041).
	reads = append(reads, bound...)

	required := map[string]*obligation{}
	require := func(hook, ops string, because []string) {
		if len(because) == 0 {
			return
		}
		o, found := required[hook]
		if !found {
			o = &obligation{hook: hook, ops: ops}
			required[hook] = o
		}
		o.because = append(o.because, because...)
	}

	// Reads. Both declarations want this one: a scoped table wants the tenant
	// predicate, and a soft-deleted table wants deleted_at filtered back out.
	//
	// A singleton's read is the strongest case of the three. The others narrow
	// a result the client could at least name a row within; this one *is* the
	// predicate — the statement carries nothing else — so an unregistered hook
	// does not widen the answer, it chooses it at random.
	if opts.Ops.Has(OpList) || opts.Ops.Has(OpRead) {
		require("BeforeQuery", "list and read", reads)
	}
	if opts.Ops.Has(OpSingleton) {
		require("BeforeQuery", "singleton read", reads)
	}

	// Writes address a row by primary key, so the read predicate is not in the
	// statement and each needs its own. Only the tenant declaration asks for
	// these: a soft delete constrains what is visible, not what is writable,
	// and a table that means DELETE to be soft leaves OpDelete out entirely.
	var scope []string
	if m.Scope != nil {
		scope = []string{fmt.Sprintf("%s is Scoped", m.Scope.Name)}
	}
	if opts.Ops.Has(OpUpdate) {
		require("BeforeUpdate", "update", scope)
	}
	if opts.Ops.Has(OpDelete) {
		require("BeforeDelete", "delete", scope)
	}
	// A Scoped column is ReadOnly — the schema validator requires it — so it is
	// absent from the generated create body and a BeforeCreate hook is the only
	// thing that can supply it. Without one the insert reaches the database
	// with no tenant at all.
	if opts.Ops.Has(OpCreate) {
		require("BeforeCreate", "create", scope)
	}

	have := sqlb.RegisteredFor[T](exec)
	registered := map[string]bool{
		"BeforeQuery":  have.BeforeQuery,
		"BeforeCreate": have.BeforeCreate,
		"BeforeUpdate": have.BeforeUpdate,
		"BeforeDelete": have.BeforeDelete,
	}

	var missing []*obligation
	for hook, o := range required {
		if !registered[hook] {
			missing = append(missing, o)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].hook < missing[j].hook })

	// Every unmet obligation at once, with the registration that would satisfy
	// it — the same courtesy a rejected filter gets, and for the same reason:
	// one round trip per mistake is a bad way to learn a rule (ADR-0011).
	var b strings.Builder
	// "confines" still, when a scope or a soft delete is among the unmet
	// obligations, because that is what the reader is looking for. A resource
	// whose only gap is an unsupplied bind is a different sentence, and saying
	// the wrong one sends someone hunting for a tenant predicate that is
	// already there.
	what := "nothing confines"
	if onlyBinds(missing) {
		what = "nothing supplies the computed binds of"
	}
	fmt.Fprintf(&b, "rest: %s exposes %s, and %s %s", opts.Path, exposes, what, m.Type)
	for _, o := range missing {
		fmt.Fprintf(&b, "\n  %s: %s is not registered (%s)",
			o.ops, o.hook, strings.Join(dedupe(o.because), "; "))
	}
	// A release is the likeliest cause when there is one, and "register the
	// hook" is unhelpful advice to someone who registered it and then released
	// it — the registration is there, and this resource asked for it not to
	// apply. Say so, and leave the two honest ways out.
	if len(opts.Unscoped) > 0 {
		fmt.Fprintf(&b, "\n  this resource releases %s, so registrations under %s do not count toward the above",
			quoteAll(opts.Unscoped), plural(len(opts.Unscoped), "that scope", "those scopes"))
		b.WriteString("\n  release fewer of them, or expose fewer operations, or drop the declaration —" +
			"\n  a surface that releases everything confining these rows is one the declaration is not true of")
		return fmt.Errorf("%s", b.String())
	}
	b.WriteString("\n  register them on the registry this handle resolves against, before mounting;" +
		"\n  or drop the declaration, which is the honest way to say the rows are not confined")
	return fmt.Errorf("%s", b.String())
}

// onlyBinds reports whether every unmet reason is a computed column's bind, so
// that the headline can say what is actually missing.
func onlyBinds(missing []*obligation) bool {
	for _, o := range missing {
		for _, because := range o.because {
			if !strings.Contains(because, "is computed from the ") {
				return false
			}
		}
	}
	return true
}

// computedNeeds describes the per-request binds this resource's derived columns
// take, one line each, in declaration order.
//
// Only the columns the resource selects. The obligation follows the selection
// because the cost does: a model may declare a per-viewer column for one screen
// and be read by three endpoints that never project it, and making all four
// register a hook for a bind three of them never render is an obligation with
// no failure behind it (#92).
func computedNeeds(m *sqlb.Model, selected []string) []string {
	wanted := make(map[string]bool, len(selected))
	for _, name := range selected {
		wanted[name] = true
	}
	var out []string
	for _, col := range m.Derived {
		if len(col.Needs) == 0 || !wanted[col.Name] {
			continue
		}
		out = append(out, fmt.Sprintf("%s is computed from the %s bind",
			col.Name, quoteAll(col.Needs)))
	}
	return out
}

// quoteAll renders bind keys for a message: `"viewer"`, or `"viewer" and "org"`.
func quoteAll(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = strconv.Quote(k)
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}

func dedupe(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// plural picks the singular or plural form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
