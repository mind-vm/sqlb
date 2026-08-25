package recipes

import (
	"encoding/json"
	"time"

	"github.com/mind-vm/sqlb"
)

// The models every recipe in this directory queries. They are hand-written
// here so that one file holds the whole vocabulary, but they are exactly what
// `sqlb generate` emits from a schema declaration: the `db` tag names the
// column and the `sqlb` tag lists the capabilities the schema granted it.
//
// The capabilities are the load-bearing part. A column that does not say
// `filter` cannot be reached by a filter — not by a REST request, not by a
// generated client, not by accident — and the recipes under "the REST filter
// grammar" are mostly about what that refusal looks like from the outside.
// schema_test.go shows the declaration form these tags are written from.

// Author writes posts.
type Author struct {
	ID    string `db:"id" sqlb:"pk,default"`
	OrgID string `db:"org_id" sqlb:"filter"`
	Name  string `db:"name" sqlb:"filter,search,sort"`
	Email string `db:"email" sqlb:"filter"`
	// Hidden is stronger than "not serialised": the column has no spelling in a
	// filter, a sort or a projection either, so it cannot be recovered by
	// probing it one prefix at a time.
	PasswordHash string `db:"password_hash" sqlb:"hidden"`

	// The reverse side of Post.Author. A collection is capped, because the
	// alternative is one request pulling an unbounded number of rows through a
	// join it did not have to ask for.
	Posts *sqlb.Collection[Post] `db:"-" json:"posts,omitempty" sqlb:"expands=author_id,order=-published_at,limit=10"`
}

// TableName maps the model to its table. Without it the name is derived from
// the type, which would also give "authors" here — it is spelled out because a
// derived name is a name nobody chose.
func (Author) TableName() string { return "authors" }

// Post is the model the dynamic list endpoint is built over: filterable by
// several columns, searchable by two, and soft-deleted.
type Post struct {
	ID       string `db:"id" sqlb:"pk,default"`
	OrgID    string `db:"org_id" sqlb:"filter"`
	AuthorID string `db:"author_id" sqlb:"filter,expand"`
	// A relation field is not a column — it holds a row that ?expand=author
	// joined in, and is null otherwise.
	Author *Author `db:"-" json:"author,omitempty" sqlb:"expands=author_id"`

	Title  string `db:"title" sqlb:"filter,search,sort"`
	Body   string `db:"body" sqlb:"search"`
	Status string `db:"status" sqlb:"filter,sort"`
	// ReadOnly: writable by Go code, but a REST request cannot set it. A view
	// counter a client can assign is not a view counter.
	ViewCount int64 `db:"view_count" sqlb:"filter,sort,readonly"`
	// A text[] column. It is a plain Go slice, not a wrapper type.
	Tags []string `db:"tags" sqlb:"filter"`
	// A jsonb column. json.RawMessage rather than []byte, which is how sqlb
	// tells a document from a blob — a bytea column maps to []byte and must not
	// acquire containment operators.
	Metadata json.RawMessage `db:"metadata" sqlb:"filter"`

	// Nullable, because the Go field is a pointer. That is what makes `isnull`
	// and `notnull` available on it.
	PublishedAt *time.Time `db:"published_at" sqlb:"filter,sort"`
	// No capability at all: the soft-delete column is readable by Go code and
	// invisible to every REST request. The hook adds the predicate; see
	// hooks_test.go.
	DeletedAt *time.Time `db:"deleted_at"`
	CreatedAt time.Time  `db:"created_at" sqlb:"default,readonly,sort"`
}

// TableName maps the model to its table.
func (Post) TableName() string { return "posts" }

// Comment hangs off a post. It exists so that the join and transaction recipes
// have a second table to be about.
type Comment struct {
	ID        string    `db:"id" sqlb:"pk,default"`
	PostID    string    `db:"post_id" sqlb:"filter,expand"`
	Post      *Post     `db:"-" json:"post,omitempty" sqlb:"expands=post_id"`
	AuthorID  string    `db:"author_id" sqlb:"filter"`
	Body      string    `db:"body" sqlb:"search"`
	CreatedAt time.Time `db:"created_at" sqlb:"default,readonly,sort"`
}

// TableName maps the model to its table.
func (Comment) TableName() string { return "comments" }
