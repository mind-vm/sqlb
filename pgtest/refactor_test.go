package pgtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/example/blog"
	"github.com/mind-vm/sqlb/example/withsqlc"
	"github.com/mind-vm/sqlb/example/withsqlc/sqlcgen"
	"github.com/mind-vm/sqlb/schema"

	// Imported for its side effects: declaring a table registers it.
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
)

// The claim docs/refactoring-from-sqlc.md rests on: the four stages are four
// spellings of one endpoint, and a project part-way through the refactoring is
// serving the same rows as it was before.
//
// example/withsqlc/refactor_test.go asserts what each stage *sends*, which is
// all a stub can answer honestly. This is the half that needs a real planner:
// four implementations, one database, one set of rows, and Postgres deciding
// whether they agree.
//
// It is worth having both. The stub tests would still pass if every stage sent
// SQL that was individually well-formed and collectively meant different
// things.

// The fixture. Every post gets a distinct published_at so that ORDER BY
// published_at DESC is a total order and the four stages cannot disagree merely
// about ties.
type fixture struct {
	pool *pgxpool.Pool
	ids  map[string]string // title → id
}

func refactorFixture(t *testing.T) fixture {
	t.Helper()

	pool := freshDB(t)
	applySchema(t, pool, schema.DefaultRegistry())
	ctx := context.Background()

	orgs := map[string]string{}
	for _, name := range []string{"acme", "globex"} {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO orgs (name, slug) VALUES ($1, $1) RETURNING id`, name,
		).Scan(&id); err != nil {
			t.Fatalf("inserting org %s: %v", name, err)
		}
		orgs[name] = id
	}

	authors := map[string]string{}
	for name, orgID := range orgs {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO authors (org_id, email, name, password_hash)
			 VALUES ($1, $2, $3, 'x') RETURNING id`,
			orgID, name+"@example.com", name,
		).Scan(&id); err != nil {
			t.Fatalf("inserting author for %s: %v", name, err)
		}
		authors[name] = id
	}

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	posts := []struct {
		org       string
		title     string
		body      string
		status    string
		views     int64
		published time.Time
		deleted   bool
	}{
		// Newest first, so the expectations below read in ORDER BY order.
		{"acme", "Ada writes", "on the analytical engine", "published", 500, base.Add(-1 * time.Hour), false},
		{"acme", "Bob drafts", "something unfinished", "draft", 50, base.Add(-2 * time.Hour), false},
		{"acme", "Ada again", "a second note", "published", 150, base.Add(-3 * time.Hour), false},
		// Soft-deleted: no stage should ever return it.
		{"acme", "Ada retracted", "withdrawn", "published", 900, base.Add(-4 * time.Hour), true},
		// Another tenant, newest of all and highest view count, so a stage that
		// lost its scope predicate sorts it to the top of every scenario.
		{"globex", "Ada elsewhere", "another tenant entirely", "published", 1000, base, false},
	}

	f := fixture{pool: pool, ids: map[string]string{}}
	for _, p := range posts {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO posts (org_id, author_id, title, body, status, view_count, published_at, deleted_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			orgs[p.org], authors[p.org], p.title, p.body, p.status, p.views, p.published,
			deletedAt(p.deleted, base),
		).Scan(&id); err != nil {
			t.Fatalf("inserting post %q: %v", p.title, err)
		}
		f.ids[p.title] = id
	}
	return f
}

func deletedAt(deleted bool, at time.Time) any {
	if deleted {
		return at
	}
	return nil
}

// titles turns a stage's rows back into the titles the expectations are written
// in, because an id assigned by the database is not something a test can spell.
func (f fixture) titles(ids []string) []string {
	byID := map[string]string{}
	for title, id := range f.ids {
		byID[id] = title
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if title, ok := byID[id]; ok {
			out[i] = title
		} else {
			out[i] = "unknown:" + id
		}
	}
	return out
}

// The scenarios. Each is one request in two spellings: the ad-hoc parameters
// stages 1 and 2 invented, and the filter grammar stages 3 and 4 serve. The
// document says the wire format changes at stage 3 and the rows do not, and
// this is where that is checked rather than asserted.
var refactorScenarios = []struct {
	name    string
	adhoc   url.Values // stages 1 and 2
	grammar url.Values // stages 3 and 4
	want    []string
}{
	{
		name:    "no filters",
		adhoc:   url.Values{},
		grammar: url.Values{"sort": {"-published_at"}},
		want:    []string{"Ada writes", "Bob drafts", "Ada again"},
	},
	{
		name:    "one filter",
		adhoc:   url.Values{"status": {"published"}},
		grammar: url.Values{"status": {"eq.published"}, "sort": {"-published_at"}},
		want:    []string{"Ada writes", "Ada again"},
	},
	{
		name:    "a numeric comparison",
		adhoc:   url.Values{"min_views": {"100"}},
		grammar: url.Values{"view_count": {"gte.100"}, "sort": {"-published_at"}},
		want:    []string{"Ada writes", "Ada again"},
	},
	{
		name:    "two filters at once",
		adhoc:   url.Values{"status": {"published"}, "min_views": {"200"}},
		grammar: url.Values{"status": {"eq.published"}, "view_count": {"gte.200"}, "sort": {"-published_at"}},
		want:    []string{"Ada writes"},
	},
	{
		name:    "a page smaller than the result set",
		adhoc:   url.Values{"limit": {"2"}},
		grammar: url.Values{"per_page": {"2"}, "sort": {"-published_at"}},
		want:    []string{"Ada writes", "Bob drafts"},
	},
	{
		// The sort stage 1 can serve, which is the only one it has. The stages
		// that take it as a value are covered in the stub tests; here it is the
		// case where all four can be compared.
		name:    "the ordering stage 1 was generated for",
		adhoc:   url.Values{"sort": {"-published_at"}},
		grammar: url.Values{"sort": {"-published_at"}},
		want:    []string{"Ada writes", "Bob drafts", "Ada again"},
	},
}

func TestEveryStageReturnsTheSameRows(t *testing.T) {
	t.Parallel()
	f := refactorFixture(t)
	orgID := f.orgOf(t, "Ada writes")

	// Stage 4's hook lives in a registry its own handle carries, so it reaches
	// stage 4's requests and nothing else in this package. It used to land in a
	// process-wide registry that every other test querying blog.Post then
	// inherited, which is why this needed putting back afterwards (ADR-0047).
	server, err := withsqlc.ServerStage4(f.pool)
	if err != nil {
		t.Fatalf("ServerStage4: %v", err)
	}

	// The org goes on the context for every stage, not just stage 4's requests:
	// stage 4 reads it in its hook, and the earlier stages take it as the
	// argument they were written to take.
	//
	// Stage 4's hook constrains every read through the handle that carries it,
	// which is stage 4's alone. Stage 3 queries the pool directly and is scoped
	// by the predicate it applies by hand. Both arrive at the same rows, which
	// is what this test compares — the difference is that stage 4 cannot forget
	// and stage 3 can.
	ctx := withsqlc.WithOrg(context.Background(), orgID)

	for _, sc := range refactorScenarios {
		t.Run(sc.name, func(t *testing.T) {
			stages := map[string][]string{}

			one, err := withsqlc.ListPostsStage1(ctx, f.pool, orgID, sc.adhoc)
			if err != nil {
				t.Fatalf("stage 1: %v", err)
			}
			stages["stage 1"] = idsOf(one, func(p sqlcgen.Post) string { return p.ID })

			two, err := withsqlc.ListPostsStage2(ctx, f.pool, orgID, sc.adhoc)
			if err != nil {
				t.Fatalf("stage 2: %v", err)
			}
			stages["stage 2"] = idsOf(two, func(p sqlcgen.Post) string { return p.ID })

			three, err := withsqlc.ListPostsStage3(ctx, f.pool, orgID, sc.grammar)
			if err != nil {
				t.Fatalf("stage 3: %v", err)
			}
			stages["stage 3"] = idsOf(three, func(p blog.Post) string { return p.ID })

			stages["stage 4"] = f.viaHTTP(t, server, orgID, sc.grammar)

			for name, ids := range stages {
				got := f.titles(ids)
				if !equal(got, sc.want) {
					t.Errorf("%s returned %v, want %v", name, got, sc.want)
				}
			}
		})
	}
}

// The one place the four stages genuinely differ, asserted rather than left for
// someone to discover in production.
//
// Stages 1 and 2 search the title, because that is what the SQL and the branch
// say. Stage 3 searches every column the schema declared Searchable, which for
// posts is title *and* body — so `?search=` widens at stage 3, and a row whose
// body matches starts appearing. That is the declaration doing what it says,
// not a bug, but it is a behaviour change and a release note.
func TestSearchIsTheOneBehaviourThatChanges(t *testing.T) {
	t.Parallel()
	f := refactorFixture(t)
	orgID := f.orgOf(t, "Ada writes")
	ctx := withsqlc.WithOrg(context.Background(), orgID)

	// "unfinished" appears in one post's body and in no post's title.
	two, err := withsqlc.ListPostsStage2(ctx, f.pool, orgID, url.Values{"search": {"unfinished"}})
	if err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if len(two) != 0 {
		t.Errorf("stage 2 searched more than the title: %v", f.titles(idsOf(two, func(p sqlcgen.Post) string { return p.ID })))
	}

	three, err := withsqlc.ListPostsStage3(ctx, f.pool, orgID, url.Values{"search": {"unfinished"}})
	if err != nil {
		t.Fatalf("stage 3: %v", err)
	}
	got := f.titles(idsOf(three, func(p blog.Post) string { return p.ID }))
	if !equal(got, []string{"Bob drafts"}) {
		t.Errorf("stage 3 returned %v, want [Bob drafts] — body declared Searchable", got)
	}
}

// orgOf reads the tenant a fixture post belongs to, since the ids are the
// database's to choose.
func (f fixture) orgOf(t *testing.T, title string) string {
	t.Helper()
	var orgID string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT org_id FROM posts WHERE id = $1`, f.ids[title],
	).Scan(&orgID); err != nil {
		t.Fatalf("reading the org of %q: %v", title, err)
	}
	return orgID
}

// viaHTTP drives stage 4 the way a client would, and returns the ids in the
// order the envelope listed them.
func (f fixture) viaHTTP(t *testing.T, server http.Handler, orgID string, query url.Values) []string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/posts?"+query.Encode(), nil)
	req = req.WithContext(withsqlc.WithOrg(req.Context(), orgID))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stage 4: status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("stage 4: decoding %s: %v", rec.Body, err)
	}
	ids := make([]string, len(body.Items))
	for i, item := range body.Items {
		ids[i] = item.ID
	}
	return ids
}

func idsOf[T any](rows []T, id func(T) string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = id(row)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
