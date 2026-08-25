package recipes_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// A jsonb column is json.RawMessage, not []byte. That distinction is load
// bearing: a bytea column is also a slice of bytes, and sqlb decides which
// operators a column offers by the Go type, so getting it backwards would
// offer document containment over a blob.
func Example_jsonScansAsRawMessage() {
	post, err := sqlb.Query[recipes.Post]().First(context.Background(), recordingDB())
	if err != nil {
		panic(err)
	}

	var meta struct {
		Lang string `json:"lang"`
	}
	if err := json.Unmarshal(post.Metadata, &meta); err != nil {
		panic(err)
	}
	fmt.Println(string(post.Metadata), "->", meta.Lang)
	// Output:
	// {"lang":"en"} -> en
}

// ContainsJSON is Postgres's `@>`: every key and value in the document must
// appear in the column. It is the operator a GIN index over the column serves,
// and it is why a document column can be narrowed without the schema declaring
// in advance which keys it holds.
//
// The argument is JSON text rather than a Go value because a predicate has no
// error to return and marshalling has one. A caller holding a value marshals it
// first, and handles the failure where it happens.
func Example_jsonContainment() {
	filter, err := json.Marshal(map[string]any{"lang": "de"})
	if err != nil {
		panic(err)
	}

	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("metadata").ContainsJSON(string(filter))))
	// Output:
	// WHERE "metadata" @> $1::jsonb
	// args: [{"lang":"de"}]
}

// Writing a document is the value going the other way, and the column takes it
// as a bind parameter like any other.
func Example_jsonWritten() {
	doc, err := json.Marshal(map[string]any{"lang": "de", "reviewed": true})
	if err != nil {
		panic(err)
	}

	show(sqlb.UpdateRows[recipes.Post]().
		Set("metadata", json.RawMessage(doc)).
		Where(sqlb.F("id").Eq("p1")))
	// Output:
	// UPDATE "posts" SET "metadata" = $1 WHERE "id" = $2 RETURNING "id", "org_id", "author_id", "title", "body", "status", "view_count", "tags", "metadata", "published_at", "deleted_at", "created_at"
	// args: [{"lang":"de","reviewed":true} p1]
}
