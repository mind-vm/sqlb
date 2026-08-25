package fxapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/mind-vm/sqlb/example/fxapp"
	"github.com/mind-vm/sqlb/example/fxapp/access"
	"github.com/mind-vm/sqlb/example/fxapp/spaces"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// Two spaces and their keys. Long enough to satisfy the length the access
// module insists on, which is itself asserted in access/config_test.go.
const (
	acmeKey   = "acme-key-0123456789abcdef"
	globexKey = "globex-key-abcdef0123456789"
	keys      = "acme=" + acmeKey + ",globex=" + globexKey
)

// TestGraphIsValid checks the wiring without building any of it.
//
// fx.ValidateApp resolves every dependency — including the value groups and
// the two named handles — and constructs nothing, so this needs no database
// and costs milliseconds. What it catches is the class of mistake a container
// introduces and a compiler cannot see: a constructor asking for a type nobody
// provides, a `group:"hooks"` misspelled at one end, a cycle.
func TestGraphIsValid(t *testing.T) {
	if err := fx.ValidateApp(fxapp.Modules()); err != nil {
		t.Fatalf("the module graph does not resolve: %v", err)
	}
}

// TestBootProvisionsSpaces asserts the startup sequence the container derives:
// connect, migrate, provision, and answer.
//
// The database it is given is empty, so a passing run is also a run of the
// checked-in migration history.
func TestBootProvisionsSpaces(t *testing.T) {
	srv := boot(t, freshDatabase(t))

	var page struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	srv.get(t, "/spaces", acmeKey, http.StatusOK, &page)

	// One, not two: the caller holds acme's key, and the hook in
	// spaces/hooks.go narrows the primary key to the space that key names.
	// Both rows exist — the second boot below sees globex's — so this is the
	// boundary, not an empty table.
	if len(page.Items) != 1 || page.Items[0].Slug != "acme" {
		t.Fatalf("GET /spaces as acme returned %+v; want exactly the acme space", page.Items)
	}

	var globex struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	srv.get(t, "/spaces", globexKey, http.StatusOK, &globex)
	if len(globex.Items) != 1 || globex.Items[0].Slug != "globex" {
		t.Fatalf("GET /spaces as globex returned %+v; want exactly the globex space", globex.Items)
	}
}

// TestProvisioningIsIdempotent boots twice against one database.
//
// The second boot must find the spaces rather than fail on the unique index —
// which is the property OnConflictDoNothing buys, and the one that decides
// whether this application can be restarted.
func TestProvisioningIsIdempotent(t *testing.T) {
	dsn := freshDatabase(t)

	first := boot(t, dsn)
	var before struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	first.get(t, "/spaces", acmeKey, http.StatusOK, &before)
	first.stop(t)

	second := boot(t, dsn)
	var after struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	second.get(t, "/spaces", acmeKey, http.StatusOK, &after)

	if len(before.Items) != 1 || len(after.Items) != 1 {
		t.Fatalf("expected one space on each boot; got %d then %d", len(before.Items), len(after.Items))
	}
	// The same row, not a second one with the same slug — which the unique
	// index would refuse anyway, and which is the point of asserting the id
	// rather than the count.
	if before.Items[0].ID != after.Items[0].ID {
		t.Fatalf("the second boot provisioned a new space: %s then %s", before.Items[0].ID, after.Items[0].ID)
	}
}

// TestSpaceBoundaryHolds is the claim the hooks exist for, asserted from the
// outside: two callers, one database, and no endpoint that mentions a space.
func TestSpaceBoundaryHolds(t *testing.T) {
	srv := boot(t, freshDatabase(t))

	var created struct {
		ID      string `json:"id"`
		SpaceID string `json:"space_id"`
		Title   string `json:"title"`
	}
	srv.post(t, "/notes", acmeKey, map[string]any{
		"title":  "Quarterly plan",
		"body":   "Ship the thing.",
		"status": "published",
	}, http.StatusCreated, &created)

	// The create body has no space_id field — the column is ReadOnly in the
	// schema — so the value here came from the BeforeCreate hook, which read
	// it from the verified key.
	if created.SpaceID == "" {
		t.Fatal("the created note has no space_id; the BeforeCreate hook did not stamp it")
	}

	t.Run("a list from another space is empty", func(t *testing.T) {
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		srv.get(t, "/notes", globexKey, http.StatusOK, &page)
		if len(page.Items) != 0 {
			t.Fatalf("globex sees %d of acme's notes", len(page.Items))
		}
	})

	t.Run("reading another space's note by id is a 404", func(t *testing.T) {
		// 404 rather than 403: the predicate is added to the query, so the row
		// is not found — which is also the answer an id that never existed
		// gets, and therefore says nothing about what exists elsewhere.
		srv.get(t, "/notes/"+created.ID, globexKey, http.StatusNotFound, nil)
	})

	t.Run("patching another space's note is a 404", func(t *testing.T) {
		srv.patch(t, "/notes/"+created.ID, globexKey,
			map[string]any{"title": "Mine now"}, http.StatusNotFound, nil)
	})

	t.Run("deleting another space's note is a 404", func(t *testing.T) {
		srv.do(t, http.MethodDelete, "/notes/"+created.ID, globexKey, nil, http.StatusNotFound, nil)
	})

	t.Run("the owner can still read and patch it", func(t *testing.T) {
		// The other half of the guard. Without this, every assertion above
		// would also pass against a server that refused everything.
		var patched struct {
			Title string `json:"title"`
		}
		srv.patch(t, "/notes/"+created.ID, acmeKey,
			map[string]any{"title": "Quarterly plan v2"}, http.StatusOK, &patched)
		if patched.Title != "Quarterly plan v2" {
			t.Fatalf("PATCH as the owner returned title %q", patched.Title)
		}
	})

	t.Run("aggregates are scoped too", func(t *testing.T) {
		// The hand-written endpoint, and the reason it is in the example: its
		// query names no space, and a GROUP BY that leaked across tenants
		// would leak quietly — counts of rows rather than rows.
		var mine struct {
			ByStatus []struct {
				Status string `json:"status"`
				Count  int64  `json:"count"`
			} `json:"by_status"`
		}
		srv.get(t, "/insights/notes", acmeKey, http.StatusOK, &mine)
		if len(mine.ByStatus) != 1 || mine.ByStatus[0].Status != "published" || mine.ByStatus[0].Count != 1 {
			t.Fatalf("acme's breakdown is %+v; want one published note", mine.ByStatus)
		}

		var theirs struct {
			ByStatus []struct {
				Status string `json:"status"`
			} `json:"by_status"`
		}
		srv.get(t, "/insights/notes", globexKey, http.StatusOK, &theirs)
		if len(theirs.ByStatus) != 0 {
			t.Fatalf("globex's breakdown is %+v; want nothing", theirs.ByStatus)
		}
	})
}

// TestAccessIsRequired covers the middleware's two answers over the real
// router, since the list of public paths is a property of the running server
// rather than of the function that reads it.
func TestAccessIsRequired(t *testing.T) {
	srv := boot(t, freshDatabase(t))

	srv.do(t, http.MethodGet, "/notes", "", nil, http.StatusUnauthorized, nil)
	srv.do(t, http.MethodGet, "/notes", "not-a-key", nil, http.StatusUnauthorized, nil)

	// The exceptions, which must answer without a key or the liveness probe
	// and the docs are unusable.
	srv.do(t, http.MethodGet, "/health", "", nil, http.StatusOK, nil)
	srv.do(t, http.MethodGet, "/openapi.json", "", nil, http.StatusOK, nil)
}

// TestResourcesRefuseToMountWithoutHooks is the guard, proven in the direction
// that matters.
//
// Every other test in this file boots the full module list and passes. This
// one removes notes.Module — the only contributor of hooks for store.Note —
// and requires the boot to fail. sqlb refuses to mount a resource whose schema
// declares a Scoped column with no registration behind it (ADR-0030), the
// generated Register returns that refusal, fxkit's OperationSet carries it
// out, and fx reports it instead of listening.
//
// Without this assertion, "the container makes the ordering structural" would
// be a claim about a program nobody had run in its broken form.
func TestResourcesRefuseToMountWithoutHooks(t *testing.T) {
	t.Setenv("FXAPP_DATABASE_URL", freshDatabase(t))
	t.Setenv("FXAPP_SPACE_KEYS", keys)
	t.Setenv("FXAPP_ADDR", "127.0.0.1:0")
	t.Setenv("FXAPP_LOG_LEVEL", "error")

	// The same list fxapp.Modules() returns, minus notes.Module. Written out
	// rather than subtracted, because an fx.Options bundle is opaque — which
	// is itself worth knowing before organising an application this way.
	app := fx.New(
		fxapp.Platform(),
		store.Module,
		access.Module,
		spaces.Module,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := app.Start(ctx)
	if err == nil {
		_ = app.Stop(ctx)
		t.Fatal("the server started with no hooks for store.Note; the declared scope was not enforced")
	}
	// Checked against the message rather than merely against failure, so that
	// the test cannot pass because of an unrelated boot error — a typo in a
	// group tag would also fail Start, and would not mean what this asserts.
	// The message names the model, the column, and each missing registration:
	//
	//	rest: /notes exposes create|read|update|delete|list, and nothing confines store.Note
	//	  create: BeforeCreate is not registered (space_id is Scoped)
	//	  ...
	for _, want := range []string{"store.Note", "space_id is Scoped", "BeforeQuery is not registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the boot failed for the wrong reason (no %q in the message): %v", want, err)
		}
	}
}

// --- the harness ------------------------------------------------------------

type server struct {
	base    string
	app     *fx.App
	stopped bool
}

// boot starts the whole application against dsn and returns its base URL.
//
// It builds exactly what cmd/server builds — fxapp.Modules() — for the reason
// the package comment gives: with a container, the assembly is the program, so
// a test that assembled its own would be testing something else.
func boot(t *testing.T, dsn string) *server {
	t.Helper()

	t.Setenv("FXAPP_DATABASE_URL", dsn)
	t.Setenv("FXAPP_SPACE_KEYS", keys)
	// Port zero: the OS picks, and the kit reports what it got, so two tests
	// running at once cannot collide on 8080.
	t.Setenv("FXAPP_ADDR", "127.0.0.1:0")
	t.Setenv("FXAPP_LOG_LEVEL", "warn")

	var srv *http.Server
	app := fx.New(fxapp.Modules(), fx.Populate(&srv))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("starting the application: %v", err)
	}

	s := &server{base: "http://" + srv.Addr, app: app}
	t.Cleanup(func() { s.stop(t) })
	return s
}

// stop shuts the application down. Idempotent, because a test that stops it
// explicitly — to boot a second one over the same database — is also
// registered for cleanup.
func (s *server) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.app.Stop(ctx); err != nil {
		t.Errorf("stopping the application: %v", err)
	}
}

func (s *server) get(t *testing.T, path, key string, want int, out any) {
	t.Helper()
	s.do(t, http.MethodGet, path, key, nil, want, out)
}

func (s *server) post(t *testing.T, path, key string, body any, want int, out any) {
	t.Helper()
	s.do(t, http.MethodPost, path, key, body, want, out)
}

func (s *server) patch(t *testing.T, path, key string, body any, want int, out any) {
	t.Helper()
	s.do(t, http.MethodPatch, path, key, body, want, out)
}

func (s *server) do(t *testing.T, method, path, key string, body any, want int, out any) {
	t.Helper()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, s.base+path, payload)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the response: %v", method, path, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d\n%s", method, path, resp.StatusCode, want, read)
	}
	if out != nil {
		if err := json.Unmarshal(read, out); err != nil {
			t.Fatalf("%s %s: decoding %s: %v", method, path, read, err)
		}
	}
}
