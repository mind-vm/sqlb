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

// A create body carrying something that is not a column (#309).
//
// The case is the one the issue is about: the table stores a digest, the
// request sends the secret, and the hook is what turns one into the other. What
// is asserted here is the whole path — the property reaches the hook, the hook's
// column reaches the INSERT, and the secret itself reaches neither the statement
// nor the response.

// Child stores a hashed PIN and never a plaintext one. pin_hash is hidden, so
// it is absent from every response and from every request body.
type Child struct {
	ID      string `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	Name    string `db:"name" json:"name" sqlb:"filter,sort"`
	PinHash string `db:"pin_hash" json:"-" sqlb:"hidden"`
}

func (Child) TableName() string { return "children" }

// CreateChildInput is what codegen emits for a declared REST.CreateInput: the
// part of the request that is not a column.
type CreateChildInput struct {
	Pin string `json:"pin"`
}

// ChildCreate is the body: the writable columns, plus the declared property.
type ChildCreate struct {
	Name string `json:"name"`
	Pin  string `json:"pin"`
}

func (c ChildCreate) Row() (*Child, error) { return &Child{Name: c.Name}, nil }

// Input satisfies rest.CreateInput, which is the whole of the opt-in: a body
// that does not implement it changes nothing.
func (c ChildCreate) Input() any { return CreateChildInput{Pin: c.Pin} }

// PlainChildCreate is the same resource's body without the declared half — the
// shape every existing generated body has.
type PlainChildCreate struct {
	Name string `json:"name"`
}

func (c PlainChildCreate) Row() (*Child, error) { return &Child{Name: c.Name}, nil }

func childCols() []string { return []string{"id", "name", "pin_hash"} }

func childRow(id, name, hash string) []any { return []any{id, name, hash} }

// hashPin stands in for bcrypt: the point is that the stored value is derived
// from the sent one and is not it.
func hashPin(pin string) string { return "hashed:" + pin }

func mountChildren[C rest.CreateBody[Child]](t *testing.T, db sqlb.Executor) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Child, C, rest.None[Child]](api, db, rest.Options{
		Path: "/children", Name: "child", Ops: rest.OpCreate | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api
}

func TestCreateInputReachesTheHook(t *testing.T) {
	hooks := sqlb.NewRegistry()
	sqlb.On[Child](hooks).BeforeCreate(func(ctx context.Context, row *Child) error {
		in, ok := sqlb.CreateInputFrom[CreateChildInput](ctx)
		if !ok {
			return errors.New("children: a child is created with a PIN")
		}
		row.PinHash = hashPin(in.Pin)
		return nil
	})

	fake := newFakeDB(t, reply{cols: childCols(), rows: [][]any{childRow("c1", "Lena", hashPin("4242"))}})
	db := sqlb.New(fake.db).WithHooks(hooks)

	api := mountChildren[ChildCreate](t, db)
	resp := api.Post("/children", map[string]any{"name": "Lena", "pin": "4242"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	stmt := fake.lastStatement()
	if !strings.Contains(strings.SplitN(stmt, "VALUES", 2)[0], `"pin_hash"`) {
		t.Errorf("the hook's column did not reach the insert:\n%s", stmt)
	}
	var sawHash bool
	for _, arg := range fake.lastArgs() {
		switch arg {
		case "4242":
			t.Errorf("the plaintext PIN reached the statement: %v", fake.lastArgs())
		case hashPin("4242"):
			sawHash = true
		}
	}
	if !sawHash {
		t.Errorf("the derived value did not reach the statement: %v", fake.lastArgs())
	}
	// The property is an input. It is not a column, so it is in no response.
	if strings.Contains(resp.Body.String(), "4242") {
		t.Errorf("the response carries the input the request sent: %s", resp.Body)
	}
}

// The other direction, which is what makes the assertion above mean something:
// a body that does not declare an input leaves nothing in the context, so a
// hook reading one is told so rather than handed a stale or zero value.
//
// This is also the case a job or a seeder is in — the hook runs for every
// insert of the model, and only a request carries a body.
func TestCreateWithoutADeclaredInputLeavesTheContextEmpty(t *testing.T) {
	hooks := sqlb.NewRegistry()
	sqlb.On[Child](hooks).BeforeCreate(func(ctx context.Context, row *Child) error {
		if _, ok := sqlb.CreateInputFrom[CreateChildInput](ctx); ok {
			return errors.New("a body that declares nothing put something in the context")
		}
		return huma.Error422UnprocessableEntity("children: a child is created with a PIN")
	})

	fake := newFakeDB(t, reply{cols: childCols(), rows: [][]any{childRow("c1", "Lena", "")}})
	db := sqlb.New(fake.db).WithHooks(hooks)

	api := mountChildren[PlainChildCreate](t, db)
	resp := api.Post("/children", map[string]any{"name": "Lena"})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), "put something in the context") {
		t.Errorf("the context carried an input no body declared: %s", resp.Body)
	}
}

// A hook asking for the wrong type is told no, rather than handed something it
// then has to type-check itself. Same rule sqlb.PrincipalFrom follows, and for
// the same reason: the two failure modes have one answer.
func TestCreateInputIsTypedFromTheHooksSide(t *testing.T) {
	type otherInput struct{ Pin string }

	hooks := sqlb.NewRegistry()
	sqlb.On[Child](hooks).BeforeCreate(func(ctx context.Context, row *Child) error {
		if _, ok := sqlb.CreateInputFrom[otherInput](ctx); ok {
			return errors.New("a differently typed input was handed over")
		}
		in, ok := sqlb.CreateInputFrom[CreateChildInput](ctx)
		if !ok {
			return errors.New("the declared input did not arrive")
		}
		row.PinHash = hashPin(in.Pin)
		return nil
	})

	fake := newFakeDB(t, reply{cols: childCols(), rows: [][]any{childRow("c1", "Lena", hashPin("4242"))}})
	db := sqlb.New(fake.db).WithHooks(hooks)

	api := mountChildren[ChildCreate](t, db)
	if resp := api.Post("/children", map[string]any{"name": "Lena", "pin": "4242"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
}
