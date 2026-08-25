# How-to

The rest of this documentation is organised by surface — schema, queries, REST,
the clients, migrations — because that is how the pieces relate to each other.
This page is organised by the question you arrived with, because that is rarely
the same thing.

Nothing here is a new page. Each entry names the recipe that already exists and
the section it lives in, so a task that spans three surfaces is one link rather
than three guesses.

## Adopting it into something that already exists

| I want to | Read |
|---|---|
| Point sqlb at a database that already has tables | [Adopting a database](../migrations/adopting.md) — introspection reads it back into a schema file |
| Keep the model structs I already have | [Using your own structs](../start/structs-first.md) — the DSL and codegen are both optional |
| Keep stock sqlc output, unedited | [Alongside sqlc](../with-sqlc.md) — which queries belong on which side |
| Move one sqlc endpoint across | [Refactoring a sqlc endpoint](../refactoring-from-sqlc.md) — four stages, and you can stop after any of them |
| Find out how much of my codebase would move | [Surveying a codebase](../surveying-a-codebase.md) — counting the endpoints before committing to any of it |
| Decide whether to adopt it at all | [How sqlb compares](../comparisons.md), then [Compatibility](../compatibility.md) |

## Declaring and changing a schema

| I want to | Read |
|---|---|
| Decide what a column should let clients do | [Capabilities](../schema/capabilities.md) — nothing is filterable, sortable or selectable unless it says so |
| Point one table at another | [References and relations](../schema/references.md) |
| Ship a library that declares its own tables | [A library that ships tables](../schema/libraries.md) — declare into the host's registry, never own the migrations |
| Follow the project's schema conventions | [Schema and API practices](../best-practices.md) — each marked Enforced or Recommended |
| Ship a schema edit to a running database | [Rolling out a migration](../migrations/rollout.md) |
| Rename or drop a column without breaking clients | [Refactoring a database](../migrations/refactoring-a-database.md), then [REST compatibility](../rest/compatibility.md) — a clean rename is still a wire break |

## Writing the Go side

| I want to | Read |
|---|---|
| Put a rule in one place and have every read obey it | [Hooks](../queries/hooks.md) — a `BeforeQuery` registration constrains every read of a model |
| Get compile-time checking on column comparands | [Typed columns](../queries/typed-columns.md) |
| See the SQL a query actually produced | [Inspecting a query](../queries/inspecting.md) |
| Test code that builds queries | [Testing](../queries/testing.md) |
| Page through a large result set | [Paging](../queries/paging.md) |

## Serving it over HTTP

| I want to | Read |
|---|---|
| Use my own router or middleware | [Bringing your own router](../rest/README.md#bringing-your-own-router) |
| Find out who is calling, and scope the query to them | [Authenticating a request](../rest/auth.md) — the identity seam, then [Hooks](../queries/hooks.md) |
| Resolve a tenant or role from a header, not just the token | [Identity is one stage; enrichment is another](../rest/auth.md#identity-is-one-stage-enrichment-is-another) |
| Offer something CRUD does not cover | [Actions](../rest/actions.md) |
| Return a score, a count or a token from a verb | [What the verb answers with](../rest/actions.md#what-the-verb-answers-with) — a declared result, instead of the row or a 204 |
| Take a password, token or id list that is not a column | [A body that carries more than the row](../rest/README.md#a-body-that-carries-more-than-the-row) — declared inputs, and the hook that derives what is stored |
| Give operators a way to browse the data | [Admin](../rest/admin.md) |
| Notify another system when a row changes | [Events](../rest/events.md) and [Webhooks](../rest/webhooks.md) |
| Control what a filter may express | [Filtering](../rest/filtering.md), and the [filter grammar reference](https://jryannel.github.io/sqlb/reference/filter-grammar/) |
| Understand a rejection a client is seeing | [Errors](../rest/errors.md), and the [rejection reference](https://jryannel.github.io/sqlb/reference/rejections/) |
| Add a command for an endpoint the generator cannot see | [Adding a command of your own](../cli/README.md#adding-a-command-of-your-own) — the seam `Client.Run` leaves, and the one client both halves share |

## Leaving

| I want to | Read |
|---|---|
| Get out, with the generated code as plain Go | [Ejecting](../eject.md) — the schema as SQL, the resources as `net/http` handlers, no sqlb import left |

## Elsewhere

[Concepts](../concepts/README.md) is the reasoning the recipes assume — five
pages, one idea each. [Reference](https://jryannel.github.io/sqlb/reference/) is
the lookup material, including a
[glossary](https://jryannel.github.io/sqlb/reference/glossary/) of the words
these pages use.
