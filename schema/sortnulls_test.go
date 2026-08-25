package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The placement rides on the `sort` token rather than becoming a token of its
// own. A separate token could be written without `sort`, and a placement
// without a sort key is a declaration with no meaning (#88).
func TestSortablePlacementRidesOnTheSortCapability(t *testing.T) {
	tests := []struct {
		name  string
		field *schema.Field
		want  string
	}{
		{"no placement", schema.Timestamp("at").Sortable(), "sort"},
		{"nulls last", schema.Timestamp("at").Sortable(schema.NullsLast), "sort:nullslast"},
		{"nulls first", schema.Timestamp("at").Sortable(schema.NullsFirst), "sort:nullsfirst"},
		{"the explicit default is the default", schema.Timestamp("at").Sortable(schema.NullsDefault), "sort"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.field.Desc().Capabilities()
			for _, part := range strings.Split(caps, ",") {
				if strings.HasPrefix(part, "sort") {
					if part != tc.want {
						t.Errorf("Capabilities() sort token = %q, want %q (full: %q)", part, tc.want, caps)
					}
					return
				}
			}
			t.Errorf("Capabilities() = %q, carries no sort token", caps)
		})
	}
}

// A placement declared without Sortable is not reachable — Sortable is the only
// way to set one — but the tag writer is what a generated model is read back
// through, so the pairing is worth pinning rather than assuming.
func TestPlacementNeverAppearsWithoutTheSortKey(t *testing.T) {
	caps := schema.Timestamp("at").Filterable().Desc().Capabilities()
	if strings.Contains(caps, "nulls") {
		t.Errorf("a non-sortable column carries a placement: %q", caps)
	}
}

func TestSortableRefusesMoreThanOnePlacement(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Sortable accepted two placements")
		}
		if !strings.Contains(r.(string), "at most one") {
			t.Errorf("panic does not say what is wrong: %v", r)
		}
	}()
	schema.Timestamp("at").Sortable(schema.NullsFirst, schema.NullsLast)
}

func TestSortableRefusesAnUnknownPlacement(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Sortable accepted a placement that is not one")
		}
		if !strings.Contains(r.(string), "NullsFirst") {
			t.Errorf("panic does not name the spellings that work: %v", r)
		}
	}()
	schema.Timestamp("at").Sortable(schema.Nulls("middle"))
}

// The manifest carries the placement beside the capability list rather than in
// it, because a reader deciding what an endpoint returns needs to know the
// order and a client generator has nothing to do with it.
func TestManifestCarriesThePlacement(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Timestamp("published_at").Nullable().Sortable(schema.NullsLast),
		schema.Timestamp("created_at").Sortable(),
	).Expose(schema.REST{Path: "/posts", Ops: schema.OpList})

	m := r.BuildManifest()
	var found, plain bool
	for _, tbl := range m.Tables {
		for _, c := range tbl.Columns {
			switch c.Name {
			case "published_at":
				found = true
				if c.SortNulls != "last" {
					t.Errorf("published_at SortNulls = %q, want %q", c.SortNulls, "last")
				}
			case "created_at":
				plain = true
				if c.SortNulls != "" {
					t.Errorf("created_at declares no placement but the manifest carries %q", c.SortNulls)
				}
			}
		}
	}
	if !found || !plain {
		t.Fatalf("manifest did not carry both columns (found=%v plain=%v)", found, plain)
	}
}
