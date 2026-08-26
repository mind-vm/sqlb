# studio — an uncurated browser over `sqlb.json`

A generic data/schema/action browser: point it at a manifest and a running
application's REST API, and it renders a grid, not a curated admin. **Not a
Django-admin replacement** — see *What it does not do* below before reaching
for it as one. See the package doc (`go doc ./studio`) for the argument and
[docs/adr/0053](../docs/architecture.md#the-manifest-describes-what-cannot-be-guessed)
for the decision this module exists to test.

It is a module of its own, like `pgtest` and `example/tasks`, so its
dependencies stay off the engine's `go.mod` and its release cadence stays off
the engine's — `mise run deps-check` still reports **standard library only**
for the root module.

## Running it

```bash
go build -o sqlb-studio ./studio/cmd/sqlb-studio
./sqlb-studio -manifest ./sqlb.json -api http://localhost:8080
```

Then <http://localhost:4000>. Without `-api`, the schema pages still work —
tables, columns, capabilities, declared actions — but every data page
redirects to a login screen there is no API to serve it against.

Sign in with a bearer token from the application itself (however it issues
one — studio has no opinion). It is attached as `Authorization: Bearer …` on
every request studio makes on your behalf, so you see exactly the rows and
actions that token can already reach — the same row-scoping hooks apply here
as they do to `?expand=` on the API directly.

```bash
go build -o sqlb-studio ./studio/cmd/sqlb-studio && \
./sqlb-studio -manifest example/tasks/sqlb.json -api http://localhost:8080
```

against a running [`example/tasks`](../example/tasks) server is the fastest
way to see it against a real schema.

## Mounting it on your own mux

`studio.Server.Handler()` is a plain `http.Handler`, and `rest.Server.Mux`'s
own doc comment says to mount application routes on it. Pass a third argument
to `NewServer` and mount at the same prefix — no `http.StripPrefix`, because
every href, redirect and asset reference the handlers and templates build
already carries the prefix:

```go
studioSrv, err := studio.NewServer(m, apiBase, "/studio")
if err != nil {
    log.Fatal(err)
}
restSrv.Mux.Handle("/studio/", studioSrv.Handler())
```

The two-argument form, `studio.NewServer(m, apiBase)`, still works exactly as
before — an empty base path, root-mounted, which is what
[`cmd/sqlb-studio`](cmd/sqlb-studio) uses for its own standalone port.

## What to read, and in what order

| | |
|---|---|
| [`doc.go`](doc.go) | The argument: why an uncurated grid needed nothing the manifest was already declining to carry for a curated one. |
| [`manifest.go`](manifest.go) | `LoadManifest` — the only place this module reads a file. Everything else is an HTTP client and a renderer. |
| [`server.go`](server.go) | Routes, and the shape every handler follows: fetch through `client.go`, render through `form.go`'s field builders. |
| [`client.go`](client.go) | The REST client. Decodes into `map[string]any` — there is no generated type to decode into, which is the whole point. |
| [`form.go`](form.go) | Where a column or a declared body property — an action's `Body`, a create's `CreateInput` — becomes an HTML field, and a submitted field becomes a typed JSON value. The narrowest part on purpose: a checkbox for `bool`, a `<select>` for a declared `Enum`, text with a hint for everything else. |
| [`server_test.go`](server_test.go) | An `httptest` fake standing in for a generated REST API — login, the grid, edit, create, and both action shapes, each against real request/response bodies rather than the templates in isolation. |
| [`createinput_test.go`](createinput_test.go) | The same fake for the one shape the columns do not describe: a create body carrying a property that is not a column, which the create form has to offer and the edit form must not. |

## What it does not do

No service credential and no cross-tenant view — every request goes through
the API as the signed-in operator. No logs or traces: that's "instrument,
don't carry," and the seam already exists
([`example_trace_test.go`](../example_trace_test.go) wraps `sqlb.Executor`
for OpenTelemetry, Uptrace, or a Grafana dashboard, none of which this module
needs to know about). No optimistic concurrency — a second operator's
concurrent edit overwrites the first, same as calling `PATCH` by hand would.
Import is not a transaction — each row is its own `POST`, so a failure
partway through an upload leaves the earlier rows created, and the result
page names which rows failed and why rather than rolling anything back. That
is not the same gap as "No bulk actions" below: import is many independent
creates through the same form-building path a single "New row" already
uses, not an operation applied to many *existing* rows at once.

**The gap to Django-admin parity, named rather than left implicit** (a
real-consumer review is what surfaced the need to say this plainly — the
phrase "uncurated browser" undersells it otherwise):

- **No curation layer — `list_display`, `list_filter`, `search_fields`,
  `fieldsets`.** Every column renders the same way on every table, on
  purpose (ADR-0053's whole argument). Building the editorial layer Django's
  `ModelAdmin` is — which column stands for a row, which are worth a list
  view, which sort into a fieldset — is a second design surface with its own
  admission test, not a rendering tweak on this one. Not planned here.
- **No inline or nested editing** — seeding a parent row's children (a
  company's memberships, a course's modules) in one screen. Studio is one
  table per page; a parent-with-children form is exactly the kind of
  hand-written screen an application still owns. Not planned here.
- **No bulk actions.** Studio is per-row plus a declared `Action`; there is
  no select-N-rows-apply-one-action. Not planned here.
- **No history or audit trail, and not because the infrastructure is
  missing — because studio doesn't read it.** The durable change feed
  ([`outbox`](../outbox), [ADR-0012](../docs/architecture.md#change-feed-outbox))
  is built and durable, but its `Event` ([`rest/events.go`](../rest/events.go))
  is an invalidation signal — table, key, create/update/delete — with no
  actor and no field-level diff. Turning that into a Django `LogEntry`
  equivalent needs both a studio page reading the stream *and* the event
  itself carrying who changed what, neither of which exists today. This one
  is the plausible near-term follow-up of the four; the other three are not.
- **No permission-configuration screen.** Studio's own model — every
  request as the signed-in operator, through the same hooks the API
  enforces — has one advantage a real review already confirmed: one source
  of truth for who sees what, rather than Django's parallel permission
  system. But *configuring* a role (who is an admin, what a role can do)
  is application data (a `CompanyMember.role` column, hooks that read it),
  and there is no studio screen for editing it. Correct placement, not a
  gap in the security model — but "replaces Django admin" would otherwise
  quietly assume the application builds this screen itself, and that should
  be said rather than discovered.
