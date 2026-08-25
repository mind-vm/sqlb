// Package studio is a generic, uncurated browser over a sqlb-declared
// schema: it reads a sqlb.json manifest for shape and calls the running
// application's generated REST API, with the operator's own bearer token,
// for everything else.
//
// It carries no per-application knowledge — no row label, no field order, no
// widget hints — because a raw grid over declared columns needs none of
// that. That is the whole argument in [the parent module's architecture doc]:
// a *curated* admin (Django's ModelAdmin, picking "title" over "id" for a
// row) has to guess something the manifest deliberately withholds, but an
// uncurated one (Convex's dashboard, a spreadsheet over raw rows addressed
// by primary key) doesn't, so sqlb.json was already enough to build it.
//
//	go build -o sqlb-studio ./studio/cmd/sqlb-studio
//	./sqlb-studio -manifest ./sqlb.json -api http://localhost:8080
//
// Four things it does, matching the manifest's own four quadrants:
//
//   - Schema: every table, column, capability and declared action, browsable
//     without an API — the -api flag is optional for this part alone.
//   - Data: a paginated grid per exposed table, and a row detail page that
//     follows an internal reference to the table it points at.
//   - Writes: create and edit, generated from the same writable-column set
//     the manifest already reports (ReadOnly and Computed excluded), plus —
//     on the create form alone — the properties RESTManifest.CreateInput
//     declares, which are part of the request and not of the row.
//   - Actions: an invoke form per declared action, built from
//     ActionManifest.Body the way ADR-0043 designed it to be read.
//
// What it deliberately does not do: no service credential, no view that sees
// across tenants — every request goes through the REST API as the signed-in
// operator, so the browser shows exactly what that credential could already
// fetch and nothing more. No logs, no traces: that is instrument, don't
// carry — see the Executor-wrapping example in the parent module's
// example_trace_test.go, and point whatever it exports at Uptrace, Jaeger,
// or a Grafana dashboard.
//
// [the parent module's architecture doc]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#the-manifest-describes-what-cannot-be-guessed
package studio
