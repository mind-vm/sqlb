# Reference

Lookup material, as tables rather than prose. Where a guide page explains a
mechanism, this says exactly what it accepts.

Each page names the file it was checked against. They are hand-written, so they
can fall behind the code — if one disagrees with the source, the source is
right, and the page is a bug. One of them no longer can: `mise run column-check`
fails if the column type page and `schema/field.go` disagree about what
constructors exist, which is what let the guide page stop repeating that table.

| | Checked against |
|---|---|
| [Filter grammar](filter-grammar.md) — every operator, which column types accept it, and the reserved parameters | `filter/filter.go` |
| [Column types](column-types.md) — constructors, SQL types, Go types, and what each admits | `schema/field.go`, `schema/type.go` |
| [Capabilities](capabilities.md) — every method, what it permits, and the struct tag it becomes | `schema/field.go` |
| [Codegen options](codegen.md) — every field of `codegen.Options` and its default | `codegen/codegen.go` |
| [Generated CLI](cli.md) — flags, environment variables, precedence | `codegen/gocli.go` |
| [Rejections](rejections.md) — the problem document, and every message it can carry | `rest/errors.go`, `filter/filter.go` |
| [Glossary](glossary.md) — the words sqlb uses for its own machinery | the pages that argue for each |

## Elsewhere

- **[pkg.go.dev](https://pkg.go.dev/github.com/mind-vm/sqlb)** — the API
  reference, with the compiled `Example` functions attached to the symbols they
  document. Those examples are the canonical form: where a documentation page
  and an example disagree, the example is right.
- **[Compatibility](../compatibility.md)** — which of those symbols are
  frozen, which are provisional, and which will change without ceremony.
- **[Architecture](../architecture.md)** — the package map, the request
  path, and the full table of where sqlb fails loudly.
