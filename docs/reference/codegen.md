# Codegen reference

Checked against `codegen/codegen.go`. The concept page is
[Generated, not hidden](../concepts/generated-not-hidden.md).

Codegen is a normal Go program that imports your schema package for its side
effects — declaring a table registers it — and writes the artefacts. There is no
CLI to install.

```go
package main

import (
    "github.com/jryannel/sqlb/codegen"
    "github.com/jryannel/sqlb/schema"

    _ "yourmodule/blogschema"
)

func main() {
    codegen.Must(codegen.Generate(codegen.Options{
        Registry: schema.DefaultRegistry(),
        Dir:      "blog",
        Package:  "blog",
    }))
}
```

## Required

| Field | Meaning |
|---|---|
| `Registry` | Supplies the tables. Usually `schema.DefaultRegistry()` |
| `Dir` | Output directory |
| `Package` | Package clause for the generated Go |

## Go output

| Field | Default | Emits |
|---|---|---|
| `ModelsFile` | `models_gen.go` | The row structs, with `db` and `sqlb` tags |
| `ColumnsFile` | `columns_gen.go` | The typed column facade and typed update statements |
| `RestFile` | `rest_gen.go` | Request bodies and a `Register` function |
| `ManifestFile` | `sqlb.json` | Every column, its capabilities, the operator vocabulary |

Set any of them to `"-"` to skip that artefact.

`RestFile` is written **only when the schema exposes at least one table**, so a
package with no REST surface does not acquire a dependency on Huma.

## Type overrides

`Types` replaces the Go type emitted for the columns each override matches —
the sqlc `overrides:` equivalent, and what lets a codebase whose ids are
`uuid.UUID` generate its models rather than describing hand-written ones.

```go
codegen.Options{
    Types: []codegen.TypeOverride{
        {Type: schema.TypeUUID, GoType: "uuid.UUID", Import: "github.com/google/uuid"},
        {Table: "invoices", Column: "amount", GoType: "decimal.Decimal",
         Import: "github.com/shopspring/decimal"},
    },
}
```

| Field | Meaning |
|---|---|
| `Type` | Match every column of a logical type |
| `Table` | Narrow to one table, by its storage name (module prefix included) |
| `Column` | Narrow to one column name |
| `GoType` | The type as it should appear in the source: `uuid.UUID` |
| `Import` | The package path providing it, or empty if it needs none |

At least one matcher is required. **More specific wins** — `Table`+`Column`
beats `Column`, which beats `Type` — and two rules of equal specificity matching
one column is an error rather than last-one-wins.

### What an override reaches, and what it does not

This is the part worth reading before using it
([ADR-0035](https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#type-overrides)).

| | An override… |
|---|---|
| Models, typed facade, REST bodies, manifest | …changes them |
| The filter grammar's coercion | …changes it, and has to — `?id=eq.019…` parses into the column's Go type |
| The SQL type in DDL | …never reaches `migrate` |
| The wire — JSON, OpenAPI, TypeScript, Dart, CLI | …must not, and cannot: every client emitter maps from the *schema* type |

So a `uuid` overridden to `uuid.UUID` is still `string` in the generated
TypeScript, still `format: uuid` in the OpenAPI document, and still text on a
CLI flag. The API did not change; only the Go did.

`Nullable` and `Array` compose on top, so an overridden nullable column is
`*uuid.UUID` and an overridden array column is `[]uuid.UUID`.

**An enum column cannot be overridden.** The generated string type carries the
value set — its constants, the TypeScript union, the CLI's `--help` — and an
override would replace it with a type that has none of that.

**A wrong `GoType` or `Import` fails in your build, not here.** sqlb emits text
and does not resolve the package, because the package lives in a module sqlb
does not depend on.

## TypeScript client

| Field | Default | Notes |
|---|---|---|
| `TSDir` | *empty* | Relative to `Dir`. **Empty means nothing is emitted** — the right default for a project with no TypeScript consumer |
| `TSClientFile` | `client.gen.ts` | Row types, request bodies, the typed parameter vocabulary, the URL encoder, one function per operation, the cache keys. **Imports nothing** |
| `TSQueriesFile` | `queries.gen.ts` | TanStack Query `queryOptions`, `infiniteQueryOptions` and `mutationOptions`. Takes `@tanstack/react-query` as a peer dependency; set to `"-"` to skip |

Two files because the layers are usable separately. See
[TypeScript SDK](../typescript/README.md).

## Dart client

| Field | Default | Notes |
|---|---|---|
| `DartDir` | *empty* | Relative to `Dir`. **Empty means nothing is emitted** — the right default for a project with no Dart consumer |
| `DartFile` | `client.gen.dart` | Row views, request bodies, the typed parameter vocabulary, the URL encoder, one function per operation, and a cursor pager per list. **Imports nothing** — not `dart:io`, not a pub package, not Flutter |

One file, not two: there is no framework layer to make optional, because Dart
has no equivalent of TanStack Query to bind to. The output is clean under
`package:lints/recommended` and stable under `dart format`, so a consuming
project needs no exclusion for either. See [Dart SDK](../dart/README.md).

## Go CLI

| Field | Default | Notes |
|---|---|---|
| `CLIDir` | *empty* | Relative to `Dir`. **Empty means nothing is emitted** |
| `CLIFile` | `cli_gen.go` | One self-contained file |
| `CLIPackage` | directory name | Package clause for the emitted CLI |
| `CLIName` | `Package` | The binary's name, and — upper-cased — the environment-variable prefix: `taskctl` gives `TASKCTL_BASE_URL` and `TASKCTL_TOKEN` |

The emitted package depends on `github.com/spf13/cobra` and nothing else beyond
the standard library. It does **not** import sqlb or the generated models: it
speaks to the API over HTTP, so the binary holds no database credential and
needs no build tag to keep one out.

A schema that exposes no resource emits no CLI at all, rather than a file that
imports cobra for an empty tree. See [Go CLI](../cli/README.md).

## Checking for staleness

```go
stale, err := codegen.Check(opts)
```

Returns the files that would change, without writing any. Generated code is
committed, so this belongs in CI — it drifts the first time somebody edits a
schema and forgets to regenerate. It covers every artefact the same `Options`
would emit, including the TypeScript client, the Dart client and the CLI.

`codegen.Must` panics on error, which is what a generator `main` wants.

## Rendering a schema from a database

The other direction, for [adopting a database](../migrations/adopting.md):

```go
src, err := codegen.RenderSchema(reg, codegen.SchemaOptions{Package: "blogschema"})
```

Everything imports with **no capabilities and nothing exposed over REST**,
because neither can be read from DDL. Widening that is a deliberate edit, which
is the correct default for a surface that decides what the outside world can
reach.
