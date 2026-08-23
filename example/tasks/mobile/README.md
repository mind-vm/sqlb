# mobile — the generated Dart client

The third thing the schema produces. `taskschema/schema.go` gives the Go models,
the typed column facade and the REST handlers; it gives `../web` a TypeScript
client; and it gives this. None of the three can disagree, because they come
from the same declaration in the same run.

```bash
dart pub get
dart analyze --fatal-infos            # the test that matters
dart format --output=none --set-exit-if-changed .
dart test                             # the encoder, the row view, the pager
```

Or `mise run test-dart`, which is what CI runs.

## Why this is a Dart package and not a Flutter app

The generated client imports nothing — not `dart:io`, not a pub package, not
Flutter — so nothing here needs a Flutter toolchain, and CI does not pay for
one. Everything below that mentions Dio or Riverpod is documentation of the
seam, not code in this package: [`lib/http.dart`](lib/http.dart) implements the
same seam over `dart:io` so it can be checked with the Dart SDK alone.

## What is generated, and what is not

| | |
|---|---|
| [`lib/api/runtime.gen.dart`](lib/api/runtime.gen.dart) | **Generated.** The half that depends on no schema: `Page`, `Problem`, `Transport`, `CursorPager` and `ChangeFeed`. Imports nothing. |
| [`lib/api/client.gen.dart`](lib/api/client.gen.dart) | **Generated.** Row views, request bodies, the typed `where`/`sort`/`select`/`expand` vocabulary, the URL encoder, one function per exposed operation, a cursor pager per list, and `TableName`/`TableChange` for the change feed. Imports the runtime and exports it. |
| [`lib/http.dart`](lib/http.dart) | **Hand-written, and meant to be.** The base URL, the bearer token, what a 401 does, how an error body reaches the UI, `POST /auth/login` — which is not a table and never will be — and the `GET /events` connection the change feed reads, since Dart has no `EventSource` to own it. |
| [`lib/board.dart`](lib/board.dart) | What it looks like in use. |
| [`lib/refusals.dart`](lib/refusals.dart) | What does *not* compile, asserted. |
| [`test/`](test/) | The three things the analyser cannot check: the strings that go on the wire, the row view's lazy decoding, and what the change feed makes of the frames that come back. |

Regenerate with `go generate ./...` from `example/tasks`, or
`sqlb generate ./taskschema`. `mise run generate-check` fails if the committed
output has drifted from the schema, the same gate the Go and TypeScript output
are under.

## The point of it

A hand-written client of this API spells the filter grammar into a query map by
hand and takes half a dozen bare `String?` parameters, with a comment explaining
in prose which values are legal. Here the request is an object and the analyser
knows the schema:

```dart
final page = await listTasks(
  transport,
  params: TaskListParams(
    where: TaskWhere(
      status: Cond(isIn: [TaskStatus.todo, TaskStatus.inProgress]),
      dueAt: NullableCond(lte: DateTime.now(), notNull: true),
      title: TextCond(contains: search),
    ),
    sort: [TaskSort.priority.desc, TaskSort.position.asc],
    perPage: 50,
  ),
);
```

Every constraint there comes from a capability the schema declared. A column
that never called `.Filterable()` has no property in `TaskWhere`; a column that
never called `.Sortable()` has no member in `TaskSort`; a hidden column has no
spelling anywhere; and `Cond<TaskStatus>` will not take a `String`.

[`lib/refusals.dart`](lib/refusals.dart) is where that claim is checked. Each
case is a request the server would answer 400 to, suppressed with an
`// ignore:` — and the `unnecessary_ignore` lint is on, so an ignore for a
diagnostic that is no longer produced is itself reported. If the generator ever
widened one of those types, the build fails. It is Dart's `@ts-expect-error`,
and it is why the gate runs with `--fatal-infos`.

## The three things Dart does differently

The design is [`../web`](../web)'s, argued in
[ADR-0028](../../../docs/architecture.md#typescript-client). Three things could not
carry over, and [ADR-0031](../../../docs/architecture.md#dart-client) is about those.

**Members are camelCase.** `org_id` is `orgId`, with the wire spelling beside
it. snake_case members would fail the lowerCamelCase lint in every file that
touched them, and Dart has to decode a response key by key regardless — so the
mapping costs a string constant rather than the runtime layer that made
TypeScript keep the wire names.

**A projection is a runtime error, not a compile error.** Dart has no
`Pick<T, K>`, so `select` cannot narrow a return type. A row is therefore a view
over the response: reading a column the request did not return throws
`MissingColumn`, naming the column and the fix.

```dart
final page = await listTasks(
  transport,
  params: const TaskListParams(select: [TaskColumn.title]),
);
page.items.first.title;   // fine
page.items.first.status;  // MissingColumn: Task.status was not in the response.
```

That is worse than a compile error and better than the alternatives — a null
that travels somewhere else first, or no `select` at all on the producer that
most needs to send less over a mobile connection.

**Paging is a pager, not a query-key factory.** The TypeScript client emits
cache keys because TanStack Query has a keyed cache to invalidate; Riverpod and
BLoC have no such registry. What a phone needs instead is the cursor walk, and
that is generated:

```dart
final feed = taskPager(
  transport,
  params: TaskListParams(sort: [TaskSort.dueAt.asc], perPage: 100),
);

await feed.loadMore();   // concurrent calls collapse onto the one in flight
feed.hasMore;            // whether the scroll listener should keep asking
feed.reset();            // pull-to-refresh
```

`page` and `cursor` are ignored in the params it is given: the pager owns where
a page starts.

## In a Flutter app

Two things change and nothing else. The transport becomes Dio, so the
interceptor chain, token refresh and retry stay where an app already keeps them:

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

And the pager goes behind whatever holds state:

```dart
final transportProvider = Provider<Transport>((ref) => ...);

class TaskFeed extends AutoDisposeNotifier<List<Task>> {
  late final CursorPager<Task> _pager;

  @override
  List<Task> build() {
    _pager = taskPager(ref.read(transportProvider));
    return const [];
  }

  Future<void> more() async {
    await _pager.loadMore();
    state = List.of(_pager.items);
  }
}
```

Rows compare by their contents, so a rebuild check on a `List<Task>` behaves the
way a `freezed` model would.

## What this is not

Not a pub package. The client is emitted into the repository that consumes it,
so it cannot be a version behind the server it talks to — the property
`models_gen.go` already has.

Not a client object. Roughly a quarter of a real application's API is schema
CRUD; `POST /auth/login` is not a table. So the generated functions are free
functions that compose with hand-written ones rather than a namespace that has
to own everything — [`lib/http.dart`](lib/http.dart) has the login call.

Not providers. A generated Riverpod provider is a framework baked in and the
thing people copy out and edit, which is the same reason the TypeScript client
emits `queryOptions` rather than hooks.

The reasoning, and the Flutter application it was drawn from, are in
[ADR-0031](../../../docs/architecture.md#dart-client).
