# API compatibility

`sqlb migrate` asks whether a schema edit is safe for the *database*. `sqlb
impact` asks whether it is safe for the *deployed REST client* — and the two
answers are different, and often inverted.

The REST contract a schema generates — the response fields, the filter and sort
parameters, the `?expand` relations, the create and patch bodies, the operation
set, and the declared routes: each `AddAction` verb with its body, each
`AddQuery` read with its parameters — is a pure function of the model's
capabilities
([ADR-0007](../architecture.md#generated-rest-handlers)). So sqlb is the one thing
that knows how a schema edit changes that contract, and `sqlb impact` reports it
([ADR-0039](../architecture.md#a-schema-edit-is-an-api-edit)).

## Why this is not the migration check

A migration diff reads the columns and types that produce DDL. It cannot see the
breaks that matter most to a client, because the sharpest ones produce no DDL at
all:

| Change | Migration | REST contract |
|---|---|---|
| Rename `title` → `headline` (with `RenamedFrom`) | a clean, reversible `RENAME` | **breaks** every client reading or writing `title` |
| Stop exposing a column, or drop `OpRead` | no DDL at all | **breaks** clients, invisibly to any migration |
| Add a nullable filterable column | safe | additive — a new optional parameter and field |
| `NULL` → `NOT NULL` on an exposed column | a lock hazard | readers unaffected; the **create body now requires it** |
| Drop a column | destructive, commented out | breaking — the field and its filter vanish |
| Change the schema's `WireCase` | no DDL at all | **breaks** every field of every resource at once |
| Drop an `AddAction` verb or an `AddQuery` read | no DDL at all | **breaks** the client holding that URL |

The cleanest migration sqlb can emit — a declared rename — is a hard wire break,
because the wire spelling of a column is derived from the column's name
([ADR-0036](../architecture.md#the-wire-is-the-column-name)). That is the case that
makes `impact` a check of its own rather than something the migration gate could
have told you.

The last row is the same break with no column renamed at all. `WireCase` is the
schema's one wire spelling, so flipping it respells every field everywhere while
the database is untouched — a rename with an empty migration and the widest blast
radius sqlb can produce.

## The three commands

```bash
sqlb impact -write ./schema     # record the current contract as the baseline
sqlb impact ./schema            # report how the contract has moved; always exits 0
sqlb impact -error ./schema     # the same report, but exit non-zero on a breaking change
```

`impact` diffs the current schema against a **committed baseline** — the file
`restcontract.json` beside the generated code, or wherever `Project.ContractFile`
points. That file is the concrete answer to "backward compatible *relative to
what?*", and it belongs in the repository like the migration history does. Record
it once with `-write`, review it, commit it. Re-record it deliberately whenever
you decide a contract change is intended.

A run against an unchanged schema says so and writes nothing:

```
sqlb: the REST contract is unchanged
```

A run against a changed one prints one line per change, most breaking first:

```
breaking /posts response.headline  renamed from "title"; the migration is a clean RENAME but the wire name changed
breaking /posts filter.headline    renamed from "title"; ?title=… now 400s
breaking /posts filter.status      filter removed; a request using it now 400s
sqlb: 3 contract change(s), 3 breaking
```

A change to the schema's wire spelling belongs to no single resource, so it is
reported once, without one, above everything else:

```
breaking wire                      wire case changed from verbatim to camel; every field of every resource is respelled at once (author_id is now authorId), so a deployed client reads and sends names the API no longer has
```

One line rather than one per column. Every field did change, but a report that
said so N times would bury the single edit that caused it.

## What the levels mean

- **breaking** — a request that worked now fails, or a response field a client
  relied on is gone or changed shape.
- **additive** — a new capability. Nothing a client sends or reads today changes
  meaning: a new optional filter, a new field, a newly exposed operation.
- **neutral** — a change with no client effect, such as a response field going
  from nullable to always-present.
- **unknown** — a real change whose effect depends on how a specific client
  generated its types. A widened integer is the example: a reader with a narrower
  generated type can overflow on a value the wider column now permits. `impact`
  surfaces it rather than claiming it safe, and `-error` counts it as breaking.

A single edit can be reported on two sides at once. A column going `NOT NULL` is
`neutral` in the response and `breaking` in the create body, and both lines are
printed — the reader-safe half never hides the writer-breaking half.

## Gating it in CI

Default `impact` states the delta and exits 0, because whether a break matters
depends on facts the schema does not hold — whether you have deployed clients,
and whether you version your API. Turn it into a wall where you know the answer:

```bash
sqlb impact -error ./schema
```

This project gates its own example that way:

```bash
mise run impact-check
```

which fails if the blog example's committed contract has a breaking change that
was not re-recorded. Add the same line to your CI beside `sqlb check`.

## Declared routes are part of it

A verb declared with `AddAction` and a read declared with `AddQuery` are routes
a generated client has a named method for, so withdrawing one, moving its path
or tightening its input is a break with no DDL in the change at all — the same
shape as un-exposing a column, and the reason both are diffed:

```
breaking  /tasks action.complete       action removed; POST /tasks/{id}/complete now 404s
breaking  /tasks query.overdue         query removed; GET /tasks/overdue now 404s
breaking  /tasks query.overdue.as_of   parameter removed; a client still sending it is refused, not ignored
```

That last line is sharper than its action-body equivalent for a reason worth
knowing: a declared query mounts with `RejectUnknownQueryParameters`, so a
client still sending a withdrawn parameter is **refused**, where an extra
property in a JSON body is merely undeclared.

Two things a declared route contributes are reported `neutral`, because no
client couples to them — but they are reported rather than dropped, since each
is a change in what a route *claims*. `Action.Writes` and `Action.Touches` say
what a verb may leave changed and what else it reaches; `Query.Reads` says what
a client cache should invalidate this read on. The `Reads` finding names which
direction the set moved, because the two directions differ: a deployed client
holds the *old* set, so widening the server's leaves that client under-
invalidating and showing stale data, while narrowing it only costs a refetch.

## What is in scope

`impact` speaks about the contract sqlb *generates*, and only that. Once you
write a custom handler, reshape a response in a hook, version your own `/v1` and
`/v2`, or put a gateway in front, the true client contract is no longer a
function of the schema — the same boundary `migrate` draws for the DDL it emits
against a production database it never reads.
