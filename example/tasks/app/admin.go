package app

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/rest"
)

// registerAdminRoutes mounts a second resource per multi-tenant table at
// /admin/{table}, over the same hooked handle rest_gen.go's Register uses,
// but with the "tenant" scope released (app/hooks.go) — every workspace's
// rows rather than the caller's own one.
//
// This is the hand-written half ADR-0050 describes: Expose stays singular
// per table, so this mount is not schema-declared. It does not appear in
// tasks/sqlb.json, carries no generated TypeScript/Dart client, and is not
// on the drift gate — a schema edit here would need this file updated by
// hand, the same as any other hand-written endpoint. What it does get for
// free is everything a second rest.Resource call over the generated model
// gets: the typed column facade, filter/sort/search validated the same way,
// and the same row[T] response shape every other endpoint uses.
//
// The route guard is auth.RequireAdmin("/admin/"), wired in New below —
// releasing a scope only changes what rows a query can see, never who may
// ask, and ADR-0053's revision named exactly this as the open, undesigned
// question a real cross-tenant view would raise. This is the answer: an
// out-of-band-minted token (cmd/mint-admin) carrying PlatformAdmin, checked
// at the route, over rows a released hook stops filtering.
//
// No create anywhere here, on purpose. BeforeCreate is not releasable
// (ADR-0054 — "there is nothing for a reader to be released from") because
// it stamps WorkspaceID from the caller's own claims, and a platform-admin
// token's Workspace names no workspace the new row should belong to. An
// admin creating a row in a specific tenant still does it through that
// tenant's own token, the same as anyone else.
func registerAdminRoutes(api huma.API, hooked *sqlb.DB) error {
	if err := rest.Resource[tasks.List, rest.None[tasks.List], tasks.ListPatch](api, hooked, rest.Options{
		Path:            "/admin/lists",
		Name:            "admin-list",
		Tag:             "admin",
		Ops:             rest.OpRead | rest.OpUpdate | rest.OpList,
		Description:     "Every workspace's lists, for a platform-admin token.",
		DefaultPageSize: 25,
		MaxPageSize:     100,
		Unscoped:        []string{"tenant"},
	}); err != nil {
		return err
	}
	if err := rest.Resource[tasks.Task, rest.None[tasks.Task], tasks.TaskPatch](api, hooked, rest.Options{
		Path:            "/admin/tasks",
		Name:            "admin-task",
		Tag:             "admin",
		Ops:             rest.OpRead | rest.OpUpdate | rest.OpList,
		Description:     "Every workspace's tasks, for a platform-admin token.",
		DefaultPageSize: 20,
		MaxPageSize:     100,
		MaxFilters:      12,
		Unscoped:        []string{"tenant"},
	}); err != nil {
		return err
	}
	if err := rest.Resource[tasks.Comment, rest.None[tasks.Comment], rest.None[tasks.Comment]](api, hooked, rest.Options{
		Path:            "/admin/comments",
		Name:            "admin-comment",
		Tag:             "admin",
		Ops:             rest.OpRead | rest.OpList,
		Description:     "Every workspace's comments, for a platform-admin token.",
		DefaultPageSize: 50,
		MaxPageSize:     100,
		Unscoped:        []string{"tenant"},
	}); err != nil {
		return err
	}
	if err := rest.Resource[tasks.Membership, rest.None[tasks.Membership], rest.None[tasks.Membership]](api, hooked, rest.Options{
		Path:            "/admin/memberships",
		Name:            "admin-membership",
		Tag:             "admin",
		Ops:             rest.OpRead | rest.OpDelete | rest.OpList,
		Description:     "Every workspace's memberships, for a platform-admin token.",
		DefaultPageSize: 25,
		MaxPageSize:     100,
		Unscoped:        []string{"tenant"},
	}); err != nil {
		return err
	}
	if err := rest.Resource[tasks.User, rest.None[tasks.User], rest.None[tasks.User]](api, hooked, rest.Options{
		Path:        "/admin/users",
		Name:        "admin-user",
		Tag:         "admin",
		Ops:         rest.OpRead | rest.OpList,
		Description: "Every user in the installation, for a platform-admin token.",
		MaxPageSize: 100,
		Unscoped:    []string{"tenant"},
	}); err != nil {
		return err
	}
	if err := rest.Resource[tasks.Workspace, rest.None[tasks.Workspace], rest.None[tasks.Workspace]](api, hooked, rest.Options{
		Path:        "/admin/workspaces",
		Name:        "admin-workspace",
		Tag:         "admin",
		Ops:         rest.OpRead | rest.OpList,
		Description: "Every tenant in the installation, for a platform-admin token.",
		Unscoped:    []string{"tenant"},
	}); err != nil {
		return err
	}
	return nil
}
