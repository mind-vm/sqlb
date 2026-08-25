# Capabilities

Every column opts in to what the outside world may do with it. A column that
does not declare a capability **cannot be reached through it — ever**, and the
failure is a 400 naming what would have worked, not a leak and not a silently
ignored parameter.

```go
schema.Text("title").Searchable().Sortable()
schema.Text("password_hash").Hidden()
```

This is the difference between sqlb and exposing your database. It is worth
understanding as a model before reading the API, because the guarantees are
sharper than "a list of allowed columns" suggests.

## The default is closed

Nothing is filterable, sortable or searchable until it says so. That direction
matters more than it looks: a default of "everything, minus a deny list" makes
every new column a decision somebody has to remember to make, and the cost of
forgetting is a column exposed. Opt-in inverts it — forgetting costs a 400 that
names the fix.

The same default applies to structs described at runtime with
[`sqlb.Describe`](../start/structs-first.md). Adopting sqlb over structs you
already have cannot widen an API by accident.

## Three tiers of "no"

The distinction between them is the part that repays attention.

**Not capable.** The column exists and the capability is absent. The rejection
says so and lists the columns that do have it:

```json
{
  "message": "column is not sortable",
  "location": "query.sort", "value": "body",
  "allowed": ["title", "status", "view_count", "published_at", "created_at"]
}
```

**`ReadOnly` / `Immutable`.** The column is readable but not writable through
REST — the database or a hook owns it. `ReadOnly` is never settable;
`Immutable` is settable at create and rejected on update. Both are enforced at
the REST boundary; Go code going through the query engine is trusted and
bypasses them.

**`Hidden`.** The column does not exist as far as the outside world is
concerned. Not in a response, not in the OpenAPI schema, not in the filter
vocabulary, and **not in the `allowed` list of a rejection**. That last one is
the point: a column reported as *forbidden* has had its existence confirmed, and
a filterable secret can be recovered a character at a time by probing. So
`Hidden` plus `Filterable` is a schema validation error rather than a
combination you can write.

`Hidden` is enforced at the projection rather than at the serialiser, so
`filter.Apply` cannot select one even by mistake — it defaults to every
non-hidden column rather than to the builder's "all mapped columns". And it
survives an `?expand` join: the target's columns are listed explicitly rather
than taken with `row_to_json`, so expansion is not a way to read a column the
resource refuses to serve.

## Rejections are data

Every problem in a request is reported at once, not one per round trip, and each
carries the allow-list that would have satisfied it
([ADR-0011](../architecture.md#actionable-errors)). A malformed request takes one
round trip to fix rather than one per mistake.

That shape exists for a specific caller: a program assembling requests against a
schema it only partly knows. "column is not sortable" is a dead end; the same
message plus the sortable columns is a fix. It is also why the same allow-list
reaches the [TypeScript client](../typescript/README.md) as a typed body and the
[Go CLI](../cli/README.md) as printed output — the guarantee is only worth
having if it survives to the last consumer in the chain.

## The combination worth knowing

`ReadOnly` on a column a hook fills is how a tenant id stays out of a client's
reach:

```go
schema.Ref("org", Org).Filterable().ReadOnly().Scoped()
```

The column is absent from both generated request bodies, so no request can name
it, and `BeforeCreate` supplies it from whatever the request authenticated as.
Both halves are live: the handler clears every read-only field before inserting,
so even a hand-written `Row()` cannot set one — and a hook still can.

`Scoped` is the third piece. It writes no predicate; it *obliges* one.
`rest.Resource` refuses to mount a model whose declarations no hook satisfies,
and names every missing registration at once. A rule that can be forgotten
entirely is not a rule, and the table somebody added last week is the case that
actually happens ([ADR-0030](../architecture.md#declared-scope-is-required)).

## Capabilities are a privacy decision

Worth stating because it is easy to treat them as an API convenience.
`Filterable` and `Searchable` are not the same choice:

An email column that is `Filterable` but deliberately *not* `Searchable` answers
"find my own record" by exact match, and refuses `?search=example.com` — not by
rejecting it, but because the search fan-out never sees the column. On a table
anyone can read, a substring match over addresses is an address-harvesting
endpoint. That is a decision about who can be enumerated, made in the schema,
where it is reviewable.

## Read next

- [Capabilities](../schema/capabilities.md) — how to declare them, and the
  patterns
- [Capability reference](../reference/capabilities.md)
  — every method and what it permits
- [ADR-0006](../architecture.md#capabilities-are-opt-in) — the decision record
