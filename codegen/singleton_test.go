package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// The shape a tenant-keyed table had no spelling for: one row per caller, so
// the collection envelope is wrong and the {id} segment asks the client for a
// value the server already holds (#166).
//
// The fixture is deliberately the reported one — a subscription keyed by the
// org that owns it — and the org column is both the primary key and the scope,
// which is what made OpRead's path parameter a restatement of the hook.
func singletonFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("billing_subscriptions",
		schema.UUIDv7("org_id").PrimaryKey().ReadOnly().Scoped(),
		schema.Enum("plan", "free", "pro", "team").Default(schema.Value("free")),
		schema.Timestamp("renews_at").Nullable(),
	).Expose(schema.REST{
		Path: "/billing-subscription",
		Ops:  schema.OpSingleton | schema.OpUpdate | schema.OpDelete,
	})
	return r
}

// keylessSingletonFixture is the same shape without a surrogate key at all,
// which is the capability the op unlocks: nothing addresses the row by id, so
// there is no id to declare.
func keylessSingletonFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("tenant_settings",
		schema.Text("org_id").ReadOnly().Scoped(),
		schema.Text("theme").Default(schema.Value("light")),
	).Expose(schema.REST{
		Path: "/settings",
		Ops:  schema.OpSingleton | schema.OpUpdate,
	})
	return r
}

// The declaration reaches the mount as the op it was written as.
func TestSingletonReachesTheGeneratedMount(t *testing.T) {
	got := generate(t, singletonFixture())["rest_gen.go"]
	if !strings.Contains(got, "rest.OpSingleton") {
		t.Errorf("the generated mount does not carry the op:\n%s", got)
	}
}

// A singleton's manifest describes the requests it serves and no others. The
// capability lists come from column declarations, and a resource with no list
// cannot answer any of them — reporting them would document a request that
// 400s, which is the failure #143 was.
func TestSingletonManifestOffersNoCollectionVocabulary(t *testing.T) {
	m := singletonFixture().BuildManifest()
	var rest *schema.RESTManifest
	for _, tbl := range m.Tables {
		if tbl.Name == "billing_subscriptions" {
			rest = tbl.REST
		}
	}
	if rest == nil {
		t.Fatal("the exposed table is missing from the manifest")
	}
	if !containsString(rest.Operations, "singleton") {
		t.Errorf("operations = %v, want singleton among them", rest.Operations)
	}
	if len(rest.Filterable) != 0 || len(rest.Sortable) != 0 || len(rest.Searchable) != 0 {
		t.Errorf("a singleton reported a collection vocabulary: filterable=%v sortable=%v searchable=%v",
			rest.Filterable, rest.Sortable, rest.Searchable)
	}
	if len(rest.Examples) != 1 || rest.Examples[0] != "GET /billing-subscription" {
		t.Errorf("examples = %v, want the one request this resource serves", rest.Examples)
	}
}

// The skill is what an agent reads before composing a request, and the two
// mistakes it can make here — asking for /{id} and asking for a filter — cost a
// round trip each.
func TestSingletonSkillSaysThereIsNoID(t *testing.T) {
	got, _, _ := renderSkillInto(t, singletonFixture(), "./billingschema")
	for _, want := range []string{
		"the caller's own row",
		"There is no `{id}`",
		"`PATCH /billing-subscription` writes the columns the body names.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the skill does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/billing-subscription/{id}") {
		t.Errorf("the skill offered an item path a singleton does not serve:\n%s", got)
	}
}

// Every client drops the id, because there is nothing for a caller to pass. A
// client that kept the parameter would be asking its user for the tenant id the
// transport's credential already carries.
func TestSingletonClientsTakeNoID(t *testing.T) {
	ts := generateTS(t, singletonFixture())["client.gen.ts"]
	for _, want := range []string{
		"export function getBillingSubscription(\n  request: Transport,\n",
		"path: '/billing-subscription', query: encodeItemQuery(params)",
		"export function updateBillingSubscription(request: Transport, body: BillingSubscriptionPatch",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("the TypeScript client does not contain %q", want)
		}
	}
	if strings.Contains(ts, "BillingSubscriptionListParams") {
		t.Error("the TypeScript client offered list parameters for a resource with no list")
	}

	dart := generateDart(t, singletonFixture())
	if !strings.Contains(dart, "Future<BillingSubscription> getBillingSubscription(\n  Transport request, {") {
		t.Errorf("the Dart client still takes an id:\n%s", dart)
	}

	cli := generateCLI(t, singletonFixture())
	for _, want := range []string{`Use:   "get"`, `Use:   "update"`, `Use:   "delete"`, "cobra.NoArgs"} {
		if !strings.Contains(cli, want) {
			t.Errorf("the CLI does not contain %q", want)
		}
	}
	if strings.Contains(cli, `client.ItemPath("/billing-subscription"`) {
		t.Error("the CLI built an item path for a resource that has none")
	}
}

// The mutation layer drops the id too, which is the seam where the two features
// meet: `mutationOptions` was written against resources addressed by key, and a
// variables object carrying an id would name an argument the transport function
// does not take (ADR-0028, ADR-0052).
func TestSingletonMutationsTakeNoID(t *testing.T) {
	queries := generateTS(t, singletonFixture())["queries.gen.ts"]
	for _, want := range []string{
		// The body alone, not { id, body }.
		"update: mutationOptions({ mutationFn: (body: BillingSubscriptionPatch) => updateBillingSubscription(request, body), }),",
		// And nothing at all, so mutate() takes no variables.
		"delete: mutationOptions({ mutationFn: () => deleteBillingSubscription(request), }),",
	} {
		if !contains(queries, want) {
			t.Errorf("queries missing %q:\n%s", want, queries)
		}
	}
	if strings.Contains(queries, "id: string | number") {
		t.Errorf("a singleton's options ask for an id no function takes:\n%s", queries)
	}

	// A singleton still reads, so the read half has to survive the same
	// predicate change — a factory keyed on OpRead alone would emit nothing.
	if !strings.Contains(queries, "export function billingSubscriptionQueries(") {
		t.Errorf("a singleton lost its read options:\n%s", queries)
	}
}

// The exit carries the shape too, and its statements are the mount's: no key
// predicate, and the confining conditions as the whole address.
func TestEjectCarriesTheSingleton(t *testing.T) {
	files := eject(t, singletonFixture())
	handlers := files["handlers.go"]
	for _, want := range []string{
		`mux.HandleFunc("GET /billing-subscription"`,
		`mux.HandleFunc("PATCH /billing-subscription"`,
		`mux.HandleFunc("DELETE /billing-subscription"`,
	} {
		if !strings.Contains(handlers, want) {
			t.Errorf("the ejected handlers do not register %q", want)
		}
	}
	if strings.Contains(handlers, "/billing-subscription/{id}") {
		t.Errorf("the exit registered an item route a singleton does not serve:\n%s", handlers)
	}
	store := files["store.go"]
	if !strings.Contains(store, "func GetBillingSubscription(ctx context.Context, db DB, where []Condition)") {
		t.Errorf("the ejected read still takes an id:\n%s", store)
	}
	if !strings.Contains(store, "writeWhere(&sb, args, where)") {
		t.Errorf("the ejected read did not address the row by its conditions alone:\n%s", store)
	}
}

// The exit has to compile, and nothing else in this suite compiles it. The
// singleton templates are hand-written net/http and hand-written SQL assembly,
// which is exactly the shape where a missing parameter is valid Go source that
// fails at the consumer's build (see TestGeneratedGoCompiles).
func TestEjectedSingletonCompiles(t *testing.T) {
	ejectCompiles(t, map[string]*schema.Registry{
		"singleton":        singletonFixture(),
		"keylesssingleton": keylessSingletonFixture(),
	})
}

// ejectCompiles ejects each registry into a scratch package inside this module
// and hands the lot to the Go compiler, for the reason compiles() does it for
// the generated side.
func ejectCompiles(t *testing.T, cases map[string]*schema.Registry) {
	t.Helper()

	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH, so ejected code cannot be compiled: %v", err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp(root, gencheckPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })

	pkgs := make([]string, 0, len(cases))
	for name, reg := range cases {
		if _, err := codegen.Eject(codegen.EjectOptions{
			Registry: reg, Dir: filepath.Join(scratch, name), Package: "ejectcheck",
		}); err != nil {
			t.Fatalf("%s: Eject: %v", name, err)
		}
		pkgs = append(pkgs, "./"+filepath.Base(scratch)+"/"+name)
	}

	build := exec.Command(gotool, append([]string{"build"}, pkgs...)...)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Errorf("ejected code does not compile: %v\n%s", err, out)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
