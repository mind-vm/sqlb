package spaces

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/fxapp/access"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// Directory maps a verified slug to the space id the hooks put in a WHERE
// clause.
//
// It is built once, at boot, and never written to again — which is only sound
// because the set of spaces *is* the configuration: access.Config names them,
// and this constructor creates any that are missing. An application where
// tenants are created at runtime resolves per request and caches instead, and
// the difference shows up in exactly one method (Current).
type Directory struct {
	ids map[string]string
}

// NewDirectory provisions the configured spaces and records their ids.
//
// Provisioning at boot rather than exposing POST /spaces is a decision the
// schema already recorded: Space is exposed for read and list only. A space
// nobody holds a key for is unreachable, and a space anybody may create is not
// a boundary — so the two halves, the row and the key, are made in the same
// place.
func NewDirectory(unscoped fxkit.Unscoped, cfg access.Config, log *slog.Logger) (*Directory, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := &Directory{ids: make(map[string]string, len(cfg.Keys))}
	for _, slug := range cfg.Slugs() {
		// OnConflictDoNothing rather than "does this slug exist?" first: the
		// check-then-insert version is a race between two instances starting
		// at once, and the unique index is the only thing that actually
		// decides. A conflict returns no rows, which is how the already-there
		// case is detected without reading a driver-specific SQLSTATE.
		created, err := sqlb.InsertRows(&store.Space{Name: slug, Slug: slug}).
			OnConflictDoNothing("slug").
			Exec(ctx, unscoped)
		if err != nil {
			return nil, fmt.Errorf("spaces: provisioning %q: %w", slug, err)
		}

		if len(created) == 1 {
			dir.ids[slug] = created[0].ID
			log.Info("spaces: created", "slug", slug, "id", created[0].ID)
			continue
		}

		existing, err := sqlb.Query[store.Space]().
			Where(store.SpaceCols.Slug.Eq(slug)).
			One(ctx, unscoped)
		if err != nil {
			return nil, fmt.Errorf("spaces: reading %q: %w", slug, err)
		}
		dir.ids[slug] = existing.ID
	}

	log.Info("spaces: directory ready", "spaces", cfg.Slugs())
	return dir, nil
}

// ID reports the id of a configured space.
func (d *Directory) ID(slug string) (string, bool) {
	id, ok := d.ids[slug]
	return id, ok
}

// Current is what every hook calls: the id of the space this request speaks
// for, or an error.
//
// It never falls back to "no restriction", which is the shape most tenancy
// bugs take. The worst case here is an endpoint that refuses — which somebody
// notices — rather than a list endpoint that quietly returns every tenant's
// rows with a 200 next to it.
//
// The error carries a status because a hook's error travels all the way out to
// the response: huma writes an error's own status when it has one and 500
// otherwise, and a missing identity is not an outage. Over HTTP this should be
// unreachable — the access middleware refuses first — which is the point of
// having it: the hook is the check that still applies to a background job, a
// test, or a surface written next year.
func (d *Directory) Current(ctx context.Context) (string, error) {
	slug, ok := access.SpaceFrom(ctx)
	if !ok {
		return "", huma.Error401Unauthorized("the request names no space")
	}
	id, ok := d.ids[slug]
	if !ok {
		// Reachable only if the configuration changed under a running
		// process, which it cannot here — the middleware verifies against the
		// same map this was built from.
		return "", huma.Error401Unauthorized("the space " + slug + " is not configured")
	}
	return id, nil
}
