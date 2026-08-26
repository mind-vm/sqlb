// Ejected from a sqlb schema by `sqlb eject`. This file is yours now:
// edit it, delete parts of it, or keep regenerating it — `sqlb eject -check`
// reports drift for as long as you want it to and is meant to be dropped
// from CI on the day you stop.
//
// One function per statement. The SQL is written out.

package ejected

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The statements. Each one is written out: the SQL is a string you can read,
// paste into psql, and change. What varies per request — the WHERE, the ORDER
// BY and the paging — is assembled by the helpers in support.go, which is the
// smallest amount of assembly a filterable list endpoint can be written with.

// authorTable is the table Author maps to.
const authorTable = "authors"

// authorSelect is the projection: every column, in declaration order.
const authorSelect = "\"id\", \"org_id\", \"email\", \"name\", \"password_hash\", \"created_at\", \"updated_at\""

// authorColumns is what a request may name, and for what.
var authorColumns = []Column{
	{Name: "id", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "org_id", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "email", Filterable: true, Sortable: false, Searchable: true, Parse: ParseText},
	{Name: "name", Filterable: true, Sortable: true, Searchable: true, Parse: ParseText},
	{Name: "created_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
	{Name: "updated_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
}

// scanAuthor reads one row of authorSelect.
func scanAuthor(s scanner) (Author, error) {
	var row Author
	err := s.Scan(
		&row.ID,
		&row.OrgID,
		&row.Email,
		&row.Name,
		&row.PasswordHash,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	return row, err
}

// ListAuthor reads a page. It returns one row more than asked for when there is
// one, which is how the handler answers has_more without a second count.
func ListAuthor(ctx context.Context, db DB, q Query) ([]Author, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", authorSelect, quoteIdent(authorTable))
	writeWhere(&sb, args, q.Where)
	writeOrder(&sb, q.Order)
	writeLimit(&sb, q.Limit, q.Offset)

	rows, err := db.Query(ctx, sb.String(), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Author
	for rows.Next() {
		row, err := scanAuthor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountAuthor is ?count=exact: the size of the matching set, which costs a second
// query over the same predicate.
func CountAuthor(ctx context.Context, db DB, where []Condition) (int64, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT count(*) FROM %s", quoteIdent(authorTable))
	writeWhere(&sb, args, where)

	var n int64
	err := db.QueryRow(ctx, sb.String(), args.values...).Scan(&n)
	return n, err
}

// GetAuthor reads one row by primary key. The extra conditions are whatever
// confines this table — a tenant, a soft delete — and they are part of the
// lookup rather than a check afterwards, so a row outside them is a 404 and not
// a 403 that confirms it exists.
func GetAuthor(ctx context.Context, db DB, id any, where []Condition) (Author, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", authorSelect, quoteIdent(authorTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	row, err := scanAuthor(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, err
	}
	return row, nil
}

// InsertAuthor writes one row and reads it back, so database defaults and computed
// columns arrive without a second query.
//
// The column order is the schema's, not the map's: a generated statement whose
// text depends on map iteration is a statement that cannot be diffed.
func InsertAuthor(ctx context.Context, db DB, values map[string]any) (Author, error) {
	order := []string{"org_id", "email", "name", "password_hash"}

	var cols, holes []string
	args := new(args)
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		cols = append(cols, quoteIdent(name))
		holes = append(holes, args.add(v))
	}

	var sb strings.Builder
	if len(cols) == 0 {
		fmt.Fprintf(&sb, "INSERT INTO %s DEFAULT VALUES", quoteIdent(authorTable))
	} else {
		fmt.Fprintf(&sb, "INSERT INTO %s (%s) VALUES (%s)",
			quoteIdent(authorTable), strings.Join(cols, ", "), strings.Join(holes, ", "))
	}
	fmt.Fprintf(&sb, " RETURNING %s", authorSelect)

	return scanAuthor(db.QueryRow(ctx, sb.String(), args.values...))
}

// UpdateAuthor writes the named columns of one row and reads the row back. An
// empty change set is the caller's mistake rather than a statement with no SET
// clause, which Postgres will not parse.
func UpdateAuthor(ctx context.Context, db DB, id any, changes map[string]any, where []Condition) (Author, error) {
	order := []string{"org_id", "email", "name", "password_hash"}

	var sets []string
	args := new(args)
	for _, name := range order {
		v, ok := changes[name]
		if !ok {
			continue
		}
		sets = append(sets, quoteIdent(name)+" = "+args.add(v))
	}
	var zero Author
	if len(sets) == 0 {
		return zero, ErrNoChanges
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "UPDATE %s SET %s", quoteIdent(authorTable), strings.Join(sets, ", "))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))
	fmt.Fprintf(&sb, " RETURNING %s", authorSelect)

	row, err := scanAuthor(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrNotFound
		}
		return zero, err
	}
	return row, nil
}

// DeleteAuthor removes one row, and reports ErrNotFound rather than success when
// the id matched nothing the conditions admit.
func DeleteAuthor(ctx context.Context, db DB, id any, where []Condition) error {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "DELETE FROM %s", quoteIdent(authorTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	tag, err := db.Exec(ctx, sb.String(), args.values...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// orgTable is the table Org maps to.
const orgTable = "orgs"

// orgSelect is the projection: every column, in declaration order.
const orgSelect = "\"id\", \"name\", \"slug\", \"created_at\", \"updated_at\""

// orgColumns is what a request may name, and for what.
var orgColumns = []Column{
	{Name: "id", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "name", Filterable: true, Sortable: true, Searchable: true, Parse: ParseText},
	{Name: "slug", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "created_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
	{Name: "updated_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
}

// scanOrg reads one row of orgSelect.
func scanOrg(s scanner) (Org, error) {
	var row Org
	err := s.Scan(
		&row.ID,
		&row.Name,
		&row.Slug,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	return row, err
}

// ListOrg reads a page. It returns one row more than asked for when there is
// one, which is how the handler answers has_more without a second count.
func ListOrg(ctx context.Context, db DB, q Query) ([]Org, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", orgSelect, quoteIdent(orgTable))
	writeWhere(&sb, args, q.Where)
	writeOrder(&sb, q.Order)
	writeLimit(&sb, q.Limit, q.Offset)

	rows, err := db.Query(ctx, sb.String(), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Org
	for rows.Next() {
		row, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountOrg is ?count=exact: the size of the matching set, which costs a second
// query over the same predicate.
func CountOrg(ctx context.Context, db DB, where []Condition) (int64, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT count(*) FROM %s", quoteIdent(orgTable))
	writeWhere(&sb, args, where)

	var n int64
	err := db.QueryRow(ctx, sb.String(), args.values...).Scan(&n)
	return n, err
}

// GetOrg reads one row by primary key. The extra conditions are whatever
// confines this table — a tenant, a soft delete — and they are part of the
// lookup rather than a check afterwards, so a row outside them is a 404 and not
// a 403 that confirms it exists.
func GetOrg(ctx context.Context, db DB, id any, where []Condition) (Org, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", orgSelect, quoteIdent(orgTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	row, err := scanOrg(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, err
	}
	return row, nil
}

// InsertOrg writes one row and reads it back, so database defaults and computed
// columns arrive without a second query.
//
// The column order is the schema's, not the map's: a generated statement whose
// text depends on map iteration is a statement that cannot be diffed.
func InsertOrg(ctx context.Context, db DB, values map[string]any) (Org, error) {
	order := []string{"name", "slug"}

	var cols, holes []string
	args := new(args)
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		cols = append(cols, quoteIdent(name))
		holes = append(holes, args.add(v))
	}

	var sb strings.Builder
	if len(cols) == 0 {
		fmt.Fprintf(&sb, "INSERT INTO %s DEFAULT VALUES", quoteIdent(orgTable))
	} else {
		fmt.Fprintf(&sb, "INSERT INTO %s (%s) VALUES (%s)",
			quoteIdent(orgTable), strings.Join(cols, ", "), strings.Join(holes, ", "))
	}
	fmt.Fprintf(&sb, " RETURNING %s", orgSelect)

	return scanOrg(db.QueryRow(ctx, sb.String(), args.values...))
}

// UpdateOrg writes the named columns of one row and reads the row back. An
// empty change set is the caller's mistake rather than a statement with no SET
// clause, which Postgres will not parse.
func UpdateOrg(ctx context.Context, db DB, id any, changes map[string]any, where []Condition) (Org, error) {
	order := []string{"name", "slug"}

	var sets []string
	args := new(args)
	for _, name := range order {
		v, ok := changes[name]
		if !ok {
			continue
		}
		sets = append(sets, quoteIdent(name)+" = "+args.add(v))
	}
	var zero Org
	if len(sets) == 0 {
		return zero, ErrNoChanges
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "UPDATE %s SET %s", quoteIdent(orgTable), strings.Join(sets, ", "))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))
	fmt.Fprintf(&sb, " RETURNING %s", orgSelect)

	row, err := scanOrg(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrNotFound
		}
		return zero, err
	}
	return row, nil
}

// DeleteOrg removes one row, and reports ErrNotFound rather than success when
// the id matched nothing the conditions admit.
func DeleteOrg(ctx context.Context, db DB, id any, where []Condition) error {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "DELETE FROM %s", quoteIdent(orgTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	tag, err := db.Exec(ctx, sb.String(), args.values...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// postTable is the table Post maps to.
const postTable = "posts"

// postSelect is the projection: every column, in declaration order.
const postSelect = "\"id\", \"org_id\", \"author_id\", \"title\", \"body\", \"status\", \"view_count\", \"published_at\", \"created_at\", \"updated_at\", \"deleted_at\""

// postColumns is what a request may name, and for what.
var postColumns = []Column{
	{Name: "id", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "org_id", Filterable: false, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "author_id", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText},
	{Name: "title", Filterable: true, Sortable: true, Searchable: true, Parse: ParseText},
	{Name: "body", Filterable: true, Sortable: false, Searchable: true, Parse: ParseText},
	{Name: "status", Filterable: true, Sortable: true, Searchable: false, Parse: ParseText},
	{Name: "view_count", Filterable: true, Sortable: true, Searchable: false, Parse: ParseInt},
	{Name: "published_at", Filterable: true, Sortable: true, Searchable: false, Parse: ParseTime},
	{Name: "created_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
	{Name: "updated_at", Filterable: false, Sortable: true, Searchable: false, Parse: ParseTime},
	{Name: "deleted_at", Filterable: false, Sortable: false, Searchable: false, Parse: ParseTime},
}

// scanPost reads one row of postSelect.
func scanPost(s scanner) (Post, error) {
	var row Post
	err := s.Scan(
		&row.ID,
		&row.OrgID,
		&row.AuthorID,
		&row.Title,
		&row.Body,
		&row.Status,
		&row.ViewCount,
		&row.PublishedAt,
		&row.CreatedAt,
		&row.UpdatedAt,
		&row.DeletedAt,
	)
	return row, err
}

// ListPost reads a page. It returns one row more than asked for when there is
// one, which is how the handler answers has_more without a second count.
func ListPost(ctx context.Context, db DB, q Query) ([]Post, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", postSelect, quoteIdent(postTable))
	writeWhere(&sb, args, q.Where)
	writeOrder(&sb, q.Order)
	writeLimit(&sb, q.Limit, q.Offset)

	rows, err := db.Query(ctx, sb.String(), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Post
	for rows.Next() {
		row, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountPost is ?count=exact: the size of the matching set, which costs a second
// query over the same predicate.
func CountPost(ctx context.Context, db DB, where []Condition) (int64, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT count(*) FROM %s", quoteIdent(postTable))
	writeWhere(&sb, args, where)

	var n int64
	err := db.QueryRow(ctx, sb.String(), args.values...).Scan(&n)
	return n, err
}

// GetPost reads one row by primary key. The extra conditions are whatever
// confines this table — a tenant, a soft delete — and they are part of the
// lookup rather than a check afterwards, so a row outside them is a 404 and not
// a 403 that confirms it exists.
func GetPost(ctx context.Context, db DB, id any, where []Condition) (Post, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %s FROM %s", postSelect, quoteIdent(postTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	row, err := scanPost(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, err
	}
	return row, nil
}

// InsertPost writes one row and reads it back, so database defaults and computed
// columns arrive without a second query.
//
// The column order is the schema's, not the map's: a generated statement whose
// text depends on map iteration is a statement that cannot be diffed.
func InsertPost(ctx context.Context, db DB, values map[string]any) (Post, error) {
	order := []string{"org_id", "author_id", "title", "body", "status", "published_at"}

	var cols, holes []string
	args := new(args)
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		cols = append(cols, quoteIdent(name))
		holes = append(holes, args.add(v))
	}

	var sb strings.Builder
	if len(cols) == 0 {
		fmt.Fprintf(&sb, "INSERT INTO %s DEFAULT VALUES", quoteIdent(postTable))
	} else {
		fmt.Fprintf(&sb, "INSERT INTO %s (%s) VALUES (%s)",
			quoteIdent(postTable), strings.Join(cols, ", "), strings.Join(holes, ", "))
	}
	fmt.Fprintf(&sb, " RETURNING %s", postSelect)

	return scanPost(db.QueryRow(ctx, sb.String(), args.values...))
}

// UpdatePost writes the named columns of one row and reads the row back. An
// empty change set is the caller's mistake rather than a statement with no SET
// clause, which Postgres will not parse.
func UpdatePost(ctx context.Context, db DB, id any, changes map[string]any, where []Condition) (Post, error) {
	order := []string{"org_id", "author_id", "title", "body", "status", "published_at"}

	var sets []string
	args := new(args)
	for _, name := range order {
		v, ok := changes[name]
		if !ok {
			continue
		}
		sets = append(sets, quoteIdent(name)+" = "+args.add(v))
	}
	var zero Post
	if len(sets) == 0 {
		return zero, ErrNoChanges
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "UPDATE %s SET %s", quoteIdent(postTable), strings.Join(sets, ", "))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))
	fmt.Fprintf(&sb, " RETURNING %s", postSelect)

	row, err := scanPost(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrNotFound
		}
		return zero, err
	}
	return row, nil
}

// DeletePost removes one row, and reports ErrNotFound rather than success when
// the id matched nothing the conditions admit.
func DeletePost(ctx context.Context, db DB, id any, where []Condition) error {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "DELETE FROM %s", quoteIdent(postTable))
	writeWhere(&sb, args, append([]Condition{{Column: "id", Op: OpEq, Value: id}}, where...))

	tag, err := db.Exec(ctx, sb.String(), args.values...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
