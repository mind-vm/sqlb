package rest_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// The models under test mirror what codegen emits: db tags for column names,
// sqlb tags for capabilities, json tags for the wire, and `json:"-"` on the
// hidden column so it cannot be marshalled by accident.
type Post struct {
	ID    string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	OrgID string `db:"org_id" json:"org_id" sqlb:"filter,immutable"`
	Title string `db:"title" json:"title" sqlb:"filter,sort,search"`
	Body  string `db:"body" json:"body" sqlb:"search"`
	// Excerpt declares nothing, so it is readable but not filterable, sortable
	// or searchable — the default an opt-in capability model gives a column.
	Excerpt   string    `db:"excerpt" json:"excerpt"`
	Tags      []string  `db:"tags" json:"tags" sqlb:"default,filter"`
	Status    string    `db:"status" json:"status" sqlb:"default,filter,sort"`
	ViewCount int64     `db:"view_count" json:"view_count" sqlb:"default,filter,sort,readonly"`
	Secret    string    `db:"secret" json:"-" sqlb:"hidden"`
	CreatedAt time.Time `db:"created_at" json:"created_at" sqlb:"default,sort,readonly"`
}

func (Post) TableName() string { return "posts" }

// PostCreate is the create body: writable columns only, with the defaulted ones
// optional so the database supplies them when the request stays quiet.
type PostCreate struct {
	OrgID  string  `json:"org_id"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Status *string `json:"status,omitempty"`
}

func (c PostCreate) Row() (*Post, error) {
	p := &Post{OrgID: c.OrgID, Title: c.Title, Body: c.Body}
	if c.Status != nil {
		p.Status = *c.Status
	}
	return p, nil
}

// PostUpdate is the patch body: every field a pointer, so absent and zero are
// distinguishable.
type PostUpdate struct {
	Title  *string `json:"title,omitempty"`
	Body   *string `json:"body,omitempty"`
	Status *string `json:"status,omitempty"`
	OrgID  *string `json:"org_id,omitempty"`
}

func (u PostUpdate) Changes() (map[string]any, error) {
	out := map[string]any{}
	if u.Title != nil {
		out["title"] = *u.Title
	}
	if u.Body != nil {
		out["body"] = *u.Body
	}
	if u.Status != nil {
		out["status"] = *u.Status
	}
	if u.OrgID != nil {
		out["org_id"] = *u.OrgID
	}
	return out, nil
}

// Keyless has no primary key, so it can only be listed.
type Keyless struct {
	Name string `db:"name" json:"name" sqlb:"filter"`
}

func (Keyless) TableName() string { return "keyless" }

// Leaky is a mistake the binder should catch: a hidden column that would still
// be serialised if anything marshalled the struct.
type Leaky struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	Secret string `db:"secret" json:"secret" sqlb:"hidden"`
}

func (Leaky) TableName() string { return "leaky" }

// LeakyWriteOnly is Leaky's WriteOnly counterpart: the same mistake — a
// column that should never reach a response, carrying a real json tag on the
// row struct instead of `json:"-"` — but on the capability that is still
// supposed to be settable rather than the one that never is.
type LeakyWriteOnly struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	Answer string `db:"answer" json:"answer" sqlb:"writeonly"`
}

func (LeakyWriteOnly) TableName() string { return "leaky_write_only" }

// QuizOption is the worked case #195 was filed over: is_correct is authored
// by whoever creates the option and must never be served back to whoever is
// taking the quiz.
type QuizOption struct {
	ID        string `db:"id" json:"id" sqlb:"pk,default,readonly"`
	Body      string `db:"body" json:"body"`
	IsCorrect bool   `db:"is_correct" json:"-" sqlb:"writeonly"`
}

func (QuizOption) TableName() string { return "quiz_options" }

type QuizOptionCreate struct {
	Body      string `json:"body"`
	IsCorrect bool   `json:"isCorrect"`
}

func (c QuizOptionCreate) Row() (*QuizOption, error) {
	return &QuizOption{Body: c.Body, IsCorrect: c.IsCorrect}, nil
}

// Archived is what schema.SoftDelete produces: a nullable, read-only deleted_at
// column and nothing else. It exists to hold the runtime to that "nothing else".
type Archived struct {
	ID        string     `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	Title     string     `db:"title" json:"title" sqlb:"filter,sort"`
	DeletedAt *time.Time `db:"deleted_at" json:"deleted_at" sqlb:"readonly"`
}

func (Archived) TableName() string { return "archived" }

// reply is one canned result, matched against the statement text.
type reply struct {
	match string
	cols  []string
	rows  [][]any
	err   error
	// rowsErr is reported after the rows, standing in for a statement that
	// fails while its result is being read rather than when it is sent. pgx
	// does this on the extended protocol, so a check that only looked at what
	// Query returned would miss a constraint violation entirely.
	rowsErr error
}

// fakeDB is a database that answers from a script. Each reply matches the first
// statement containing its substring, so a test can distinguish the page query
// from the count query without a real database.
//
// It is the Executor itself — db points back at it — because ADR-0040 made pgx
// the contract and there is no driver to register any more.
type fakeDB struct {
	t  *testing.T
	db *fakeDB

	mu      sync.Mutex
	replies []reply
	log     []string
	// args are the bind parameters of each statement, so a test can assert on
	// the values a request produced and not only on the SQL around them.
	args [][]any
}

func newFakeDB(t *testing.T, replies ...reply) *fakeDB {
	t.Helper()
	f := &fakeDB{t: t, replies: replies}
	f.db = f
	return f
}

// answer picks the reply for a statement, recording the statement either way.
func (f *fakeDB) answer(query string, args []any) (reply, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, query)
	f.args = append(f.args, args)
	for _, r := range f.replies {
		if r.match == "" || strings.Contains(query, r.match) {
			return r, true
		}
	}
	return reply{}, false
}

// record logs a statement that carries no bind parameters, keeping the args
// slice aligned with the log so lastArgs stays meaningful.
func (f *fakeDB) record(stmt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, stmt)
	f.args = append(f.args, nil)
}

// statements returns every statement the handler issued, in order.
func (f *fakeDB) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// lastStatement is the most recent statement, for asserting on compiled SQL.
//
// Transaction markers are skipped: a write is wrapped by default, so the raw
// last entry is COMMIT and no assertion here has ever been about that. Tests
// that care whether a write was wrapped read statements() instead.
func (f *fakeDB) lastStatement() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.lastRealLocked(); i >= 0 {
		return f.log[i]
	}
	return ""
}

// lastArgs is the bind parameters of the most recent statement, skipping the
// transaction markers for the same reason lastStatement does.
func (f *fakeDB) lastArgs() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.lastRealLocked(); i >= 0 {
		return append([]any(nil), f.args[i]...)
	}
	return nil
}

// lastRealLocked is the index of the most recent statement that is not a
// transaction marker, or -1. Callers hold f.mu.
func (f *fakeDB) lastRealLocked() int {
	for i := len(f.log) - 1; i >= 0; i-- {
		switch f.log[i] {
		case "BEGIN", "COMMIT", "ROLLBACK":
		default:
			return i
		}
	}
	return -1
}

// Generated writes run in a transaction by default, so the fake has to be able
// to open one. BEGIN, COMMIT and ROLLBACK go into the same statement log as
// everything else, which is what lets a test assert that a write was wrapped —
// and, for a failing write, that it rolled back rather than committing.
func (f *fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	f.record("BEGIN")
	return &pgfake.Tx{
		Statements: f,
		OnCommit:   func() error { f.record("COMMIT"); return nil },
		OnRollback: func() error { f.record("ROLLBACK"); return nil },
	}, nil
}

func (f *fakeDB) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	r, ok := f.answer(query, args)
	if !ok {
		return &pgfake.Rows{}, nil
	}
	if r.err != nil {
		return nil, r.err
	}
	return &pgfake.Rows{Cols: r.cols, Data: r.rows, Fail: r.rowsErr}, nil
}

func (f *fakeDB) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	r, ok := f.answer(query, args)
	if !ok {
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", len(r.rows))), nil
}

// postRow is the column set and one row of it, as the page query returns them.
func postCols() []string {
	return []string{"id", "org_id", "title", "body", "excerpt", "status", "view_count", "created_at"}
}

func postRow(id, title string) []any {
	return []any{id, "acme", title, "body text", "excerpt text", "draft", int64(3), time.Unix(0, 0).UTC()}
}

// Tenanted is the shape the ReadOnly-plus-hook pattern needs, and the one the
// Post model above cannot express: a read-only column with no database default,
// whose value a BeforeCreate hook supplies. A tenant id and an author id are
// both this.
type Tenanted struct {
	ID string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	// No `default`: nothing in the database will fill this in, so if the hook's
	// value does not reach the INSERT the row is written with a NULL.
	TenantID string `db:"tenant_id" json:"tenant_id" sqlb:"filter,readonly"`
	Title    string `db:"title" json:"title" sqlb:"filter,sort"`
}

func (Tenanted) TableName() string { return "tenanted" }

// TenantedCreate is what codegen would emit: the read-only columns are absent.
type TenantedCreate struct {
	Title string `json:"title"`
}

func (c TenantedCreate) Row() (*Tenanted, error) { return &Tenanted{Title: c.Title}, nil }

// SmugglingCreate is the hand-written body the clearing defends against: it
// sets a read-only column the schema says a request may not write.
type SmugglingCreate struct {
	Title    string `json:"title"`
	TenantID string `json:"tenant_id"`
}

func (c SmugglingCreate) Row() (*Tenanted, error) {
	return &Tenanted{Title: c.Title, TenantID: c.TenantID}, nil
}

func tenantedCols() []string { return []string{"id", "tenant_id", "title"} }

func tenantedRow(id, tenant, title string) []any {
	return []any{id, tenant, title}
}

func archivedCols() []string {
	return []string{"id", "title", "deleted_at"}
}

// archivedRow carries a non-null deleted_at, which is the row an unfiltered
// read is expected to return.
func archivedRow(id, title string) []any {
	return []any{id, title, time.Unix(0, 0).UTC()}
}

// Expandable models, for ?expand. The relation is the two-field shape the
// engine expects: `expand` on the column, `expands=` on the field beside it.
type Org struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	Name   string `db:"name" json:"name"`
	Secret string `db:"secret" json:"-" sqlb:"hidden"`
}

func (Org) TableName() string { return "orgs" }

type Doc struct {
	ID    string `db:"id" json:"id" sqlb:"pk,default"`
	OrgID string `db:"org_id" json:"org_id" sqlb:"filter,expand"`
	Title string `db:"title" json:"title" sqlb:"filter,sort"`

	Org *Org `db:"-" json:"org,omitempty" sqlb:"expands=org_id"`
}

func (Doc) TableName() string { return "docs" }

func docCols() []string { return []string{"id", "org_id", "title", "__expand_org"} }

func docRow(id, title string, org []byte) []any {
	return []any{id, "acme", title, org}
}

// OneToOneUser and OneToOneProfile exercise a unique-FK-backed reverse
// relation, the shape codegen emits for a Ref(...).Unique() Inverse: Profile
// is a bare pointer with the `reverse` token, not a *sqlb.Collection. Tasks
// sits beside it as an ordinary capped-collection reverse relation, so a
// schema test can prove the OpenAPI nullable fix touches one-to-one and
// leaves a real collection alone.
type OneToOneUser struct {
	ID      string                         `db:"id" json:"id" sqlb:"pk"`
	Name    string                         `db:"name" json:"name"`
	Profile *OneToOneProfile               `db:"-" json:"profile,omitempty" sqlb:"expands=user_id,reverse"`
	Tasks   *sqlb.Collection[OneToOneTask] `db:"-" json:"tasks,omitempty" sqlb:"expands=user_id,limit=50"`
}

func (OneToOneUser) TableName() string { return "one_to_one_users" }

type OneToOneProfile struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	UserID string `db:"user_id" json:"user_id"`
}

func (OneToOneProfile) TableName() string { return "one_to_one_profiles" }

type OneToOneTask struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	UserID string `db:"user_id" json:"user_id"`
}

func (OneToOneTask) TableName() string { return "one_to_one_tasks" }

// Ledger has no primary key, which a list-only resource is allowed to have.
// It is the case cursor paging cannot serve: with no unique column there is no
// tiebreaker, so no position can be named unambiguously.
type Ledger struct {
	Account string `db:"account" json:"account" sqlb:"filter,sort"`
	Amount  int64  `db:"amount" json:"amount" sqlb:"filter,sort"`
}

func (Ledger) TableName() string { return "ledger" }
