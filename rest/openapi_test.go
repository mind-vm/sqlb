package rest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// paramsOf indexes a list operation's query parameters by name.
func paramsOf(t *testing.T, api huma.API, path string) map[string]*huma.Param {
	t.Helper()
	item := api.OpenAPI().Paths[path]
	if item == nil || item.Get == nil {
		t.Fatalf("no GET operation documented at %s", path)
	}
	out := map[string]*huma.Param{}
	for _, p := range item.Get.Parameters {
		out[p.Name] = p
	}
	return out
}

// The claim ADR-0007 doubted: a compositional filter grammar can be described
// precisely, by enumerating one parameter per filterable column rather than
// trying to express the grammar itself.
func TestListDocumentsOneParameterPerFilterableColumn(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	params := paramsOf(t, api, "/posts")

	for _, name := range []string{"id", "org_id", "title", "status", "view_count"} {
		if params[name] == nil {
			t.Errorf("filterable column %s has no documented parameter", name)
		}
	}
	// body declares only search, and search implies filter, so it does get a
	// parameter. excerpt declares nothing and so gets none: capabilities are
	// opt-in, and the document says exactly which columns opted in.
	if params["body"] == nil {
		t.Error("a searchable column is filterable too and should be a parameter")
	}
	if params["excerpt"] != nil {
		t.Error("excerpt declares no capability and should not be a parameter")
	}
	// The hidden column must not appear anywhere in the document.
	if params["secret"] != nil {
		t.Error("hidden column documented as a parameter")
	}
}

func TestFilterParameterDocumentsItsOperators(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	params := paramsOf(t, api, "/posts")

	text := params["title"].Description
	for _, op := range []string{"eq", "in", "contains", "startswith"} {
		if !strings.Contains(text, op) {
			t.Errorf("title's description omits the %s operator: %s", op, text)
		}
	}
	// The pattern operators need a text column, so an integer must not offer
	// them — documenting a request that parsing rejects is worse than silence.
	number := params["view_count"].Description
	for _, op := range []string{"contains", "startswith", "ilike"} {
		if strings.Contains(number, op) {
			t.Errorf("view_count's description offers the text-only %s operator: %s", op, number)
		}
	}

	// An array column takes containment and nothing else. Offering `between`
	// or `contains` here would document a request the parser refuses — and
	// `contains` in particular would suggest it means containment, which is
	// the one thing it must not be read as.
	array := params["tags"].Description
	for _, op := range []string{"has", "hasany", "hasall"} {
		if !strings.Contains(array, op) {
			t.Errorf("tags's description omits the %s operator: %s", op, array)
		}
	}
	for _, op := range []string{"between", "contains", "gte", "nin"} {
		if strings.Contains(array, op) {
			t.Errorf("tags's description offers %s, which an array column refuses: %s", op, array)
		}
	}
}

func TestSortParameterEnumeratesSortableColumnsInBothDirections(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	params := paramsOf(t, api, "/posts")

	sort := params["sort"]
	if sort == nil || sort.Schema == nil || sort.Schema.Items == nil {
		t.Fatal("sort is not documented as an array")
	}
	got := map[string]bool{}
	for _, v := range sort.Schema.Items.Enum {
		name, _ := v.(string)
		got[name] = true
	}
	for _, want := range []string{"title", "-title", "status", "-status", "view_count", "-view_count"} {
		if !got[want] {
			t.Errorf("sort enum missing %q: %v", want, sort.Schema.Items.Enum)
		}
	}
	// excerpt is not sortable, in either direction.
	if got["excerpt"] || got["-excerpt"] {
		t.Errorf("sort enum offers a column that is not sortable: %v", sort.Schema.Items.Enum)
	}
}

func TestSelectEnumeratesOnlyVisibleColumns(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())

	sel := paramsOf(t, api, "/posts")["select"]
	if sel == nil || sel.Schema == nil || sel.Schema.Items == nil {
		t.Fatal("select is not documented as an array")
	}
	for _, v := range sel.Schema.Items.Enum {
		if v == "secret" {
			t.Error("select enum discloses the hidden column")
		}
	}
}

func TestPerPageDocumentsTheResourceCeiling(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())

	perPage := paramsOf(t, api, "/posts")["per_page"]
	if perPage == nil {
		t.Fatal("per_page is not documented")
	}
	if perPage.Schema.Default != 2 {
		t.Errorf("per_page default = %v, want the resource's 2", perPage.Schema.Default)
	}
	if !strings.Contains(perPage.Description, "10") {
		t.Errorf("per_page should document the ceiling of 10: %s", perPage.Description)
	}
}

func TestErrorResponsesCarryTheAllowedField(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())

	resp := api.OpenAPI().Paths["/posts"].Get.Responses["400"]
	if resp == nil {
		t.Fatal("the list operation does not document a 400")
	}
	media := resp.Content["application/problem+json"]
	if media == nil || media.Schema == nil {
		t.Fatal("the 400 has no problem+json schema")
	}
	schema := media.Schema
	if schema.Ref != "" {
		schema = api.OpenAPI().Components.Schemas.SchemaFromRef(schema.Ref)
	}
	detail := schema.Properties["errors"]
	if detail == nil || detail.Items == nil {
		t.Fatal("the error schema has no errors array")
	}
	items := detail.Items
	if items.Ref != "" {
		items = api.OpenAPI().Components.Schemas.SchemaFromRef(items.Ref)
	}
	// ADR-0011's substance: the allow-list is a structured field, not prose a
	// client would have to parse out of the message.
	if items.Properties["allowed"] == nil {
		t.Errorf("the error detail schema has no allowed field: %v", keys(items.Properties))
	}
}

func TestDocumentIsSerialisable(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())

	doc, err := api.OpenAPI().YAML()
	if err != nil {
		t.Fatalf("rendering the document: %v", err)
	}
	if len(doc) == 0 {
		t.Fatal("the document is empty")
	}
	// A hidden column must not survive anywhere in the document, including in
	// the response schema derived from the model struct.
	if strings.Contains(string(doc), "secret") {
		t.Error("the OpenAPI document mentions a hidden column")
	}

	if _, err := json.Marshal(api.OpenAPI()); err != nil {
		t.Fatalf("marshalling the document as JSON: %v", err)
	}
}

func TestOperationsAreRegisteredForTheDeclaredOpsOnly(t *testing.T) {
	db := newFakeDB(t)
	opts := postOptions()
	opts.Ops = rest.OpList
	api := mount(t, db.db, opts)

	if api.OpenAPI().Paths["/posts"].Get == nil {
		t.Error("list was not registered")
	}
	if item := api.OpenAPI().Paths["/posts/{id}"]; item != nil {
		t.Error("single-row operations were registered for a list-only resource")
	}
	if api.OpenAPI().Paths["/posts"].Post != nil {
		t.Error("create was registered for a list-only resource")
	}
}

func keys(m map[string]*huma.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Cursor paging is documented, because a client that cannot discover it from
// the document will keep using ?page= on a table where that gets slower.
func TestListDocumentsTheCursorParameter(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	params := paramsOf(t, api, "/posts")

	cursor := params["cursor"]
	if cursor == nil {
		t.Fatal("no cursor parameter documented")
	}
	if !strings.Contains(cursor.Description, "next_cursor") {
		t.Errorf("description does not say where a cursor comes from: %s", cursor.Description)
	}

	// The other half of the contract: the response says where the next cursor
	// comes from, or a client has no way to obtain the first one.
	media := api.OpenAPI().Paths["/posts"].Get.Responses["200"].Content["application/json"]
	if media == nil || media.Schema == nil {
		t.Fatal("the list operation documents no 200 body")
	}
	body := media.Schema
	if body.Ref != "" {
		body = api.OpenAPI().Components.Schemas.SchemaFromRef(body.Ref)
	}
	if body.Properties["next_cursor"] == nil {
		t.Errorf("the list response does not document next_cursor: %v", keys(body.Properties))
	}
}

// A model with no primary key has no tiebreaker, so cursor paging cannot work
// for it. It is withheld from the document rather than offered and refused —
// the failure mode ADR-0025 records for ?expand, and the same answer.
func TestKeylessResourceDocumentsNoCursor(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Ledger, rest.None[Ledger], rest.None[Ledger]](api, db.db, rest.Options{
		Path: "/ledger",
		Name: "entry",
		Ops:  rest.OpList,
	})
	if err != nil {
		t.Fatalf("mounting a keyless list resource: %v", err)
	}

	if params := paramsOf(t, api, "/ledger"); params["cursor"] != nil {
		t.Error("a keyless resource should not offer a cursor it cannot honour")
	}
	if params := paramsOf(t, api, "/ledger"); params["page"] == nil {
		t.Error("offset paging should still be offered")
	}
}

// Security documents; it does not enforce. What this pins is that it reaches
// *every* operation — the one that would be easy to miss is delete, whose
// operation literal is aligned differently from the other four.
func TestSecurityReachesEveryOperation(t *testing.T) {
	db := newFakeDB(t)
	opts := postOptions()
	opts.Security = []map[string][]string{{"bearerAuth": {}}}
	api := mount(t, db.db, opts)

	doc := api.OpenAPI()
	type opRef struct {
		name string
		op   *huma.Operation
	}
	item := doc.Paths["/posts/{id}"]
	coll := doc.Paths["/posts"]
	if item == nil || coll == nil {
		t.Fatal("the resource did not mount both paths")
	}
	for _, ref := range []opRef{
		{"list", coll.Get},
		{"create", coll.Post},
		{"read", item.Get},
		{"update", item.Patch},
		{"delete", item.Delete},
	} {
		if ref.op == nil {
			t.Errorf("%s is not documented", ref.name)
			continue
		}
		if len(ref.op.Security) != 1 || len(ref.op.Security[0]["bearerAuth"]) != 0 {
			t.Errorf("%s carries Security %v, want the resource's requirement", ref.name, ref.op.Security)
		}
	}
}

// The default is nothing, because a requirement sqlb invented would document an
// auth scheme the deployment may not have.
func TestSecurityIsAbsentUnlessAsked(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	if op := api.OpenAPI().Paths["/posts"].Get; op.Security != nil {
		t.Errorf("an unconfigured resource documented Security %v", op.Security)
	}
}

// Every group parameter the parser reads must also be advertised, or a caller
// reading the document cannot discover a shape the server accepts. `not` was
// added to the grammar in #98 and this is the check that it did not stop at
// the parser.
func TestListDocumentsEveryGroupParameter(t *testing.T) {
	db := newFakeDB(t)
	api := mount(t, db.db, postOptions())
	params := paramsOf(t, api, "/posts")

	for _, name := range []string{"or", "and", "not"} {
		p := params[name]
		if p == nil {
			t.Errorf("group parameter %q is not documented", name)
			continue
		}
		if p.In != "query" {
			t.Errorf("%s: In = %q, want query", name, p.In)
		}
		// Groups conjoin, so each is a repeatable parameter rather than a
		// single value — the document has to say so or a client generator
		// will emit the wrong type.
		if p.Schema == nil || p.Schema.Type != "array" {
			t.Errorf("%s: schema should be an array, so repeats are expressible", name)
		}
	}
	if d := params["not"].Description; !strings.Contains(d, "NOT (a AND b)") {
		t.Errorf("not: description should state how several conditions read, got %q", d)
	}
}

// mountOneToOneUser registers OneToOneUser, whose Profile field is the bare
// pointer a unique-FK-backed Inverse codegens: a one-to-one reverse relation,
// alongside Tasks, an ordinary capped-collection reverse relation.
func mountOneToOneUser(t *testing.T, db sqlb.Executor) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[OneToOneUser, rest.None[OneToOneUser], rest.None[OneToOneUser]](api, db, rest.Options{
		Path: "/one-to-one-users", Name: "one_to_one_user", Ops: rest.OpRead | rest.OpList,
		Expandable: []string{"profile", "tasks"},
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api
}

// The server sends `"profile": null` when a unique-FK-backed Inverse's target
// is absent (proven end to end by example/tasks/app's
// TestExpandOfAOneToOneRelationIsNullWhenAbsent) — so the document has to say
// null is a legal value for that property, not just the bare $ref a plain
// struct pointer would otherwise get. Without this, a strict OpenAPI
// validator or a generator built from the document would reject a real
// response.
func TestOneToOneExpandFieldSchemaAdmitsNull(t *testing.T) {
	db := newFakeDB(t)
	api := mountOneToOneUser(t, db.db)

	schema := api.OpenAPI().Components.Schemas.Map()["OneToOneUser"]
	if schema == nil {
		t.Fatal("no OneToOneUser schema in the document")
	}
	profile := schema.Properties["profile"]
	if profile == nil {
		t.Fatal("no profile property documented")
	}
	if profile.Ref != "" {
		t.Fatalf("profile is a bare $ref (%s), so a validator would reject the null the server actually sends", profile.Ref)
	}
	if len(profile.AnyOf) != 2 {
		t.Fatalf("profile should be anyOf [$ref, null], got %+v", profile)
	}
	var sawRef, sawNull bool
	for _, alt := range profile.AnyOf {
		switch {
		case alt.Ref != "":
			sawRef = true
		case alt.Type == "null":
			sawNull = true
		}
	}
	if !sawRef {
		t.Errorf("profile's anyOf has no $ref to OneToOneProfile: %+v", profile.AnyOf)
	}
	if !sawNull {
		t.Errorf("profile's anyOf has no null branch: %+v", profile.AnyOf)
	}

	// Guard-proven-both-ways: an ordinary capped collection must not be swept
	// up by the same widening — it already reports absence as an empty items
	// list, never null, so its schema stays exactly what a plain relation gets.
	tasks := schema.Properties["tasks"]
	if tasks == nil {
		t.Fatal("no tasks property documented")
	}
	if tasks.Ref == "" {
		t.Errorf("tasks (a capped collection) should still be a bare $ref, got %+v", tasks)
	}
}
