# Dart client

A Flutter app is the producer with the least type safety over this API and the
most to gain from some. It assembles filters from what a person taps, it pages a
list as that person scrolls it, and it does both over a connection that is
sometimes a train tunnel.

`codegen` emits a Dart client for all three, from the same schema declaration the
Go models come from. It is [the TypeScript client](../typescript/README.md) in a
second language, with the three differences the language forces —
[ADR-0031](../architecture.md#dart-client) is where those are argued.

## Turning it on

Set `DartDir` on the generator you already have:

```go
codegen.Must(codegen.Generate(codegen.Options{
    Registry: schema.DefaultRegistry(),
    Dir:      "blog",
    Package:  "blog",

    // Relative to Dir. Two files land here; nothing is emitted without it.
    DartDir: "mobile/lib/api",
}))
```

Two files, and neither **imports anything** — not `dart:io`, not a pub package,
not Flutter. There is no framework layer to make optional, because Dart has no
equivalent of TanStack Query to bind to.

| | |
|---|---|
| `runtime.gen.dart` | `Page`, `Collection`, `Problem`, `Transport`, `CursorPager`, `ChangeFeed` — the vocabulary an application names, and no schema-specific code. |
| `client.gen.dart` | Row views, request bodies, the typed filter vocabulary, one function per exposed operation, `TableName` and `TableChange` — plus a second row view per table that has a `Needs` column, for what `create`/`update` return, since a write cannot answer with the same shape a read can. Imports the runtime, and exports it. |

The split is what Dart's nominal typing forces
([#110](https://github.com/jryannel/sqlb/issues/110)). Two clients each
declaring their own `Page` declare two *unrelated* classes, so no shared pager
widget could accept both and the application could not give both one
`Transport`. Two clients exporting one library offer one `Page`.

`Row` and the `Cond` family stay with each client, because both keep a private
contract with the generated code — the `_str`/`_int` protocol every row view
inherits, and `Cond._encode` — and Dart privacy is per library. Duplicating
them costs nothing observable, because nothing can observe them.

Importing the client alone is still enough: `client.gen.dart` exports the
runtime, so `Page`, `Problem` and `Transport` arrive with it.

The client is emitted into the repository that consumes it, the way
`models_gen.go` is. There is no pub package to install and therefore no way for
the client to be a version behind the server it talks to. `codegen.Check` covers
both, so the usual staleness gate catches a schema change that was never
regenerated.

## What the types know

Everything a column declared, and nothing it did not:

```dart
final page = await listPosts(
  transport,
  params: TaskListParams(
    where: PostWhere(
      status: Cond(isIn: [PostStatus.draft, PostStatus.published]),
      title: TextCond(contains: search),
      publishedAt: NullableCond(notNull: true),
      viewCount: Cond(gte: 100),
      labels: ArrayCond(has: 'urgent'),
    ),
    sort: [PostSort.publishedAt.desc, PostSort.title.asc],
    select: [PostColumn.title, PostColumn.status],
    expand: [PostExpand.author],
    perPage: 50,
  ),
);
```

- **`where` admits filterable columns only**, and the condition type is narrowed
  by the column. `TextCond` exists only on text, so `contains` on a number does
  not compile; `NullableCond` only on a nullable column, so neither does
  `isNull` on one that is required; and the value type is the column's own, so
  an enum compares against its own members and not against any string.
- **An array column takes `ArrayCond`** — `has` for one element, `hasAny` and
  `hasAll` for a list, `notHas`/`notHasAny`/`notHasAll` for their negations, and
  `eq` for the whole array. It carries no `contains`, which
  belongs to text, and none of the ordering operators; the element type is the
  column's own, so `has` on an enum array compares against its members. Reading
  one back gives a `List<String>`, and a nullable one distinguishes null from
  the empty list.
- **`sort` names sortable columns**, and `.asc` / `.desc` are the two terms each
  one offers. An array column is never among them.
- **`select` and `expand` are closed sets**, one enum per resource.
- **Hidden columns have no spelling anywhere.** Not on the row, not in `select`,
  not in `where`.

This is [the typed column facade](../queries/typed-columns.md) carried across
the wire, and it is why the client is generated from the schema rather than from
the OpenAPI document: the document can only say `array<string>` about a filter
parameter, with the operators in prose.

## A row is a view over the response

```dart
class Post extends Row { ... }
```

Columns are decoded on access — `post.publishedAt` parses the timestamp, and a
list of two hundred rows whose screen reads three columns decodes three columns.
Members are lowerCamelCase with the wire spelling beside them, because
snake_case would fail the lowerCamelCase lint in every file that touched it.

That shape is what makes `select` usable at all. Dart cannot narrow a type by a
runtime projection, so a column the request did not return is reported where it
is read:

```dart
final page = await listPosts(
  transport,
  params: const PostListParams(select: [PostColumn.title]),
);
page.items.first.title;   // fine
page.items.first.status;  // throws MissingColumn: Post.status was not in the
                          // response. Add it to select, or drop select to get
                          // every column.
```

`row.has(PostColumn.status)` is there for code that means to branch on it, and
`row.toJson()` gives back exactly what arrived, which is what a local cache
wants to store.

A column carrying `Needs(...)` never arrives on a create or update response —
its expression depends on who is asking, and a write has no per-request bind to
resolve that with, so the key is absent
([ADR-0041](../architecture.md#computed-fields)). Lazy decoding means a getter for
it would still compile against the plain row class and only fail at
`MissingColumn` the first time something called it, which is the same
promise-and-fail-later shape as an unselected column, only with no `select` to
blame. So `createPost` and `updatePost` return `PostWriteResult` instead of
`Post` whenever the table has one of these: a second, narrower row class with
no getter for the columns a write cannot answer, generated alongside `Post`
rather than reachable only by remembering not to call one.

An expansion is nullable rather than absent, in both directions:

```dart
post.author;          // Author?      — filled in by expand: [PostExpand.author]
author.posts;         // Collection<Post>?  — {items, hasMore}, capped
```

The reverse direction keeps its envelope rather than becoming a bare list, so a
screen showing twenty of two hundred can say so.

## The transport is yours

The generated functions take a request function rather than constructing one.
Base URL, auth header, refresh, retry, offline behaviour and what a 401 does are
not derivable from a schema, and are the parts of a real client that matter
most. Over Dio:

```dart
final Transport transport = (request) async {
  final response = await dio.request<Object?>(
    request.query == null || request.query!.isEmpty
        ? request.path
        : '${request.path}?${request.query}',
    options: Options(method: request.method),
    data: request.body,
    cancelToken: request.cancel as CancelToken?,
  );
  return response.data;
};
```

`ApiRequest.cancel` is `Object?` rather than a `CancelToken` so that the
generated file keeps its promise to import nothing; it is passed through
untouched.

This is the same seam `rest` takes by mounting onto a `huma.API` you built. It
also means the generated functions compose with hand-written ones: a login
endpoint is not a table, and no schema generator will produce it.

## Paging is a pager

`next_cursor` is on every list response with a page after it, which is the walk
an infinite-scrolling list needs:

```dart
final feed = postPager(
  transport,
  params: PostListParams(sort: [PostSort.publishedAt.desc], perPage: 50),
);

await feed.loadMore();
feed.items;      // what has arrived
feed.hasMore;    // whether to keep going
feed.isLoading;  // for the spinner at the bottom
```

Concurrent `loadMore()` calls collapse onto the one already running, because a
scroll listener fires on every frame near the end of a list. `reset()` is
pull-to-refresh. The pager holds rows and a position and nothing about how they
are shown, so it drops into a Riverpod notifier, a BLoC or a `StatefulWidget`
without preferring any of them:

```dart
class PostFeed extends AutoDisposeNotifier<List<Post>> {
  late final CursorPager<Post> _pager;

  @override
  List<Post> build() {
    _pager = postPager(ref.read(transportProvider));
    return const [];
  }

  Future<void> more() async {
    await _pager.loadMore();
    state = List.of(_pager.items);
  }
}
```

## Rejections keep their allow-list

A 400 from the filter grammar carries what would have been accepted. The client
types that body rather than flattening it to a message, so a filter UI can offer
the alternatives instead of a dead end:

```dart
final problem = Problem.tryParse(errorBody);
final sortable = problem?.allowedFor('query.sort') ?? const [];
```

## Writes

A create body carries what a request may write, which is not the row: read-only
columns are absent, and a column with a default is optional.

```dart
await createPost(transport, const PostCreate(orgId: 'o1', title: 'Hello'));
```

A patch is a builder, one method per writable column, because *omitted* and
*explicitly null* are different requests and no field can carry that
distinction:

```dart
final patch = PostPatch()
  ..title('Hello again')
  ..publishedAt(null);   // writes NULL; a column not named is not written

await updatePost(transport, id, patch);
```

An immutable column has no method here at all — it is settable once, at create.

## Names come from table names

A table's types are its own singularised name — `posts` → `Post` — and that
name plus a suffix: `PostColumn`, `PostSort`, `PostWhere`, `PostListParams`,
`PostCreate`, `PostPatch`. Two tables can therefore want the same name:
`board_columns` singularises to `BoardColumn`, which is also what `boards`
calls its column enum.

`sqlb generate` refuses that rather than writing a file the compiler rejects,
naming the identifier, both tables, and what each contributed
([#261](https://github.com/jryannel/sqlb/issues/261)). The fix is to rename one
of the tables — the generated names follow. Before this check the generator
reported success and the analyser reported a duplicate top-level declaration,
naming neither table.

## The change feed

[`rest.Events`](../rest/events.md) sends the *address* of a change — a table, a
row key, an operation — and never the row. `ChangeFeed` reads that stream:

```dart
final feed = ChangeFeed();
await for (final event in feed.read(body, parseJson: jsonDecode)) {
  switch (event) {
    case ChangeEvent():
      final change = TableChange.from(event);
      if (change != null) refetch(change.table, change.key);
    case ResetEvent():
      refetchEverything();
  }
}
```

`FeedEvent` is sealed, so that `switch` is exhaustive: a subscriber that
handles a change and forgets a reset does not compile, and the reset is the
case that matters — it means the stream could not be resumed, so nothing on
display can be trusted. `TableChange.from` narrows the event's table to a
`TableName` member, and answers null for a table this client does not serve,
which is what a module of a larger schema receives.

Three things are deliberately the application's:

- **The connection.** Dart has no `EventSource`, and this library imports
  nothing, so `read` takes the response body as text and the app opens the
  request. That turns out to be the easier half — a `Last-Event-ID` header is
  just a header here, where a browser subscriber needs a polyfill to send one.
  `example/tasks/mobile/lib/http.dart` has the `dart:io` version.
- **The JSON decoder.** `parseJson: jsonDecode` — passed rather than imported,
  for the reason `Transport` is passed rather than built.
- **Reconnecting.** When the stream ends, open it again with the same
  `ChangeFeed`: its `lastEventId` is the position to send, and the server
  replays from there or answers with a `ResetEvent` when it cannot reach back
  that far.

A frame that cannot be decoded ends the stream rather than being skipped. That
is the rule the whole feed is built on — a dropped connection reconnects and
converges, a dropped event leaves a client wrong forever — and `lastEventId`
has already moved past the frame, so the reconnection does not meet it again.

## What is not generated

Riverpod providers, BLoCs, widgets, a client object, a pub package. A generated
provider is a framework baked in and the thing people copy out and edit, which
is the same reason [ADR-0028](../architecture.md#typescript-client) refuses to
generate React hooks.

There is also no cache-key factory, which the TypeScript client does emit. That
one exists because TanStack Query has a keyed cache to invalidate; Dart's state
managers have no such registry, so keys would be vocabulary with no consumer —
which is why a subscriber here gets a narrowed `TableChange` and decides what
to refetch, where the TypeScript one is handed the keys.

`example/tasks/mobile` is a worked one, including
[the refusals](../../example/tasks/mobile/lib/refusals.dart) — sixteen requests
that must *not* compile, each suppressed with an `// ignore:` that the
`unnecessary_ignore` lint reports if it ever stops being needed. A generator
that widened one of those types fails the build.

## Next

- [TypeScript client](../typescript/README.md) — the same design where the
  language allows narrowing
- [Mounting resources](../rest/README.md) — the server side of the same grammar
- [Capabilities](../schema/capabilities.md) — the declarations these types come
  from
