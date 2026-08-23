// Package attachmentschema is the schema definition for the attachments
// example: the single source of truth an author, or an agent, edits.
package attachmentschema

import "github.com/jryannel/sqlb/schema"

// Attachment is a row that points at bytes Postgres never sees.
//
// The bytes go to object storage, direct from the browser to a presigned URL,
// and what is in the database is where they went and whether they arrived.
// That split is the whole subject of this example, and the columns are shaped
// by it:
//
//   - key is the object's name in the bucket, and is minted by the server —
//     never accepted from a caller. A client that could choose the key could
//     choose another tenant's, and a presigned URL is exactly as narrow as the
//     key it was signed for.
//   - status is the arrival, not the intent. A row is born pending, because it
//     is written *before* the upload it authorises, and only a HEAD against
//     the storage may move it to ready.
//   - size is what the storage reported, not what the uploader promised. The
//     promise is checked at the presign step, where it can only be a policy
//     decision; the fact is read afterwards.
//
// All three are read-only, so none of them has a spelling in a generated write
// body. That is what leaves this table with no generated create or update: the
// two writes it has are a presign and a completion, and neither is a row write
// a client could describe. It is example/vault's argument reached from the
// other side — there the payload was Hidden, here it is somewhere else
// entirely.
//
// There is no tenant column, and that is a simplification rather than a
// recommendation: a real attachments table declares one `.Scoped()` and
// prefixes the object key with it, so a listing of one tenant's rows and a
// listing of one tenant's objects agree. example/tasks is where that boundary
// is worked through; keeping it out of here leaves the ordering visible, which
// is what this example is for.
var Attachment = schema.Table("attachments",
	schema.UUIDv7("id").PrimaryKey(),

	// The object's name in the bucket. Read-only because the server mints it,
	// and filterable because the sweeper looks rows up by it — an object found
	// in the bucket has a key and nothing else to be matched on.
	schema.Text("key").ReadOnly().Filterable(),

	// What the person called the file. This one is theirs, and it is the only
	// column here that is: it names nothing and addresses nothing, so a caller
	// may write it.
	schema.Text("filename"),

	// What the storage was told to accept. It is signed into the upload URL,
	// so a PUT sending anything else is refused by the storage rather than
	// recorded here as a lie.
	schema.Text("content_type").ReadOnly().Filterable(),

	// What arrived, in bytes. Zero until the completion step reads it off a
	// HEAD; never the number the uploader claimed.
	schema.BigInt("size").ReadOnly().Filterable().Sortable(),

	// Where in the two-step this row is. A pending row older than the URL it
	// authorised is garbage, and the sweeper reaps it.
	schema.Enum("status", "pending", "ready").
		Default(schema.Value("pending")).ReadOnly().Filterable(),

	schema.Timestamps(),
).
	// The sweeper's two questions: which rows never completed, and does this
	// object have a row at all.
	Index("status", "created_at").
	UniqueIndex("key").
	Describe("A file in object storage, and the row that says it is there.").
	Expose(schema.REST{
		Path: "/attachments",
		// No OpCreate and no OpUpdate. Creating an attachment is minting a key
		// and signing a URL for it; completing one is reading the storage back.
		// Neither is a row a client could send, and mounting a generated body
		// that could set key, size or status would be mounting the hole this
		// example exists to close.
		//
		// OpDelete is generated, and is the interesting one: deleting the row
		// has to delete the object too, and that happens in a hook — after the
		// commit, because object storage is not in the transaction.
		Ops: schema.OpRead | schema.OpList | schema.OpDelete,
	})
