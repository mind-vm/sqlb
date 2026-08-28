package app_test

import (
	"net/http"
	"testing"
)

// GET /tasks/by-list against a real Postgres (ADR-0043, #240, #316).
//
// The declared read's counterpart to action_test.go, and asserted for the same
// reason: what the declaration bought is the envelope, not the eight lines in
// actions.go. In order — the route exists, the answer is the shape the schema
// declared rather than rows of the table, the parameter is bound, and the
// workspace boundary holds on an aggregate, which is the one place a leak is
// hardest to see.

// rollup is ByListTaskResult as it arrives on the wire. Declared here rather
// than imported so that a change to the generated type has to be a deliberate
// change to this test too.
type rollup struct {
	ListID string `json:"list_id"`
	Open   int64  `json:"open"`
	Done   int64  `json:"done"`
}

func TestADeclaredReadAnswersWithTheShapeItDeclared(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	work := alice.listID("Work")
	home := alice.listID("Home")

	done := alice.taskID(work, "Ship it", nil)
	alice.taskID(work, "Write it up", nil)
	alice.taskID(home, "Buy milk", nil)
	alice.post("/tasks/"+done+"/complete", map[string]any{}).expect(http.StatusOK)

	var rows []rollup
	alice.get("/tasks/by-list").expect(http.StatusOK).decode(&rows)

	// Two lists, one row each — the answer is a row per group, which is a row
	// of no declared table and the reason Query.Returns exists. A []Task here
	// could not have carried the counts at all.
	if len(rows) != 2 {
		t.Fatalf("got %d groups, want one per list: %+v", len(rows), rows)
	}
	byList := map[string]rollup{}
	for _, r := range rows {
		byList[r.ListID] = r
	}
	if got := byList[work]; got.Open != 1 || got.Done != 1 {
		t.Errorf("work = %+v, want one open and one done", got)
	}
	if got := byList[home]; got.Open != 1 || got.Done != 0 {
		t.Errorf("home = %+v, want one open and none done", got)
	}
}

func TestADeclaredReadBindsItsParameter(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	work := alice.listID("Work")
	home := alice.listID("Home")
	alice.taskID(work, "Ship it", nil)
	alice.taskID(home, "Buy milk", nil)

	var rows []rollup
	alice.get("/tasks/by-list?list_id=" + work).expect(http.StatusOK).decode(&rows)

	if len(rows) != 1 || rows[0].ListID != work {
		t.Fatalf("got %+v, want only the work list", rows)
	}

	// The parameter is optional and its absence is the zero value, because
	// huma refuses a pointer on a query parameter. Omitting it is therefore a
	// request for every list rather than a 422 — asserted because the opposite
	// reading is the one a `required` tag would have produced.
	alice.get("/tasks/by-list").expect(http.StatusOK).decode(&rows)
	if len(rows) != 2 {
		t.Errorf("omitting the parameter gave %d groups, want every list: %+v", len(rows), rows)
	}

	// RejectUnknownQueryParameters is on for a declared read, so the filter
	// grammar is not quietly available on a route that never declared it.
	alice.get("/tasks/by-list?status=eq.done").expect(http.StatusUnprocessableEntity)
}

// The property that is worth a test on an aggregate specifically: a rollup
// that leaked across tenants would leak quietly, because it answers with
// numbers rather than rows and nothing in the response names the workspace it
// counted. Nothing in tasksByList mentions a workspace — the BeforeQuery hook
// narrows the GROUP BY because the func runs against the same Executor every
// generated read does.
func TestADeclaredReadIsConfinedByTheSameHookAsEveryOtherRead(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	bob := account(t, server, "bob@example.com", "Globex")

	alice.taskID(alice.listID("Work"), "Ship it", nil)
	alice.taskID(alice.listID("Home"), "Buy milk", nil)
	bobList := bob.listID("Theirs")
	bob.taskID(bobList, "Not yours", nil)

	var rows []rollup
	bob.get("/tasks/by-list").expect(http.StatusOK).decode(&rows)

	if len(rows) != 1 || rows[0].ListID != bobList {
		t.Fatalf("bob sees %+v, want only their own list", rows)
	}
	if rows[0].Open != 1 {
		t.Errorf("bob's count is %d, want 1 — alice's tasks are being counted", rows[0].Open)
	}
}
