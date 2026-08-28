package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// A declared read in the TypeScript client (#316).
//
// The asymmetry this closes was measurable: a declared action reached all four
// toolchains and a declared query reached the Go mount and the docs checklist,
// so the one property a declared route is for did not hold for reads. The last
// test in this file is the one the issue was really about — Reads had been
// validated, carried into rest.QuerySpec, rendered into the OpenAPI
// description and read by nothing, because there was no transport function to
// key.

// tsQueryFixture is two resources and two declared reads: one answering rows
// of its own table and reading a second one, the other declaring its result.
func tsQueryFixture() *schema.Registry {
	r := schema.NewRegistry()
	list := r.Table("lists",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("list", list).Filterable(),
		schema.Text("title").Sortable(),
	).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddQuery(schema.Query{
			Name: "overdue",
			Params: schema.Body(
				schema.Timestamp("as_of").Comment("Tasks due before this."),
				schema.UUID("list_id").Nullable(),
			),
			// The read joins lists, so a list changing changes its answer.
			Reads: []*schema.TableDef{list},
		}).
		AddQuery(schema.Query{
			Name: "by-list",
			Returns: schema.Result(
				schema.UUID("list_id"),
				schema.BigInt("open"),
			),
		})
	return r
}

func TestTSDeclaredReadReachesTheClient(t *testing.T) {
	src := generateTS(t, tsQueryFixture())["client.gen.ts"]

	for _, want := range []string{
		// The parameters are a type alias rather than an interface, because
		// encodeQueryParams takes a Record and only an alias gets an implicit
		// index signature. An interface here compiles everywhere but the one
		// call site below.
		"export type OverdueTasksParams = {",
		"  as_of: string;",
		"  list_id?: string | null;",
		// A read that declared its result answers with that shape, not with
		// rows of the table it hangs off.
		"export interface ByListTasksResult {",
		"export function byListTasks(request: Transport, params: ByListTasksParams = {}, signal?: AbortSignal): Promise<ByListTasksResult[]> {",
		// A read that declared none answers with the table's rows.
		"export function overdueTasks(request: Transport, params: OverdueTasksParams, signal?: AbortSignal): Promise<Task[]> {",
		"return request({ method: 'GET', path: '/tasks/overdue', query: encodeQueryParams(params), signal });",
	} {
		if !contains(src, want) {
			t.Errorf("client missing %q:\n%s", want, src)
		}
	}
}

// A required parameter takes the default away, so the call cannot omit it —
// the same rule the Go signature follows by making an optional property a
// pointer.
func TestTSDeclaredReadWithARequiredParameterTakesOne(t *testing.T) {
	src := generateTS(t, tsQueryFixture())["client.gen.ts"]
	if contains(src, "params: OverdueTasksParams = {}") {
		t.Errorf("as_of is required, so params must not default to {}:\n%s", src)
	}
}

func TestTSDeclaredReadReachesTheTanStackFactory(t *testing.T) {
	src := generateTS(t, tsQueryFixture())["queries.gen.ts"]

	for _, want := range []string{
		// Beside list and detail, in the resource's own factory rather than
		// hand-written next to it.
		"    overdue: (params: OverdueTasksParams) =>",
		"        queryKey: taskKeys.query('overdue', params),",
		"        queryFn: ({ signal }) => overdueTasks(request, params, signal),",
		// A hyphenated name is one identifier on the object and stays the
		// declared spelling in the key, which is what the change feed matches.
		"    byList: (params: ByListTasksParams = {}) =>",
		"        queryKey: taskKeys.query('by-list', params),",
	} {
		if !contains(src, want) {
			t.Errorf("queries file missing %q:\n%s", want, src)
		}
	}
}

// The point of the whole emitter. A change to lists invalidates the read
// declared on tasks that says it reads lists — which is the behaviour
// schema.Query.Reads has described in the present tense since it existed and
// nothing performed.
func TestAChangeInvalidatesTheDeclaredReadsThatSayTheyReadIt(t *testing.T) {
	src := generateTS(t, tsQueryFixture())["client.gen.ts"]

	for _, want := range []string{
		// Under the declaring table's namespace, so one factory answers for
		// every read on that resource.
		"  query: (name: string, params: unknown = {}) => ['tasks', 'query', name, params] as const,",
		// lists is a foreign table to this read, so its key is listed on both
		// branches: taskKeys is not under listKeys.all().
		"      ? [listKeys.all(), taskKeys.query('overdue')]",
		"      : [listKeys.lists(), listKeys.infinites(), listKeys.detail(key), taskKeys.query('overdue')],",
		// On the declaring table the keyless branch needs nothing added,
		// because all() already covers the whole namespace the keys sit in.
		"      ? [taskKeys.all()]",
		"      : [taskKeys.lists(), taskKeys.infinites(), taskKeys.detail(key), taskKeys.query('overdue'), taskKeys.query('by-list')],",
	} {
		if !contains(src, want) {
			t.Errorf("change feed missing %q:\n%s", want, src)
		}
	}

	// lists has no query key factory of its own: it declares no reads, and a
	// read declared elsewhere keys under the table that declared it.
	if contains(src, "['lists', 'query'") {
		t.Errorf("lists declares no reads, so it should have no query key factory:\n%s", src)
	}
}

// A Reads that names a table this client does not serve invalidates nothing,
// because the subscriber drops every event for a table absent from
// keysByTable. The emitter says so in the file rather than leaving it to be
// found by a chart that never refreshes.
func TestAReadOfAnUnservedTableIsStatedRatherThanSilent(t *testing.T) {
	r := schema.NewRegistry()
	audit := r.Table("audit_log",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("note"),
	)
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
	).
		Expose(schema.REST{Ops: schema.OpList}).
		AddQuery(schema.Query{Name: "recent", Reads: []*schema.TableDef{audit}})

	src := generateTS(t, r)["client.gen.ts"]
	want := "Not invalidated: audit_log (read by tasks.recent)"
	if !contains(src, want) {
		t.Errorf("the gap is not stated in the client:\n%s", src)
	}
}

// The collision the schema validator cannot see: "detail" is how this file
// spells the shape of a generated read rather than the name of a route, so
// nothing upstream refuses it and an object literal with a duplicate key would
// silently lose one of them.
func TestADeclaredReadMayNotTakeAReadFactorysName(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	).
		Expose(schema.REST{Ops: schema.OpRead | schema.OpList}).
		AddQuery(schema.Query{Name: "detail"})

	_, err := codegen.Generate(codegen.Options{
		Registry: r, Dir: t.TempDir(), Package: "gen", TSDir: "web/api",
	})
	if err == nil {
		t.Fatal("a query named detail was accepted, and would have overwritten the read factory's own entry")
	}
	for _, want := range []string{`query "detail"`, "taskQueries(request).detail", "Name the read for what it answers with"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say %q:\n%v", want, err)
		}
	}
}
