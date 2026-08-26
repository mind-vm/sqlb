package rest_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// The half of #311 that the codegen assertions cannot make: that the tag a
// declared format rule becomes is one Huma actually enforces, and that the rule
// reaches the document a caller reads.
//
// codegen proves the tag is written. This proves writing it is worth anything —
// without it, "emitted as the corresponding struct tag, where Huma picks it up
// with no further work" is an assumption about somebody else's library sitting
// underneath the whole feature.

// Kid carries a PIN whose format is declared rather than checked in a func.
type Kid struct {
	ID  string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	PIN string `db:"pin" json:"pin"`
}

func (Kid) TableName() string { return "kids" }

// KidCreate is the shape codegen emits for a Varchar(4).Pattern(`^[0-9]{4}$`).
type KidCreate struct {
	PIN string `json:"pin" pattern:"^[0-9]{4}$"`
}

func (c KidCreate) Row() (*Kid, error) { return &Kid{PIN: c.PIN}, nil }

func mountKids(t *testing.T, db sqlb.Executor) (humatest.TestAPI, huma.API) {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Kid, KidCreate, rest.None[Kid]](api, db, rest.Options{
		Path: "/kids", Name: "kid", Ops: rest.OpCreate | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api, api
}

func TestADeclaredPatternIsEnforcedByTheServer(t *testing.T) {
	fake := newFakeDB(t, reply{cols: []string{"id", "pin"}, rows: [][]any{{"k1", "4242"}}})
	api, _ := mountKids(t, sqlb.New(fake.db))

	// The value the hand-written regexp used to catch, now caught before the
	// transition func is reached at all.
	resp := api.Post("/kids", map[string]any{"pin": "42"})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a PIN that is not four digits: %s", resp.Code, resp.Body)
	}
	// And no statement ran: a rejected request must not have reached the table.
	if stmt := fake.lastStatement(); stmt != "" {
		t.Errorf("a request rejected by validation still issued a statement:\n%s", stmt)
	}
}

// The other direction, without which the assertion above is satisfied by
// rejecting everything.
func TestAValueMatchingTheDeclaredPatternIsAccepted(t *testing.T) {
	fake := newFakeDB(t, reply{cols: []string{"id", "pin"}, rows: [][]any{{"k1", "4242"}}})
	api, _ := mountKids(t, sqlb.New(fake.db))

	resp := api.Post("/kids", map[string]any{"pin": "4242"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
}

// And the rule reaches the document, which is the reason for declaring it
// rather than writing it in Go: a caller with no compile step discovers it
// without sending a bad request.
func TestADeclaredPatternReachesTheOpenAPIDocument(t *testing.T) {
	fake := newFakeDB(t, reply{cols: []string{"id", "pin"}, rows: [][]any{{"k1", "4242"}}})
	_, api := mountKids(t, sqlb.New(fake.db))

	body := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[KidCreate](), false, "KidCreate")
	prop, ok := body.Properties["pin"]
	if !ok {
		t.Fatalf("the create body schema has no pin property: %+v", body.Properties)
	}
	if prop.Pattern != "^[0-9]{4}$" {
		t.Errorf("the document does not carry the declared pattern, got %q", prop.Pattern)
	}
}
