package recipes_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// The support code the recipes share. Nothing here is part of sqlb's API;
// it exists so that each recipe file can be about one thing.
//
// Most recipes never reach this file. A query is a value, so compiling it with
// SQL() shows what would run without running it — which is both the honest way
// to demonstrate a query builder and the reason these examples need no
// database. The few recipes that must *execute* something — hooks fire on
// execution, and a transaction is not a statement — run against the recording
// executor below.

// compiled is what Builder, Insert, Update and Delete all satisfy: something
// that renders SQL text and bind parameters without running them.
type compiled interface {
	SQL() (string, []any, error)
}

// show prints a whole statement and its bind parameters. Use it when the shape
// of the statement is the point — a projection, a join, a LIMIT.
func show(c compiled) {
	sql, args, err := c.SQL()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(sql)
	if len(args) > 0 {
		fmt.Println("args:", formatArgs(args))
	}
}

// showWhere prints everything from WHERE onwards. Use it when the predicate is
// the point, which for a model with a dozen columns is most of the time: the
// default projection is thirty words of noise in front of the six that matter.
func showWhere(c compiled) {
	sql, args, err := c.SQL()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	_, where, ok := strings.Cut(sql, " WHERE ")
	if !ok {
		fmt.Println("(no WHERE clause)")
		return
	}
	fmt.Println("WHERE", where)
	if len(args) > 0 {
		fmt.Println("args:", formatArgs(args))
	}
}

// formatArgs renders bind parameters as text rather than as Go values, so a
// jsonb document reads as a document instead of as a list of byte numbers.
// Everything else is already the value pgx will encode: since ADR-0040 an
// array parameter is a plain slice rather than something pre-encoded.
func formatArgs(args []any) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case json.RawMessage:
			parts[i] = string(v)
		case []byte:
			parts[i] = string(v)
		default:
			parts[i] = fmt.Sprint(v)
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// showError prints an error, or says there was none. Several recipes are about
// a refusal, and the message is the recipe.
func showError(err error) {
	if err == nil {
		fmt.Println("(no error)")
		return
	}
	fmt.Println(err)
}

// showFilterErrors prints every rejected parameter rather than only the first.
// Reporting them all is the package's own promise, and a recipe that printed
// one would hide it.
func showFilterErrors(err error) {
	errs, ok := filter.AsErrors(err)
	if !ok {
		showError(err)
		return
	}
	for _, e := range errs {
		fmt.Println("filter:", e)
	}
}

func showConst(name, value string) { fmt.Printf("%s = %s\n", name, value) }

// showContains reports whether compiled SQL mentions something, for the recipes
// whose claim is that a column is *absent* from a statement.
func showContains(sql, want string) {
	fmt.Printf("mentions %s: %v\n", want, strings.Contains(sql, want))
}

func showExpanded(names []string) { fmt.Println("expanded:", names) }

// showDecodedCursor prints what a cursor decodes to. It is base64 over JSON and
// nothing else, which is the point being made where this is called.
func showDecodedCursor(c sqlb.Cursor) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(string(c), "="))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))
}

// firstWords shortens a recorded statement to its opening words, for the
// recipes whose claim is about the *order* statements ran in rather than their
// contents.
func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

func count(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

func showArgCount(args []any) {
	if len(args) == 1 {
		fmt.Println("1 bind parameter")
		return
	}
	fmt.Printf("%d bind parameters\n", len(args))
}

// postColumns is the projection Query[Post] produces by default, in declaration
// order. The recording executor replays a row this wide.
var postColumns = []string{
	"id", "org_id", "author_id", "title", "body", "status",
	"view_count", "tags", "metadata", "published_at", "deleted_at", "created_at",
}

// postRow is one canned row matching postColumns. The values are Go values
// rather than wire encodings, which is what pgx hands a scanner: an array
// column arrives as a slice and a document as its bytes, so neither needs
// decoding on the way in.
func postRow() []any {
	published := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	return []any{
		"p1", "acme", "a1", "Hello", "Body text", "published",
		int64(12), []string{"go", "sql"}, json.RawMessage(`{"lang":"en"}`),
		&published, (*time.Time)(nil),
		time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
	}
}

// recordingDB returns a handle over the recording executor and clears the log.
// The canned result is one Post; pass different columns for a query that
// projects something else.
func recordingDB() *sqlb.DB {
	return recordingDBWith(postColumns, postRow())
}

func recordingDBWith(cols []string, rows ...[]any) *sqlb.DB {
	log = nil
	replay.cols = cols
	replay.rows = rows
	replay.err = nil
	return sqlb.New(recordingExec{})
}

// failingDB is a handle whose every statement fails with err, for the recipes
// about what an application does with the failure.
func failingDB(err error) *sqlb.DB {
	db := recordingDBWith(postColumns, postRow())
	replay.err = err
	return db
}

// statements returns every statement the executor saw, including BEGIN and
// COMMIT. A recipe about transactions is a recipe about that sequence.
func statements() []string { return log }

// lastWhere returns the predicate of the statement that ran most recently, so a
// recipe can show what a hook contributed without printing the projection or
// the RETURNING clause the hook did not touch.
func lastWhere() string {
	if len(log) == 0 {
		return "(no statement ran)"
	}
	_, where, ok := strings.Cut(log[len(log)-1], " WHERE ")
	if !ok {
		return "(no WHERE clause)"
	}
	predicate, _, _ := strings.Cut(where, " RETURNING ")
	return predicate
}

// The recording executor. Since ADR-0040 an Executor is pgx-shaped, so a canned
// result has to be a pgx.Rows — nine methods — and a transaction a pgx.Tx,
// which is eleven. internal/pgfake owns that boilerplate so the packages
// needing it cannot drift apart; what stays here is the policy: what a
// statement answers, and what gets recorded.

var (
	log    []string
	replay struct {
		cols []string
		rows [][]any
		// err, when set, is what the executor returns instead of a result. It
		// is how a recipe about a refused write gets one to be about.
		err error
	}
)

type recordingExec struct{}

func (recordingExec) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	log = append(log, sql)
	if replay.err != nil {
		return nil, replay.err
	}
	// A count is one column whatever the model is, so the canned projection
	// would not fit it.
	if strings.HasPrefix(sql, "SELECT count(") {
		return &pgfake.Rows{Cols: []string{"count"}, Data: [][]any{{int64(len(replay.rows))}}}, nil
	}
	return &pgfake.Rows{Cols: replay.cols, Data: replay.rows}, nil
}

func (recordingExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	log = append(log, sql)
	if replay.err != nil {
		return pgconn.CommandTag{}, replay.err
	}
	return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", len(replay.rows))), nil
}

// BeginTx is what makes WithTx work through this executor. Beginner is asserted
// for rather than required, so Executor stays two methods and a wrapper that
// wants transactions to pass through implements this alongside them.
func (e recordingExec) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	log = append(log, "BEGIN")
	return &pgfake.Tx{
		Statements: e,
		OnCommit:   func() error { log = append(log, "COMMIT"); return nil },
		OnRollback: func() error { log = append(log, "ROLLBACK"); return nil },
	}, nil
}
