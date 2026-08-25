package app_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// adminClient mints a PlatformAdmin token the way cmd/mint-admin does — with
// the server's own secret, out of band from register/login — and returns a
// client holding it. workspace is a real one an ordinary account already
// created; the value is required by Signer.Sign but unread by the "tenant"
// hooks the admin mount releases, which is the whole point being tested here.
func adminClient(t *testing.T, h http.Handler, workspace string) *client {
	t.Helper()
	signer, err := auth.NewSigner(secret, "tasks", time.Hour)
	if err != nil {
		t.Fatalf("building the admin signer: %v", err)
	}
	token, err := signer.Sign(auth.Claims{
		Subject:       "platform-admin-1",
		Workspace:     workspace,
		Role:          auth.RoleAdmin,
		PlatformAdmin: true,
	})
	if err != nil {
		t.Fatalf("signing the admin token: %v", err)
	}
	return &client{t: t, h: h, token: token}
}

// TestAdminRoutesRefuseAnOrdinaryToken is RequireAdmin's half of the
// boundary: valid claims, wrong claims.
func TestAdminRoutesRefuseAnOrdinaryToken(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	alice.listID("Backlog") // just to have something an admin route could leak

	alice.get("/admin/tasks").expect(http.StatusForbidden)
	alice.get("/admin/workspaces").expect(http.StatusForbidden)
	alice.patch("/admin/tasks/00000000-0000-0000-0000-000000000001",
		map[string]any{"title": "x"}).expect(http.StatusForbidden)
}

// TestAdminSeesEveryWorkspace is the claim the whole feature exists to make:
// a token bearing PlatformAdmin, hitting /admin/tasks, sees rows that belong
// to a workspace the token itself does not name.
func TestAdminSeesEveryWorkspace(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	bob := account(t, server, "bob@example.com", "Globex")

	aliceList := alice.listID("Acme work")
	alice.taskID(aliceList, "Acme secret", nil)
	bobList := bob.listID("Globex work")
	bobTask := bob.taskID(bobList, "Globex secret", nil)

	// Minted against Alice's workspace — irrelevant to what it can see, which
	// is exactly the assertion: the claim's Workspace does not gate /admin/*.
	admin := adminClient(t, server, "Acme")

	all := admin.get("/admin/tasks").expect(http.StatusOK).list()
	if len(all.Items) != 2 {
		t.Fatalf("admin saw %d tasks, want 2 (one per workspace): %v", len(all.Items), titles(all.Items))
	}

	got := admin.get("/admin/tasks/" + bobTask).expect(http.StatusOK).item()
	if got["title"] != "Globex secret" {
		t.Errorf("admin could not read a specific row in another workspace: %v", got)
	}

	spaces := admin.get("/admin/workspaces").expect(http.StatusOK).list()
	if len(spaces.Items) != 2 {
		t.Errorf("admin saw %d workspaces, want 2: %v", len(spaces.Items), spaces.Items)
	}
}

// TestAdminCanEditAcrossWorkspaces checks the write half — scopeWrites'
// "tenant" release, not just scopeReads'.
func TestAdminCanEditAcrossWorkspaces(t *testing.T) {
	server := newServer(t, freshDB(t))
	bob := account(t, server, "bob@example.com", "Globex")

	bobList := bob.listID("Globex work")
	bobTask := bob.taskID(bobList, "Globex secret", nil)

	admin := adminClient(t, server, "Acme") // Alice's workspace, editing Bob's row
	admin.patch("/admin/tasks/"+bobTask, map[string]any{"title": "corrected by ops"}).
		expect(http.StatusOK)

	after := bob.get("/tasks/" + bobTask).expect(http.StatusOK).item()
	if after["title"] != "corrected by ops" {
		t.Errorf("admin's edit did not persist: %v", after)
	}
}

// TestAdminStillHonoursSoftDelete checks the split hooks.go's comment argues
// for: "tenant" is released, the soft-delete predicate is a separate hook and
// is not.
func TestAdminStillHonoursSoftDelete(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	list := alice.listID("Backlog")
	task := alice.taskID(list, "Will be deleted", nil)
	alice.delete("/tasks/" + task).expect(http.StatusNoContent)

	admin := adminClient(t, server, "Acme")
	admin.get("/admin/tasks/" + task).expect(http.StatusNotFound)

	all := admin.get("/admin/tasks").expect(http.StatusOK).list()
	for _, item := range all.Items {
		if item["id"] == task {
			t.Fatalf("a soft-deleted task is still visible through /admin/tasks: %v", item)
		}
	}
}

// TestOrdinaryRoutesStayScopedForAnAdminToken is the other direction: the
// released scope belongs to the /admin/* mount's handle, not to the claim.
// A platform-admin token hitting the ordinary /tasks still only sees its own
// workspace — the boundary is which route you took, not who you are.
func TestOrdinaryRoutesStayScopedForAnAdminToken(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	bob := account(t, server, "bob@example.com", "Globex")

	aliceList := alice.listID("Acme work")
	alice.taskID(aliceList, "Acme secret", nil)
	bobList := bob.listID("Globex work")
	bob.taskID(bobList, "Globex secret", nil)

	// Real UUID, not the workspace name: this is the one test where the
	// admin token also hits an ordinary route, and there the "tenant" hook
	// is still active and actually filters by it.
	aliceWorkspace := alice.get("/workspaces").expect(http.StatusOK).list().Items[0]["id"].(string)

	admin := adminClient(t, server, aliceWorkspace)
	got := admin.get("/tasks").expect(http.StatusOK).list()
	if len(got.Items) != 1 || got.Items[0]["title"] != "Acme secret" {
		t.Errorf("an admin token on the ordinary route saw more than its own workspace: %v", titles(got.Items))
	}
}
