// Ejected from a sqlb schema by `sqlb eject`. This file is yours now:
// edit it, delete parts of it, or keep regenerating it — `sqlb eject -check`
// reports drift for as long as you want it to and is meant to be dropped
// from CI on the day you stop.
//
// The row structs, with the sqlb tags removed.

// Package ejected is the exit `sqlb eject` wrote: pgx and the standard
// library, and nothing else.
package ejected

import "time"

// The rows. These are the structs the generated models were, with the sqlb tags
// removed: nothing reads them any more. Relations are gone with them — ?expand
// was one statement built by the engine, and the exit does not carry it.

// Author is a row of authors.
type Author struct {
	ID           string    `db:"id" json:"id"`
	OrgID        string    `db:"org_id" json:"org_id"`
	Email        string    `db:"email" json:"email"`
	Name         string    `db:"name" json:"name"`
	PasswordHash string    `db:"password_hash" json:"-"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// Org a tenant. Every other table is scoped to one.
type Org struct {
	ID        string    `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Slug      string    `db:"slug" json:"slug"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// Post a blog post.
type Post struct {
	ID          string     `db:"id" json:"id"`
	OrgID       string     `db:"org_id" json:"org_id"`
	AuthorID    string     `db:"author_id" json:"author_id"`
	Title       string     `db:"title" json:"title"`
	Body        string     `db:"body" json:"body"`
	Status      string     `db:"status" json:"status"`
	ViewCount   int64      `db:"view_count" json:"view_count"`
	PublishedAt *time.Time `db:"published_at" json:"published_at"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt   *time.Time `db:"deleted_at" json:"deleted_at"`
}
