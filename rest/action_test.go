package rest_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// CompletePost is what codegen emits for an action's declared body.
type CompletePost struct {
	Note *string `json:"note,omitempty"`
}

func completeSpec() rest.ActionSpec {
	return rest.ActionSpec{
		Name:    "complete",
		Path:    "/posts/{id}/complete",
		Field:   "CompletePost",
		Summary: "Complete a post",
		Writes:  []string{"status", "title"},
		HasBody: true,
	}
}

// mountAction registers the Post resource and one action against a test API.
func mountAction(t *testing.T, db sqlb.Executor, spec rest.ActionSpec,
	do func(context.Context, *Post, CompletePost) error) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Action[Post, CompletePost](api, db, postOptions(), spec, do); err != nil {
		t.Fatalf("mounting the action: %v", err)
	}
	return api
}

// The envelope is a fetch, the verb, and then a write of exactly the declared
// columns. This is the whole feature in one assertion.
func TestActionFetchesRunsAndWritesTheDeclaredColumns(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})

	var seen *Post
	var body CompletePost
	api := mountAction(t, db.db, completeSpec(), func(_ context.Context, p *Post, in CompletePost) error {
		seen = p
		body = in
		p.Status = "published"
		p.Title = "Hello, done"
		// Changed and *not* declared, so the envelope must leave it alone.
		p.Body = "rewritten"
		return nil
	})

	resp := api.Post("/posts/p1/complete", map[string]any{"note": "shipped"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	if seen == nil || seen.ID != "p1" {
		t.Fatalf("the verb was handed %+v, want the fetched row", seen)
	}
	if body.Note == nil || *body.Note != "shipped" {
		t.Errorf("the verb was handed body %+v, want the decoded note", body)
	}

	stmt := db.lastStatement()
	if !strings.HasPrefix(stmt, `UPDATE "posts"`) {
		t.Fatalf("the last statement is not the update:\n%s", stmt)
	}
	// The SET clause only. RETURNING names every column by construction, so an
	// assertion over the whole statement would pass on any write set at all.
	set, _, _ := strings.Cut(stmt, " WHERE ")
	for _, want := range []string{`"status"`, `"title"`} {
		if !strings.Contains(set, want) {
			t.Errorf("update is missing %q:\n%s", want, set)
		}
	}
	// The verb changed body; the declaration did not name it. A write set that
	// is merely documentation is the failure this asserts against.
	if strings.Contains(set, `"body"`) {
		t.Errorf("the update wrote a column the action did not declare:\n%s", set)
	}
}

// A Hidden column in Writes is the case the default projection cannot serve.
// The fetch selects every *non-hidden* column and the write-back reads its
// values off the struct the verb mutated, so the verb used to be handed a zero
// value for exactly the columns Hidden exists for — a secret, an internal
// counter — and any read-modify-write on one persisted a value derived from
// zero over the stored one, under the FOR UPDATE lock whose whole purpose is
// that this cannot happen (#67).
func TestAnActionWritingAHiddenColumnFetchesIt(t *testing.T) {
	cols := append(postCols(), "secret")
	row := append(postRow("p1", "Hello"), "stored-secret")
	db := newFakeDB(t, reply{cols: cols, rows: [][]any{row}})

	spec := completeSpec()
	spec.Writes = []string{"secret"}

	var seen string
	api := mountAction(t, db.db, spec, func(_ context.Context, p *Post, _ CompletePost) error {
		seen = p.Secret
		p.Secret = p.Secret + "-rotated"
		return nil
	})

	resp := api.Post("/posts/p1/complete", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	if seen != "stored-secret" {
		t.Errorf("the verb was handed secret = %q, want the stored value", seen)
	}

	var fetch string
	for _, stmt := range db.statements() {
		if strings.HasPrefix(stmt, "SELECT") {
			fetch = stmt
			break
		}
	}
	// The load-bearing assertion, and the one that fails without the fix: the
	// fake answers with the columns it was scripted with rather than with the
	// ones the statement asked for, so what a real Postgres would withhold is
	// visible here only in the SQL.
	if !strings.Contains(fetch, `"secret"`) {
		t.Errorf("the fetch does not project the column the action writes:\n%s", fetch)
	}

	// Fetched and written, but still never serialised: projecting a Hidden
	// column for the write-back must not put it in the response.
	if strings.Contains(resp.Body.String(), "secret") {
		t.Errorf("the hidden column reached the response: %s", resp.Body)
	}
}

// A declared write set means read-modify-write, and a read-modify-write across
// a round trip that does not lock is a lost update waiting for a second client.
func TestAnActionThatWritesLocksTheRow(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mountAction(t, db.db, completeSpec(), func(context.Context, *Post, CompletePost) error { return nil })

	api.Post("/posts/p1/complete", map[string]any{})

	var fetch string
	for _, stmt := range db.statements() {
		if strings.HasPrefix(stmt, "SELECT") {
			fetch = stmt
			break
		}
	}
	if !strings.Contains(fetch, "FOR UPDATE") {
		t.Errorf("the fetch does not lock the row:\n%s", fetch)
	}
}

// An action that declares no write set is not doing a read-modify-write, so it
// pays neither for the lock nor for the UPDATE.
func TestAnActionThatWritesNothingTakesNoLockAndIssuesNoUpdate(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	spec := completeSpec()
	spec.Writes = nil
	api := mountAction(t, db.db, spec, func(context.Context, *Post, CompletePost) error { return nil })

	resp := api.Post("/posts/p1/complete", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	for _, stmt := range db.statements() {
		if strings.Contains(stmt, "FOR UPDATE") {
			t.Errorf("a verb that writes nothing locked the row:\n%s", stmt)
		}
		if strings.HasPrefix(stmt, "UPDATE") {
			t.Errorf("a verb that declares no write set issued an update:\n%s", stmt)
		}
	}
}

// The verb runs inside the unit of work, so a failure rolls the fetch and any
// statement the verb issued back rather than committing half a transition.
func TestAFailingVerbRollsBack(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mountAction(t, db.db, completeSpec(), func(context.Context, *Post, CompletePost) error {
		return errors.New("the domain said no")
	})

	resp := api.Post("/posts/p1/complete", map[string]any{})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.Code, resp.Body)
	}
	if got := db.statements(); got[len(got)-1] != "ROLLBACK" {
		t.Errorf("the transaction did not roll back: %v", got)
	}
	// And the reason is not handed to the client, the way no other server-side
	// failure is.
	if strings.Contains(resp.Body.String(), "the domain said no") {
		t.Errorf("the verb's error leaked into the response: %s", resp.Body)
	}
}

// The escape hatch: "cannot complete an archived post" is a 409, and saying so
// needs no mechanism this package did not already have.
func TestAVerbReturningAProblemKeepsItsStatus(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mountAction(t, db.db, completeSpec(), func(context.Context, *Post, CompletePost) error {
		return &rest.Problem{
			Title:  http.StatusText(http.StatusConflict),
			Status: http.StatusConflict,
			Detail: "an archived post cannot be completed",
		}
	})

	resp := api.Post("/posts/p1/complete", map[string]any{})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), "an archived post cannot be completed") {
		t.Errorf("the verb's own detail was replaced: %s", resp.Body)
	}
}

// A row that is not there is a 404, and the verb never runs.
func TestAnActionOnAMissingRowIs404(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	ran := false
	api := mountAction(t, db.db, completeSpec(), func(context.Context, *Post, CompletePost) error {
		ran = true
		return nil
	})

	resp := api.Post("/posts/nope/complete", map[string]any{})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
	if ran {
		t.Error("the verb ran on a row that does not exist")
	}
}

// The compiler cannot insist that a func field is non-nil, so this is where
// that half lands: at mount, naming the field to go and set.
func TestMountRefusesAnActionWithNoFunc(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Action[Post, CompletePost](api, db.db, postOptions(), completeSpec(), nil)
	if err == nil {
		t.Fatal("mounting an action with no func succeeded")
	}
	for _, want := range []string{"complete", "Actions.CompletePost", "is nil"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A typo in Writes is a route that answers 200 having written nothing, which is
// the least debuggable outcome available. The schema validator catches it for a
// declared schema; this is the same refusal for a hand-written model.
func TestMountRefusesAWriteSetNamingNoColumn(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	spec := completeSpec()
	spec.Writes = []string{"stauts"}
	err := rest.Action[Post, CompletePost](api, db.db, postOptions(), spec,
		func(context.Context, *Post, CompletePost) error { return nil })
	if err == nil {
		t.Fatal("mounting an action with an unknown write column succeeded")
	}
	if !strings.Contains(err.Error(), "stauts") {
		t.Errorf("error does not name the column: %v", err)
	}
}

// The safety half of ADR-0043. Hand-written verb handlers are where the tenant
// predicate is remembered by hand, so an action whose fetch is generated
// inherits ADR-0030's obligation — and refuses to mount without the hook.
func TestAnActionOnAScopedModelObligesTheReadHook(t *testing.T) {
	spec := rest.ActionSpec{
		Name: "publish", Path: "/scoped/{id}/publish", Field: "PublishScoped",
		Writes: []string{"title"},
	}
	do := func(context.Context, *Scoped, CompletePost) error { return nil }

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	bare := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())
	err := rest.Action[Scoped, CompletePost](api, bare, scopedOptions(), spec, do)
	if err == nil {
		t.Fatal("an action mounted on a confined model with nothing confining it")
	}
	for _, want := range []string{"BeforeQuery", "org_id is Scoped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}

	// A BeforeQuery is enough, and deliberately so: the row this writes is one
	// the envelope fetched under that predicate. A PATCH needs its own
	// BeforeUpdate because its id comes from the request; this does not.
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil })
	hooked := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	_, api2 := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Action[Scoped, CompletePost](api2, hooked, scopedOptions(), spec, do); err != nil {
		t.Fatalf("mounting an action whose read hook is registered: %v", err)
	}
}

// An action named for an operation the resource serves is a second operation
// with the same id, and huma.AddOperation panics on the second. Mounting is a
// path whose every other refusal is a returned error naming the declaration to
// change, so this one is too — and it names both operations, which the panic
// does not.
//
// The schema package refuses the same pair at declaration time, which is where
// it is fixable. This is the half of it that survives ADR-0010: the DSL is
// optional, so a guard that only exists there leaves the hand-written mount as
// the unguarded one.
func TestAnActionCannotTakeTheIDOfAMountedOperation(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, postOptions()); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}

	spec := completeSpec()
	spec.Name, spec.Path, spec.Field = "create", "/posts/{id}/create", "CreatePost"
	err := rest.Action[Post, CompletePost](api, db.db, postOptions(), spec,
		func(context.Context, *Post, CompletePost) error { return nil })

	if err == nil {
		t.Fatal("an action taking a mounted operation's id was accepted")
	}
	// The id, the operation already holding it, and the way out.
	for _, want := range []string{`action "create"`, "create-post", "POST /posts", "Options.Ops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}

	// The collection form is the same refusal, and it is the shape that
	// actually occurs: POST /posts/create beside POST /posts.
	spec.Path = "/posts/create"
	err = rest.CollectionAction[CompletePost](api, db.db, postOptions(), spec,
		func(context.Context, CompletePost) error { return nil })
	if err == nil {
		t.Fatal("a collection action taking a mounted operation's id was accepted")
	}
}

// The id is what has to be free, not the word. A verb spelled like an operation
// the resource does not expose collides with nothing, and refusing it would
// refuse the read-only resource whose one write is a declared verb.
func TestAnActionMayTakeTheVerbOfAnOperationThatIsNotMounted(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	opts := postOptions()
	opts.Ops = rest.OpRead | rest.OpList
	if err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, opts); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}

	spec := completeSpec()
	spec.Name, spec.Path, spec.Field = "create", "/posts/create", "CreatePost"
	if err := rest.CollectionAction[CompletePost](api, db.db, opts, spec,
		func(context.Context, CompletePost) error { return nil }); err != nil {
		t.Fatalf("a verb no mounted operation holds was refused: %v", err)
	}
}

// A collection action has no row: no fetch, no lock, no write set, 204.
func TestACollectionActionFetchesNothingAndAnswers204(t *testing.T) {
	db := newFakeDB(t)
	ran := false
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.CollectionAction[CompletePost](api, db.db, postOptions(), rest.ActionSpec{
		Name: "purge", Path: "/posts/purge", Field: "PurgePost", HasBody: true,
	}, func(context.Context, CompletePost) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("mounting the collection action: %v", err)
	}

	resp := api.Post("/posts/purge", map[string]any{})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	if !ran {
		t.Error("the verb did not run")
	}
	for _, stmt := range db.statements() {
		if strings.HasPrefix(stmt, "SELECT") {
			t.Errorf("a collection action fetched a row:\n%s", stmt)
		}
	}
}

// The operation's description states what the verb reaches beyond the row it
// answers with.
//
// This is the surface the report singled out (#149): a write set of two columns
// is what the envelope persists, and a reader given only that concludes the
// route is confined to one row. The correction has to be in the document the
// client generator and the agent read, not only in the schema file.
func TestTheOperationDescriptionCarriesTheDeclaredReach(t *testing.T) {
	db := newFakeDB(t)
	spec := completeSpec()
	spec.Description = "Close the post and note why."
	spec.Touches = []string{"comments", "audit_log"}

	api := mountAction(t, db.db, spec, func(context.Context, *Post, CompletePost) error { return nil })

	op := api.OpenAPI().Paths["/posts/{id}/complete"].Post
	if op == nil {
		t.Fatal("the action is not in the document")
	}
	for _, want := range []string{
		// The schema's own prose survives; the reach is appended to it.
		"Close the post and note why.",
		"comments, audit_log",
		"declared rather than enforced",
	} {
		if !strings.Contains(op.Description, want) {
			t.Errorf("description is missing %q:\n%s", want, op.Description)
		}
	}
}

// A verb that declares no reach gets its description back unchanged, rather
// than a paragraph of hedging on every operation in the document.
func TestAnUndeclaredReachAddsNothingToTheDescription(t *testing.T) {
	db := newFakeDB(t)
	spec := completeSpec()
	spec.Description = "Close the post."

	api := mountAction(t, db.db, spec, func(context.Context, *Post, CompletePost) error { return nil })

	op := api.OpenAPI().Paths["/posts/{id}/complete"].Post
	if op.Description != "Close the post." {
		t.Errorf("description = %q, want the schema's own text untouched", op.Description)
	}
}
