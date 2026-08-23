# attachments — the file that never passes through the server

A browser asks for permission, PUTs the file straight to object storage with
the URL it gets back, and tells the server it is done. Postgres holds the row
that says where the bytes went and whether they arrived.

This is the shape people reach for `FileField` for, and the reason sqlb has no
such column type: the database part is three ordinary columns, and everything
that is actually hard is about **the order the two writes happen in and what is
left behind when one of them fails**. That is what this example is about. The
signing is a means to it.

Run it:

```bash
cd example/attachments
go test ./...          # no Docker, no Postgres, no bucket
```

Against real storage — RustFS, MinIO, or anything else S3-compatible:

```bash
docker run -p 9000:9000 \
  -e RUSTFS_ACCESS_KEY=rustfsadmin -e RUSTFS_SECRET_KEY=rustfsadmin \
  rustfs/rustfs
# create the bucket once, with mc / the console / any S3 client

SQLB_TEST_S3_ENDPOINT=http://localhost:9000 \
SQLB_TEST_S3_BUCKET=uploads \
SQLB_TEST_S3_ACCESS_KEY=rustfsadmin \
SQLB_TEST_S3_SECRET_KEY=rustfsadmin \
  go test ./... -run Live -v
```

## The flow

```
   client                      server                     storage
     │  POST /attachments        │                           │
     │──────────────────────────>│  INSERT … status=pending  │
     │                           │──────────────────────────>│ (database)
     │<──────────────────────────│  presigned PUT URL        │
     │                                                       │
     │  PUT <url>  (the bytes, and the server never sees them)│
     │──────────────────────────────────────────────────────>│
     │                           │                           │
     │  POST /attachments/{id}/complete                      │
     │──────────────────────────>│  HEAD <key> ────────────> │
     │                           │  UPDATE … status=ready    │
     │<──────────────────────────│  size from the storage    │
```

Three things in that picture are decisions rather than mechanics.

**The row is written first, and it is born `pending`.** The alternative — bytes
first, row after — leaves an object nothing references if the second write
fails, and an object with no row is findable only by listing the whole bucket.
A row with no object is findable with a `WHERE`.

**The size comes from the storage, not from the client.** A presigned PUT
carries no length the storage will enforce, so the number in the request body
is a *policy input* — it lets the server refuse an upload before it starts —
and the number in the row comes from a `HEAD` afterwards. A client that lied
still uploads; the completion step is where the lie is caught, and the object
is deleted rather than recorded.

**The content type is signed into the URL.** It is in `SignedHeaders`, so a PUT
sending anything else fails the signature check at the storage. That is the
difference between recording what the uploader claimed and constraining what it
may send.

## Deleting, which is the same argument backwards

`DELETE /attachments/{id}` is the generated handler — nothing here wraps it.
The object goes with the row through a hook:

```go
sqlb.On[Attachment](reg).AfterDeleteRows(func(ctx context.Context, rows []Attachment) error {
    keys := keysOf(rows)
    return sqlb.AfterCommit(ctx, func(ctx context.Context) error {
        return removeObjects(ctx, keys)
    })
})
```

A hook rather than an endpoint wrapper, so a cascade, a bulk cleanup in Go and
the admin script at two in the morning all clean up too — the same argument
that puts tenant scoping in `BeforeQuery` rather than in each handler.
`AfterDeleteRows` rather than `AfterDelete`, because the count hook knows how
many rows went and this needs to know *which*: the key is on the row. That
costs the delete a `RETURNING`, which is the price of being able to clean up
after it.

And `AfterCommit` rather than inline, because object storage is not in the
transaction. Delete the object inside one and roll back, and the row is still
there pointing at bytes that are gone — a broken image, forever, with nothing
left to find it by. Fail the other way and an object outlives its row, which
costs storage and is reapable. **A leak that can be found beats a lie that
cannot.**

| What fails | What is left | How it is found |
|---|---|---|
| The upload never happens | A `pending` row | `WHERE status = 'pending' AND created_at < …` |
| The completion call is lost | A `pending` row with an object | the same sweep, and the delete takes the object with it |
| The object delete fails after the commit | An object with no row | list the bucket, ask the table about the keys |
| The transaction rolls back | Nothing | — |

`Store.Sweep` is both directions of that table, and it is a job on a timer
rather than something a request calls.

### One trap, and it reports itself

`sqlb.AfterCommit` **fails** outside a transaction rather than running the
callback immediately:

```
sqlb: AfterCommit found no transaction in the context; the write it should
follow must be inside db.WithTx
```

Which is the right error in the right place: under autocommit there is no
"after the commit" for sqlb to mean. The generated REST delete wraps itself in
a transaction, so `DELETE /attachments/{id}` just works. A hand-written delete
does not, which is why `Store.Sweep` opens one — and why `NewStore` takes a
`*sqlb.DB` rather than an `sqlb.Executor`.

## What the schema declares, and what it therefore refuses

`key`, `content_type`, `size` and `status` are all `ReadOnly`, so none of them
has a spelling in a generated write body. That leaves the table with no
generated create and no generated update at all:

```go
Expose(schema.REST{
    Path: "/attachments",
    Ops:  schema.OpRead | schema.OpList | schema.OpDelete,
})
```

Which is the point rather than a gap. The two writes this table has are a
presign and a completion, and neither is a row a client could describe: one
mints a key and signs a URL, the other reads the storage back. Mounting a
generated body that could set `key`, `size` or `status` would mount the hole
the whole design closes — a client that can choose its own key can choose
somebody else's.

It is [`example/vault`](../vault)'s argument reached from the other side: there
the payload is `Hidden` because only Go may write it, here the payload is not
in the database at all.

## The presigner

`s3/` is Signature Version 4, query-string form, against the standard library —
about 250 lines, no dependency. Taking the AWS SDK for one operation would put
a hundred-odd packages behind an example about ordering, and this module's
`go.mod` requires exactly one thing: sqlb.

Everything is a presigned URL, including the server's own `HEAD`, `DELETE` and
`LIST`, with a lifetime measured in seconds. One signing routine rather than
two, and nothing has to build an `Authorization` header.

It is **cross-checked against `aws-sdk-go-v2`**: for the same endpoint, bucket,
key, credentials, region, expiry and instant, `PresignHead` reproduces
`s3.NewPresignClient`'s `PresignHeadObject` byte for byte, and that signature is
pinned in `presign_test.go`. (The SDK adds an `x-id=GetObject` marker of its own
to GET, PUT and DELETE, which the storage ignores and the signature covers, so
those differ by that parameter; adding it reproduces them too.)

Path-style addressing — `http://host:9000/bucket/key` — because that is what a
container on localhost serves.

## What each test file answers

| | |
|---|---|
| `s3/presign_test.go` | Is the signature right? Against aws-sdk-go-v2, plus the encoding rules the standard library's escapers each get differently. |
| `store_test.go` | Is the *order* right? `sqlbtest.DB` records the statements and an httptest bucket holds the objects, so "the object went after the commit" is an assertion. No database, no network. |
| `live_test.go` | Does real storage accept it? Skipped unless `SQLB_TEST_S3_ENDPOINT` is set. |

The middle one is where the design lives, and it is worth reading for how much
can be pinned without a database: that a refused upload writes no row, that a
rolled-back delete leaves the object alone, that a completion against storage
that is *down* is not read as an object that is *missing*.

## What this deliberately does not do

- **Tenancy.** A real attachments table declares its tenant column `.Scoped()`
  and prefixes the object key with it, so a listing of one tenant's rows and a
  listing of one tenant's objects agree. [`example/tasks`](../tasks) is where
  that boundary is worked through; leaving it out here keeps the ordering
  visible.
- **Enforcing a size limit at upload time.** S3 can bind a length through a
  POST policy, which is a different upload shape with a different browser API.
  This uses a PUT and checks afterwards.
- **Multipart, resumable uploads, virus scanning, image processing.** Each is a
  queue and a worker.
- **Creating the bucket**, or managing credentials. Both are deployment.
