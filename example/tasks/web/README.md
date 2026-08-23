# web — the generated TypeScript client

The other half of the schema. `taskschema/schema.go` produces the Go models, the
typed column facade and the REST handlers; it also produces this, and the two
cannot disagree because they come from the same declaration in the same run.

```bash
npm install
npm run typecheck   # the test that matters
npm test            # the encoder, against the grammar the server parses
```

## What is generated, and what is not

| | |
|---|---|
| [`src/api/runtime.gen.ts`](src/api/runtime.gen.ts) | **Generated.** The half that depends on no schema: `Page`, `Problem`, `Transport`, the filter encoder and the change-feed stream. Imports nothing, not even the DOM lib. |
| [`src/api/client.gen.ts`](src/api/client.gen.ts) | **Generated.** Row types, request bodies, the typed `where`/`sort`/`select`/`expand` vocabulary, the URL encoder, one function per exposed operation, the cache keys, and `subscribeChanges` — the change-feed subscriber, which resolves an event into the keys that read it. Imports the runtime and re-exports it. |
| [`src/api/queries.gen.ts`](src/api/queries.gen.ts) | **Generated.** TanStack Query `queryOptions` and `infiniteQueryOptions`. The only file that needs `@tanstack/react-query`. |
| [`src/api/http.ts`](src/api/http.ts) | **Hand-written, and meant to be.** The base URL, the bearer token, what a 401 does, and how an error body reaches the UI. None of it is derivable from a schema, so the generated functions take a request function instead of building one. |
| [`src/board.ts`](src/board.ts) | What it looks like in use. |
| [`src/refusals.ts`](src/refusals.ts) | What does *not* compile, asserted. |
| [`src/api/encode.test.ts`](src/api/encode.test.ts) · [`src/api/events.test.ts`](src/api/events.test.ts) | The two things tsc cannot check: the strings that go on the wire, and what a subscriber does with the frames that come back. |

Regenerate with `go generate ./...` from `example/tasks`, or
`sqlb generate ./taskschema`. `mise run generate-check` fails if the committed
output has drifted from the schema, the same gate the Go output is under.

## The point of it

A hand-written client of this API would spell the filter grammar into
`URLSearchParams` by hand and take half a dozen bare `string` parameters. Here,
the request is an object and the compiler knows the schema:

```ts
const page = await listTasks(request, {
  where: {
    status: { in: ['todo', 'in_progress'] },   // the enum's values, not any string
    due_at: { lte: new Date(), notnull: true }, // null tests, because it is nullable
    title: { contains: search },                // pattern operators, because it is text
  },
  sort: ['-priority', 'position'],
  select: ['title', 'status'],
});

page.items[0]?.title;   // fine
page.items[0]?.due_at;  // does not compile: it was not selected
```

Every one of those constraints comes from a capability the schema declared. A
column that never called `.Filterable()` has no spelling in `where`; a hidden
column has no spelling anywhere. That is the same thing the typed column facade
does for Go, carried across the wire — and it is what an OpenAPI document cannot
express, since `?status=eq.todo` documents as `array<string>` with the operators
in prose.

[`src/refusals.ts`](src/refusals.ts) is where that claim is checked. Each case is
a request the server would answer 400 to, marked `@ts-expect-error`; if the
generator ever widened one of those types, `tsc` would report the suppression as
unused and the build would fail. The guard fails when it stops guarding.

## Paging

`next_cursor` is on every list response with a page after it, so walking a whole
result set needs no page arithmetic:

```ts
const feed = taskQueries(request).infinite({ sort: 'due_at', per_page: 100 });
```

`page` and `cursor` are absent from that factory's parameters on purpose: they
are two answers to where a page starts, and the factory owns the answer.

## What this is not

Not an npm package. The client is emitted into the repository that consumes it,
so it cannot be a version behind the server it talks to — the property
`models_gen.go` already has.

Not a client object either. Roughly a quarter of a real application's API is
schema CRUD; `POST /auth/login` is not a table and never will be. So the
generated functions are free functions that compose with hand-written ones
rather than a namespace that has to own everything ([`src/api/http.ts`](src/api/http.ts)
has the login call).

Not hooks. A hook bakes in React and is the thing people copy out and edit; a
`queryOptions` object is spread and overridden:

```ts
{ ...taskQueries(request).list({ sort: 'position' }), staleTime: 30_000 }
```

The reasoning, and the two hand-written clients it was drawn from, are in
[ADR-0028](../../../docs/architecture.md#typescript-client).
