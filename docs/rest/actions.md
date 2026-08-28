# Actions

CRUD covers what a row *is*. It does not cover what happens to it. Completing a
task, archiving a project, publishing a post — these are transitions with rules,
and the way to say one through a PATCH is to send the columns the transition
would have written and hope the server agrees.

A declared action gives the transition a route, and generates everything around
it except the transition itself ([ADR-0043](../architecture.md#declared-actions)):

```go
Task.AddAction(schema.Action{
    Name:   "complete",
    Body:   schema.Body(schema.Text("note").Nullable()),
    Writes: []string{"status", "completed_at"},
})
```

That serves `POST /tasks/{id}/complete`, and asks the application for one func.

## What is generated, and what is not

**Generated:** the route, the OpenAPI operation, the request body type, the id
parse, the scoped fetch, the 404, the transaction, the row lock, the write of
the declared columns, the response — and the same verb in the TypeScript client,
the Dart client and the CLI.

**Not generated:** the transition. That is a plain Go func, the seam
`BeforeCreate` already uses:

```go
func completeTask(ctx context.Context, task *tasks.Task, in tasks.CompleteTaskInput) error {
    if task.Status == tasks.TaskStatusDone {
        return &rest.Problem{
            Title:  http.StatusText(http.StatusConflict),
            Status: http.StatusConflict,
            Detail: "the task is already done",
        }
    }
    now := time.Now().UTC()
    task.Status = tasks.TaskStatusDone
    task.CompletedAt = &now
    return nil
}
```

The split is deliberate and it is the whole design. Domain logic is where a
DSL's expressiveness runs out first, and [vision.md](../vision.md) names the
consequence: generated handlers that get copied out and edited by hand mean the
seams were in the wrong place. So nothing here tries to *express* the
transition. `Writes` says which columns it may leave changed; the rest is Go.

## The verb has to be free

The name reaches every surface as an identifier: `create-tasks` in the OpenAPI
document, `createTask` in the TypeScript and Dart clients, `tasks create` on the
command line. So a verb spelled like an operation the resource already exposes
is not an override of it — it is a second one, with the same name:

```go
Task.Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
    AddAction(schema.Action{Name: "create", Path: "/create"}) // refused
```

Schema validation refuses the pair, naming the operation and the two ways out —
rename the verb, or drop the operation from `Expose` and let the action be the
resource's only `create` route. Left to run, that declaration produces a server
that panics at mount on the duplicate operation id and four clients that do not
compile.

`rest.Action` refuses it a second time, against what is actually mounted rather
than against what was declared, and returns an error where Huma would panic.
The DSL is optional ([ADR-0010](../architecture.md#codegen-is-optional)), so a guard
that lived only in the schema would leave the hand-written mount as the
unguarded one — and the mount-time check sees collisions the declaration cannot,
such as two resources sharing a `Name`.

The verbs that are taken are the ones the exposed operations are generated
under: `create`, `get` (`OpRead`, and `OpSingleton`), `update`, `delete`,
`list`. Only those, and only when the table exposes them — `create` is a fine
verb on a read-only resource, which is exactly the shape `schema.Reads` is for.

## Binding the func

When any table declares an action, `Register` grows a parameter:

```go
if err := tasks.Register(api, db, tasks.Actions{
    CompleteTask: completeTask,
}); err != nil {
    return err
}
```

`Actions` has one field per declared verb, with the exact signature the envelope
will call. That is the compiler's half: adding an action to the schema fails the
next build at this call site rather than serving a route nobody wired. The other
half is at startup — `Actions{}` compiles, so a nil field is refused when the
resource mounts, naming the field to go and set
([ADR-0030](../architecture.md#declared-scope-is-required)'s shape).

## The write set is enforced

The envelope writes exactly the columns `Writes` names, taken off the row the
func mutated. A column the func changed and the declaration did not name stays
unwritten.

That is what makes `Writes` a statement about the route rather than a comment on
it: `sqlb impact` reports it, the OpenAPI document carries it, and `taskctl tasks
complete --help` prints it. A verb that has to touch anything else has the
transaction and can issue the statement:

```go
tx, ok := sqlb.TxFrom(ctx)
if !ok {
    return rest.ErrNoTransaction
}
_, err := sqlb.InsertRows(&tasks.Comment{TaskID: task.ID, Body: *in.Note}).One(ctx, tx)
```

The comment and the completion commit together or neither does.

## `Writes` is not the blast radius, and `Touches` says so

Read those two sections together and the gap is obvious: `Writes` is what the
*envelope* persists — columns, on one row — and the paragraph above hands the
same verb a transaction it can write anything through. The three surfaces that
print `Writes` cannot see the difference, and a caller with no compile step,
which is the caller [ADR-0029](../architecture.md#go-cli) has in mind,
reads `status, completed_at` and concludes the route is confined to one row
([#149](https://github.com/mind-vm/sqlb/issues/149)).

`Touches` is how a wide route says it is wide:

```go
Task.AddAction(schema.Action{
    Name:    "complete",
    Writes:  []string{"status", "completed_at"},
    Touches: []string{"comments"},
})
```

Table names, not columns. It travels with `Writes` everywhere `Writes` goes —
`sqlb impact`, the manifest, the OpenAPI description, the generated `Actions`
doc comment, and `--help`:

```
Beyond that row the route writes: comments.
The schema declares that set; nothing enforces it.
```

**Nothing checks the claim**, and that is the design rather than a gap. Tracing
what a Go func writes is not something the schema package can do, and the
alternative on offer was silence. So the failure mode is an over-broad claim
instead of a confident understatement, and a test asserting the declaration
against the statements the verb actually issued is yours to write — which, from
inside the application, it can be.

A verb that declares nothing gets a sentence saying so, in those words: the
absence of a claim, not a checked bound.

## The lock covers one row

`Writes` also decides the lock. Every one of these is a read-modify-write across
a round trip, so a declared write set makes the fetch `SELECT … FOR UPDATE` —
without it, two concurrent completions read the same row and the second
overwrites the first. Nobody has to remember it per route.

**The row it locks is the one the envelope fetched, and nothing else.**
Statements issued through `sqlb.TxFrom` take their own locks, in an order this
application owns — which is where deadlocks between two wide verbs come from,
and which no declaration can arrange for you. The lock is a guarantee about the
transition on one row, not about the transaction around it.

## Scoping comes with the fetch, authorisation does not

The envelope's fetch runs the model's `BeforeQuery` hooks, so an action on a
`Scoped` model is confined by the same registration its list and read endpoints
are — and refuses to mount without one. Hand-written verb handlers are precisely
where the tenant predicate is otherwise remembered by hand, on the majority of an
application's routes.

A request for another tenant's row gets a **404**, not a 403: the row was never
in the query.

That settles *whose row this is* and nothing more. **An action that declares
`Writes` also obliges a `BeforeUpdate`**, because the envelope persists through
`sqlb.UpdateRows` and "who may write this row" is a different question — one
that only differs from the first when a tenant has more than one kind of member,
which is most products with roles. A family is one tenant; a parent and a child
resolve to it alike, and only one of them may set the PIN.

```
rest: /family/{id}/set-parent-pin exposes the action "set-parent-pin", which
writes "parent_pin", and nothing confines Family
  update: BeforeUpdate is not registered (family_id is Scoped)
```

A verb that declares no `Writes` persists nothing, so the fetch is the whole of
its contact with the row and `BeforeQuery` remains the entire obligation.

The hook is what the library can oblige, not what it can check: it demands a
write rule exists, not that the rule is the right one. Whether *this* caller may
perform *this* transition stays in the func, where preconditions live.

## Collection actions

A path with no `{id}` addresses the collection. There is no row to fetch, so the
func receives only the body, and the response is a 204 unless the verb declares
what it answers with:

```go
Project.AddAction(schema.Action{
    Name: "purge-archived",
    Path: "/purge-archived",
})
```

**Note what is absent along with the fetch.** No `BeforeQuery` runs, so a
declared scope obliges nothing here, and confining the statements the func issues
is the func's own job — the position `sqlb.Query[T]()` in application code is
already in.

## The body

Declared in the field vocabulary, not reflected from an application type:

```go
Body: schema.Body(
    schema.Text("note").Nullable(),
    schema.Timestamp("completed_at"),
),
```

The reason is that the emitters read the declaration. A body sqlb cannot see
produces a TypeScript function typed `unknown`, which is most of what an action
was supposed to remove. Only what describes a *value* applies — name, type,
nullability, enum values, default, comment, and the format rules below. A
property claiming `Filterable` or `Hidden` is a schema validation error rather
than something quietly ignored.

Optionality follows the create body's rule: a nullable or defaulted property may
be omitted, everything else is required.

**Format rules are part of that vocabulary.** `Pattern` for text and `Min`/`Max`
for numbers reach the generated body as the struct tags Huma reads, so the
server rejects a malformed value with a 422 before the func runs, and the rule
appears in the OpenAPI document:

```go
Body: schema.Body(
    schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`),
),
```

Written as a regexp inside the func instead, the same rule reaches no emitter:
the document does not carry it, the generated clients do not know it, and a
caller with no compile step discovers it by sending a bad request. That is the
argument for declaring the body, applied to the rules about its values —
see [the column reference](../reference/column-types.md#format-rules) for
what they do not do, which is write any DDL.

**A map-shaped body declares a map.** `schema.Map(name, valueType)` is the one
request shape the vocabulary could not describe — a set of answers, a settings
patch, a per-key override — and the only spelling for it was `JSON`, which
reaches Go as `json.RawMessage` and every client as `unknown`:

```go
Body: schema.Body(
    schema.Map("answers", schema.TypeUUID),   // question id -> option id
),
```

| | |
|---|---|
| Go | `map[string]string` |
| TypeScript | `Record<string, string>` |
| Dart | `Map<String, String>` |
| CLI | `--answers '{"q1":"o2"}'`, sent as an object |

Keys are always strings. An array is not the substitute: one value per key is a
fact the map carries for free, and a list lets a client send the same key twice,
so the server grows a validation to refuse what the shape used to make
unrepresentable. See [the column reference](../reference/column-types.md#map-which-is-not-a-column)
for why a `jsonb` *column* stays `jsonb`.

These are *format* rules, not domain rules. Whether this PIN is the one already
in use is a question about the world rather than about the string, and it stays
in the func with every other precondition.

An action that declares no body still gets an input type on the Go side, empty,
so that declaring the first property later does not change the signature of the
func you already wrote. The operation reads no request body until there is one.

## What the verb answers with

By default an item action answers with the row it acted on, and a collection
action answers 204. For most verbs that is right: the point of `complete` is
the transition, and the row is what changed.

Some verbs are not like that. Grading a quiz produces a score, marking a batch
read produces a count, issuing an invite produces a token — the *answer* is the
point, and it is not a row of the table. `Returns` declares it:

```go
Lesson.AddAction(schema.Action{
    Name: "submit-quiz",
    Body: schema.Body(schema.JSON("answers")),
    Returns: schema.Result(
        schema.Int("score"),
        schema.Int("total"),
    ),
    Writes: []string{"attempts"},
})
```

The func grows a return value, and the compiler will not let you forget it:

```go
func submitQuiz(ctx context.Context, l *lessons.Lesson, in lessons.SubmitQuizLessonInput) (lessons.SubmitQuizLessonResult, error) {
    score := grade(in.Answers)
    l.Attempts++
    return lessons.SubmitQuizLessonResult{Score: score, Total: 10}, nil
}
```

Everything else is unchanged: the same scoped fetch, the same row lock, the same
write of the declared columns. Only the response differs, and it *replaces* the
default one — the row is persisted and not returned, because one operation has
one response body. A client that needs both re-reads the row whose id it already
sent.

`Result` is `Body` under a second name; the two build the same thing, and the
second name is there so a declaration says which direction it travels in.

**The status is 200 either way**, including for a collection action, which
answers 200 with the result instead of 204. A verb that creates something and
wants to say so with a 201 is describing `OpCreate` — which can take an input
the row has no column for, see
[a body that carries more than the row](README.md#a-body-that-carries-more-than-the-row).

The declaration reaches every surface the body does: the OpenAPI response
schema, a `Promise<SubmitQuizLessonResult>` in TypeScript, a
`Future<SubmitQuizLessonResult>` in Dart, and the CLI's `--help`, which stops
promising the row.

## Errors

| The func returns | The client sees |
|---|---|
| `nil` | 200 with the row (or the declared result), or 204 for a collection action that declares none |
| `*rest.Problem` | that problem's own status — this is how "cannot complete an archived task" is a 409 |
| anything else | 500, with the error logged and the transaction rolled back |

A failing func rolls back everything, including whatever it wrote through the
transaction itself.

## What is deliberately not declarable

Preconditions, guards, and state machines. The moment the schema can say "refuse
if archived", it is expressing the transition, and the failure mode above is
live. Refusing is what the func is for, and a one-line refusal in Go is not the
thing anyone is asking to be freed from.
