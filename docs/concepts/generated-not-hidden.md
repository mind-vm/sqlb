# Generated, not hidden

Code generation here follows three rules, and they are worth knowing up front
because they decide what working with sqlb feels like day to day.

1. **It is optional.** The engine has no dependency on the schema package.
2. **Its output is committed.** It is code you read in a diff, not a build step.
3. **It stops at the seam.** Anything that encodes a domain decision is
   hand-written, in files the generator does not touch.

## Optional, structurally

`sqlb` — the engine — does not import `schema`. It reflects over struct tags,
and derives column names from field names when no tag is present. So the query
builder, the filter grammar, the capabilities and the hooks all work over
structs you already have; `sqlb.Describe[T]()` supplies at startup what
reflection cannot infer.

That is not a promise, it is a dependency direction: the engine cannot quietly
grow a reliance on the DSL, because it cannot see it
([ADR-0010](../architecture.md#codegen-is-optional)). Capabilities reach the runtime
as struct tags or `Describe` calls, never as a schema import.

The practical consequence is that adoption has no cliff.
[Structs-first](../start/structs-first.md) is a real path, including over stock
[sqlc](../with-sqlc.md) output, and moving to the DSL later is a schema file plus
a generator program rather than a rewrite.

## Committed, not built

Codegen is a normal Go program that imports your schema package for its side
effects and writes files. There is no CLI to install and no version of a tool to
pin — the generator is a `main` in your repository, on the Go version your
project already uses.

Its output is checked in. That means:

- **a schema change is reviewable as a diff.** Adding `.Filterable()` to a
  column shows up as new operators in the manifest, a new parameter in the
  OpenAPI document, and a new field in the TypeScript `where` type. Whether
  that was intended is a question a reviewer can answer;
- **staleness is a gate, not a mystery.** `codegen.Check` reports which files
  would change, and belongs in CI. Generated code that is committed drifts the
  first time somebody edits a schema and forgets to regenerate, so the gate is
  the thing that makes committing it safe.

The generated artefacts, all from one `Generate` call:

| Artefact | Contents | Opt-in |
|---|---|---|
| `models_gen.go` | Row structs, with `db` and `sqlb` tags | always |
| `columns_gen.go` | The [typed column facade](../queries/typed-columns.md) and typed update statements | always |
| `rest_gen.go` | Request bodies and a `Register` function, one call per exposed table | when a table is exposed |
| `sqlb.json` | The manifest: every column, its capabilities, the operator vocabulary | always |
| `client.gen.ts`, `queries.gen.ts` | [The TypeScript client](../typescript/README.md) | `TSDir` |
| `client.gen.dart` | [The Dart client](../dart/README.md) | `DartDir` |
| `cli_gen.go` | [The cobra command tree](../cli/README.md) | `CLIDir` |

The last three are emitted into the repository that consumes them, the way
`models_gen.go` is. There is no npm package to install, and therefore no way for
the client to be a version behind the server it talks to.

## The seam

Generated code covers what the schema implies: a setter per writable column, a
handler per exposed operation. Anything that encodes a decision belongs beside
it, in a file the generator never rewrites.

The two shapes this takes, both visible in
[`example/blog`](../start/first-app.md):

**A method on a generated type.** `view_count` is `ReadOnly`, so there is no
generated `SetViewCount`; incrementing it in the database rather than
read-modify-write is a correctness decision, so `post_ext.go` writes
`AddViewCount` by hand and it composes with the generated update statement.

**An endpoint beside the generated ones.** `posts` leaves `OpDelete` out of its
exposure because the generated delete issues a real `DELETE` and this table's
deletes are meant to be soft. No hook can bridge that, so `deletes.go` registers
`DELETE /posts/{id}` on the same Huma API — two calls in `main`, not a wrapper,
because neither half owns the router.

`example/tasks` is the same pattern at scale: six generated resources mounted in
one call, and six hand-written endpoints on the same router and in the same
OpenAPI document.

## What is deliberately not generated

**Handlers.** `rest.Resource[T, C, U]` is one generic function serving every
resource. What is per-resource is the OpenAPI document, built from each column's
capabilities. So there is no generated handler to review, and no chance of
twelve that agree and a thirteenth that does not.

**Anything that is not a table.** A login endpoint is not a table, and no schema
generator will produce one. The generated TypeScript functions take a transport
as an argument and the generated CLI tree covers generated CRUD and stops, so
both compose with hand-written siblings.

**An annotation slot.** There is no open extension field on `Table` for third
parties to hang config off. The schema has exactly one annotation — `Expose`,
which is typed — and adding an untyped one would move the failure from "does not
compile" to "was ignored" ([ADR-0024](../architecture.md#no-annotation-slot)).

## Read next

- [Quickstart §2](../start/quickstart.md) — running the generator
- [Codegen reference](../reference/codegen.md) —
  every option and its default
- [ADR-0010](../architecture.md#codegen-is-optional) — the decision record
