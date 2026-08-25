package restcompat_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// withActions builds a tasks resource carrying the given verbs.
func withActions(actions ...schema.Action) *schema.Registry {
	r := schema.NewRegistry()
	t := r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("status", "open", "done").Default(schema.Value("open")),
		schema.Timestamp("closed_at").Nullable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	for _, a := range actions {
		t.AddAction(a)
	}
	return r
}

func complete() schema.Action {
	return schema.Action{
		Name:   "complete",
		Body:   schema.Body(schema.Text("note").Nullable()),
		Writes: []string{"status", "closed_at"},
	}
}

// find returns the break whose summary contains want, or fails.
func find(t *testing.T, breaks []restcompat.Break, want string) restcompat.Break {
	t.Helper()
	for _, b := range breaks {
		if strings.Contains(b.Summary, want) {
			return b
		}
	}
	t.Fatalf("no break mentioning %q in %v", want, breaks)
	return restcompat.Break{}
}

// A verb is a route, and withdrawing a route breaks the client holding its URL
// — with no DDL anywhere in the change, which is the premise of `sqlb impact`.
func TestRemovingAnActionIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(withActions(complete()), withActions())

	b := find(t, breaks, "action removed")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Facet != restcompat.FacetAction {
		t.Errorf("facet = %s, want action", b.Facet)
	}
	// The URL, not just the name: that is what a deployed client actually has.
	if !strings.Contains(b.Summary, "/tasks/{id}/complete") {
		t.Errorf("summary does not name the route: %s", b.Summary)
	}
}

func TestAddingAnActionIsAdditive(t *testing.T) {
	breaks := restcompat.Diff(withActions(), withActions(complete()))

	if b := find(t, breaks, "action added"); b.Level != restcompat.LevelAdditive {
		t.Errorf("level = %s, want additive", b.Level)
	}
}

// The body is a wire format from the first deploy, which is the part of an
// action ADR-0043 calls the expensive one to get wrong.
func TestBodyPropertyChangesAreClassified(t *testing.T) {
	optional := complete()
	required := complete()
	required.Body = schema.Body(
		schema.Text("note").Nullable(),
		schema.Text("reason"), // not nullable, no default: required
	)

	added := restcompat.Diff(withActions(optional), withActions(required))
	if b := find(t, added, "required body property added"); b.Level != restcompat.LevelBreaking {
		t.Errorf("adding a required property: level = %s, want breaking", b.Level)
	}

	removed := restcompat.Diff(withActions(required), withActions(optional))
	if b := find(t, removed, "body property removed"); b.Level != restcompat.LevelBreaking {
		t.Errorf("removing a property: level = %s, want breaking", b.Level)
	}

	// Tightening an existing property is the sneakier version of the same
	// break: the name is unchanged, so a diff that only matched names would
	// report nothing.
	tightened := complete()
	tightened.Body = schema.Body(schema.Text("note"))
	got := restcompat.Diff(withActions(optional), withActions(tightened))
	if b := find(t, got, "became required"); b.Level != restcompat.LevelBreaking {
		t.Errorf("tightening a property: level = %s, want breaking", b.Level)
	}
}

// A property that changes in two ways at once is two deltas. The comparison was
// one switch, so it reported the first arm it matched and stopped — and when
// requiredness relaxes while an enum narrows, the arm it matched first was the
// additive one, hiding the break entirely (#68).
func TestTwoChangesToOneBodyPropertyAreBothReported(t *testing.T) {
	before := complete()
	before.Body = schema.Body(schema.Enum("channel", "email", "sms", "push"))

	after := complete()
	after.Body = schema.Body(schema.Enum("channel", "email").Nullable())

	breaks := restcompat.Diff(withActions(before), withActions(after))

	if b := find(t, breaks, "became optional"); b.Level != restcompat.LevelAdditive {
		t.Errorf("relaxing requiredness: level = %s, want additive", b.Level)
	}
	// The half that used to be swallowed: a client sending "sms" now 422s.
	if b := find(t, breaks, "values changed"); b.Level != restcompat.LevelBreaking {
		t.Errorf("narrowing the enum: level = %s, want breaking", b.Level)
	}
}

// An optional property is a client's business to ignore.
func TestAddingAnOptionalBodyPropertyIsAdditive(t *testing.T) {
	wider := complete()
	wider.Body = schema.Body(
		schema.Text("note").Nullable(),
		schema.Text("channel").Nullable(),
	)

	breaks := restcompat.Diff(withActions(complete()), withActions(wider))
	if b := find(t, breaks, "optional body property added"); b.Level != restcompat.LevelAdditive {
		t.Errorf("level = %s, want additive", b.Level)
	}
}

// Nothing a client sends or reads changes when the write set does — but what
// the route is allowed to mutate does, and that is the question impact exists
// to answer.
func TestAChangedWriteSetIsReportedAsNeutral(t *testing.T) {
	wider := complete()
	wider.Writes = []string{"status", "closed_at", "title"}

	breaks := restcompat.Diff(withActions(complete()), withActions(wider))
	b := find(t, breaks, "write set changed")
	if b.Level != restcompat.LevelNeutral {
		t.Errorf("level = %s, want neutral", b.Level)
	}
	if len(restcompat.Breaking(breaks)) != 0 {
		t.Errorf("a write-set change failed the strict gate: %v", restcompat.Breaking(breaks))
	}
}

// A verb whose declared reach grows is the change a reviewer most wants shown,
// and the one the diff could otherwise not see at all: no column moves, no
// route moves, and the only evidence is the claim itself (#149).
func TestAChangedReachIsReportedAsNeutral(t *testing.T) {
	before := complete()
	before.Touches = []string{"comments"}
	after := complete()
	after.Touches = []string{"comments", "inventory_reservations", "payments"}

	breaks := restcompat.Diff(withActions(before), withActions(after))
	b := find(t, breaks, "declared reach changed")
	if b.Level != restcompat.LevelNeutral {
		t.Errorf("level = %s, want neutral", b.Level)
	}
	if !strings.Contains(b.Summary, "payments") {
		t.Errorf("the summary should name the tables, got %q", b.Summary)
	}
	if len(restcompat.Breaking(breaks)) != 0 {
		t.Errorf("a reach change failed the strict gate: %v", restcompat.Breaking(breaks))
	}
}

// Reordering declarations in a schema file must not show up as a contract
// change, or every `-write` becomes a diff nobody can review.
func TestActionOrderIsNotContract(t *testing.T) {
	archive := schema.Action{Name: "archive", Writes: []string{"status"}}

	one := withActions(complete(), archive)
	two := withActions(archive, complete())
	if breaks := restcompat.Diff(one, two); len(breaks) != 0 {
		t.Errorf("reordering the declarations produced %v", breaks)
	}
}

// A snapshot recorded before actions existed has none, so every verb in the
// current schema reads as an addition rather than as an unreadable file.
func TestASnapshotWithoutActionsReadsAsHavingNone(t *testing.T) {
	old := restcompat.Capture(withActions())
	for _, r := range old.Resources {
		if len(r.Actions) != 0 {
			t.Fatalf("a schema with no verbs captured %v", r.Actions)
		}
	}
	breaks := restcompat.DiffSnapshots(old, restcompat.Capture(withActions(complete())))
	if len(restcompat.Breaking(breaks)) != 0 {
		t.Errorf("adding the first action broke the contract: %v", breaks)
	}
}
