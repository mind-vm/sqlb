package rest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// A create body reporting which columns the request carried (#314).
//
// The case is the reported one: a bool whose database default is true. Its Go
// zero value is false, so "the request did not mention it" and "the request
// asked for false" are the same struct — and Insert resolves that ambiguity
// towards the default, which is right for a generated id and inverts this.
//
// What is asserted is the whole path: the sent false reaches the statement, an
// omitted field still does not, and a column the mount does not let a request
// write is not made explicit by a body that names it anyway.

// Offering has a draft flag that defaults to true, and an id the database
// supplies. Both carry defaults; only one of them has a zero worth writing.
type Offering struct {
	ID     string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	Title  string `db:"title" json:"title" sqlb:"filter,sort"`
	Active bool   `db:"active" json:"active" sqlb:"default,filter"`
}

func (Offering) TableName() string { return "offerings" }

// OfferingCreate is the shape codegen emits: an optional column is a pointer,
// so absent and false are different requests, and Explicit is that distinction
// surviving as far as the insert.
type OfferingCreate struct {
	Title  string `json:"title"`
	Active *bool  `json:"active,omitempty"`
}

func (c OfferingCreate) Row() (*Offering, error) {
	row := &Offering{Title: c.Title}
	if c.Active != nil {
		row.Active = *c.Active
	}
	return row, nil
}

func (c OfferingCreate) Explicit() []string {
	var cols []string
	if c.Active != nil {
		cols = append(cols, "active")
	}
	return cols
}

// overreachingCreate is a hand-written body naming a column the schema marks
// read-only. Row cannot write it — clearReadOnly sees to that — and Explicit
// must not be a second way in.
type overreachingCreate struct {
	Title string `json:"title"`
}

func (c overreachingCreate) Row() (*Offering, error) { return &Offering{Title: c.Title}, nil }
func (c overreachingCreate) Explicit() []string      { return []string{"id"} }

func offeringCols() []string { return []string{"id", "title", "active"} }

func mountOfferings[C rest.CreateBody[Offering]](t *testing.T, db sqlb.Executor) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Offering, C, rest.None[Offering]](api, db, rest.Options{
		Path: "/offerings", Name: "offering", Ops: rest.OpCreate | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api
}

// written returns the column list of the last INSERT, which is the half of the
// statement the question is about. RETURNING names every column whatever
// happened, so asserting over the whole statement would pass on anything.
func written(t *testing.T, fake *fakeDB) string {
	t.Helper()
	stmt := fake.lastStatement()
	if !strings.Contains(stmt, "VALUES") {
		t.Fatalf("the last statement is not an insert:\n%s", stmt)
	}
	return strings.SplitN(stmt, "VALUES", 2)[0]
}

func TestCreateWritesAnExplicitlySentZero(t *testing.T) {
	fake := newFakeDB(t, reply{cols: offeringCols(), rows: [][]any{{"o1", "Draft", false}}})
	db := sqlb.New(fake.db)

	api := mountOfferings[OfferingCreate](t, db)
	resp := api.Post("/offerings", map[string]any{"title": "Draft", "active": false})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	cols := written(t, fake)
	if !strings.Contains(cols, `"active"`) {
		t.Errorf("a sent false did not reach the statement, so the row was created active:\n%s", cols)
	}
	// The column the request did not mention is still the database's. Only(...)
	// would have taken this away, which is why the seam is Explicit.
	if strings.Contains(cols, `"id"`) {
		t.Errorf("a column the request never named must still take its default:\n%s", cols)
	}
	if strings.Contains(resp.Body.String(), `"active":true`) {
		t.Errorf("the response reports the opposite of what was asked: %s", resp.Body)
	}
}

// The other direction, and the reason the rule exists at all: a field the
// request omitted is not made explicit, so the database's default applies.
// Without this half the assertion above is satisfied by writing every column
// unconditionally, which would overwrite every default with a zero.
func TestCreateLeavesAnOmittedColumnToItsDefault(t *testing.T) {
	fake := newFakeDB(t, reply{cols: offeringCols(), rows: [][]any{{"o1", "Draft", true}}})
	db := sqlb.New(fake.db)

	api := mountOfferings[OfferingCreate](t, db)
	resp := api.Post("/offerings", map[string]any{"title": "Draft"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	if cols := written(t, fake); strings.Contains(cols, `"active"`) {
		t.Errorf("an omitted field must leave the column to its default:\n%s", cols)
	}
}

// Explicit is the body's to implement, and a hand-written body can name
// anything. A read-only column has just been zeroed by clearReadOnly, so
// honouring the name would write that zero — the read-only guarantee inverted
// by the mechanism meant to preserve an intentional one.
func TestCreateIgnoresAnExplicitNameForAReadOnlyColumn(t *testing.T) {
	fake := newFakeDB(t, reply{cols: offeringCols(), rows: [][]any{{"o1", "Draft", true}}})
	db := sqlb.New(fake.db)

	api := mountOfferings[overreachingCreate](t, db)
	resp := api.Post("/offerings", map[string]any{"title": "Draft"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	if cols := written(t, fake); strings.Contains(cols, `"id"`) {
		t.Errorf("a read-only column must not be writable through Explicit:\n%s", cols)
	}
}
