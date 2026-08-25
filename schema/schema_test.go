package schema_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

func TestTableDeclaration(t *testing.T) {
	r := schema.NewRegistry()
	org := r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	user := r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique().Searchable(),
		schema.Int("age").Nullable().Filterable().Sortable(),
		schema.Ref("org", org).OnDelete(schema.Cascade).Expandable(),
		schema.Timestamps(),
	).Index("email").Expose(schema.REST{Ops: schema.CRUD | schema.OpList})

	if err := r.Validate(); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}

	// Timestamps contributes two columns, so the group must be flattened.
	if got, want := len(user.Fields()), 6; got != want {
		t.Errorf("users has %d columns, want %d", got, want)
	}

	// Ref names the column after the relation and adopts the target's key type.
	ref := user.Field("org_id")
	if ref == nil {
		t.Fatal("Ref should have produced an org_id column")
	}
	if ref.Desc().Ref.Table != org {
		t.Error("org_id does not point at orgs")
	}
	if got := ref.Desc().Type; got != schema.TypeUUID {
		t.Errorf("org_id type = %s, want %s (adopted from the target key)", got, schema.TypeUUID)
	}
	if ref.Desc().Ref.OnDelete != schema.Cascade {
		t.Error("OnDelete was not recorded")
	}

	// Expose defaults the path from the table name.
	if got := user.Rest().Path; got != "/users" {
		t.Errorf("REST path = %q, want %q", got, "/users")
	}
}

func TestCapabilityTagRoundTrip(t *testing.T) {
	r := schema.NewRegistry()
	tbl := r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Searchable().Sortable(),
		schema.Text("secret").Hidden(),
	)

	// The tag body is what the runtime engine reads back off the generated
	// model, so it is the contract between the two halves of the system.
	if got, want := tbl.Field("id").Desc().Capabilities(), "pk,default,filter,readonly"; got != want {
		t.Errorf("id capabilities = %q, want %q", got, want)
	}
	// Searchable implies filterable, so ?title=... works on a searchable column.
	if got, want := tbl.Field("title").Desc().Capabilities(), "filter,sort,search"; got != want {
		t.Errorf("title capabilities = %q, want %q", got, want)
	}
	if got, want := tbl.Field("secret").Desc().Capabilities(), "hidden"; got != want {
		t.Errorf("secret capabilities = %q, want %q", got, want)
	}
}

func TestGoTypeMapping(t *testing.T) {
	r := schema.NewRegistry()
	tbl := r.Table("things",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Int("count"),
		schema.Int("maybe_count").Nullable(),
		schema.Timestamp("at"),
		schema.Bytes("blob").Nullable(),
		schema.JSON("doc"),
		schema.JSON("maybe_doc").Nullable(),
	)
	for _, tt := range []struct{ column, want string }{
		{"id", "string"},
		{"count", "int32"},
		{"maybe_count", "*int32"},
		{"at", "time.Time"},
		{"blob", "[]byte"}, // already nilable, so it is not wrapped in a pointer
		{"doc", "json.RawMessage"},
		// The pair above and below is the whole point of this case. Both types
		// are slices of bytes, and only bytea may skip the pointer: nil is what
		// a []byte is when it is absent. A document type is not that, and a
		// bare json.RawMessage was the one generated type that did not say its
		// column could be NULL.
		{"maybe_doc", "*json.RawMessage"},
	} {
		if got := tbl.Field(tt.column).Desc().GoType(); got != tt.want {
			t.Errorf("%s: Go type = %q, want %q", tt.column, got, tt.want)
		}
	}
}

// Validation is the schema author's feedback loop, so it reports every problem
// at once and each message has to name the fix.
func TestValidationCatchesAuthoringMistakes(t *testing.T) {
	tests := []struct {
		name  string
		build func(*schema.Registry)
		want  string
	}{
		{
			name: "two primary keys",
			build: func(r *schema.Registry) {
				r.Table("a", schema.UUIDv7("id").PrimaryKey(), schema.UUIDv7("other").PrimaryKey())
			},
			want: "expected at most one",
		},
		{
			name: "duplicate column",
			build: func(r *schema.Registry) {
				r.Table("b", schema.UUIDv7("id").PrimaryKey(), schema.Text("x"), schema.Int("x"))
			},
			want: "declared twice",
		},
		{
			name: "searchable non-text column",
			build: func(r *schema.Registry) {
				r.Table("c", schema.UUIDv7("id").PrimaryKey(), schema.Int("n").Searchable())
			},
			want: "Searchable requires a text column",
		},
		{
			name: "expandable non-reference",
			build: func(r *schema.Registry) {
				r.Table("d", schema.UUIDv7("id").PrimaryKey(), schema.Text("x").Expandable())
			},
			want: "only meaningful on a Ref",
		},
		{
			name: "hidden and filterable leaks through probing",
			build: func(r *schema.Registry) {
				r.Table("e", schema.UUIDv7("id").PrimaryKey(), schema.Text("pw").Hidden().Filterable())
			},
			want: "leaks its contents",
		},
		{
			name: "write-only and filterable leaks through probing",
			build: func(r *schema.Registry) {
				r.Table("e2", schema.UUIDv7("id").PrimaryKey(), schema.Text("answer").WriteOnly().Filterable())
			},
			want: "leaks its contents",
		},
		{
			name: "write-only and sortable leaks through ordering",
			build: func(r *schema.Registry) {
				r.Table("e3", schema.UUIDv7("id").PrimaryKey(), schema.Text("answer").WriteOnly().Sortable())
			},
			want: "leaks its contents",
		},
		{
			name: "write-only and hidden say the same thing about reads and disagree about writes",
			build: func(r *schema.Registry) {
				r.Table("e4", schema.UUIDv7("id").PrimaryKey(), schema.Text("answer").WriteOnly().Hidden())
			},
			want: "pick one",
		},
		{
			name: "write-only and read-only is never settable and only ever settable",
			build: func(r *schema.Registry) {
				r.Table("e5", schema.UUIDv7("id").PrimaryKey(), schema.Text("answer").WriteOnly().ReadOnly())
			},
			want: "never settable and only ever settable",
		},
		{
			name: "primary key cannot be write-only",
			build: func(r *schema.Registry) {
				r.Table("e6", schema.UUIDv7("id").PrimaryKey().WriteOnly())
			},
			want: "primary key cannot be WriteOnly",
		},
		{
			name: "index over an unknown column",
			build: func(r *schema.Registry) {
				r.Table("f", schema.UUIDv7("id").PrimaryKey()).Index("nonexistent")
			},
			want: "unknown column",
		},
		{
			name: "exposed for read without a key to address rows by",
			build: func(r *schema.Registry) {
				r.Table("g", schema.Text("x")).Expose(schema.REST{Ops: schema.OpRead})
			},
			want: "no primary key",
		},
		{
			name: "colliding REST paths",
			build: func(r *schema.Registry) {
				r.Table("h", schema.UUIDv7("id").PrimaryKey()).Expose(schema.REST{Path: "/same", Ops: schema.OpList})
				r.Table("i", schema.UUIDv7("id").PrimaryKey()).Expose(schema.REST{Path: "/same", Ops: schema.OpList})
			},
			want: "already used by table",
		},
		{
			name: "page size above the maximum",
			build: func(r *schema.Registry) {
				r.Table("j", schema.UUIDv7("id").PrimaryKey()).
					Expose(schema.REST{Ops: schema.OpList, DefaultPageSize: 500, MaxPageSize: 100})
			},
			want: "exceeds MaxPageSize",
		},
		{
			name: "scoped column a request could write",
			build: func(r *schema.Registry) {
				r.Table("s1", schema.UUIDv7("id").PrimaryKey(),
					schema.UUID("org_id").Filterable().Scoped())
			},
			want: "must be ReadOnly",
		},
		{
			name: "scoped column that may be NULL",
			build: func(r *schema.Registry) {
				r.Table("s2", schema.UUIDv7("id").PrimaryKey(),
					schema.UUID("org_id").Nullable().ReadOnly().Scoped())
			},
			want: "cannot be Nullable",
		},
		{
			name: "two scope columns",
			build: func(r *schema.Registry) {
				r.Table("s3", schema.UUIDv7("id").PrimaryKey(),
					schema.UUID("org_id").ReadOnly().Scoped(),
					schema.UUID("team_id").ReadOnly().Scoped())
			},
			want: "2 Scoped columns declared",
		},
		{
			name: "identifier that is not valid SQL",
			build: func(r *schema.Registry) {
				r.Table("k", schema.UUIDv7("id").PrimaryKey(), schema.Text("Bad Name"))
			},
			want: "not a valid SQL identifier",
		},
		{
			// A rename hint asserts that the old name is gone. If the old
			// column is still declared, either the hint is wrong or the two
			// columns are being swapped — which Postgres cannot do in one
			// statement either.
			name: "column renamed from one that still exists",
			build: func(r *schema.Registry) {
				r.Table("l",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("headline"),
					schema.Text("title").RenamedFrom("headline"),
				)
			},
			want: "still declared as a column of its own",
		},
		{
			name: "two columns renamed from the same one",
			build: func(r *schema.Registry) {
				r.Table("m",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("title").RenamedFrom("headline"),
					schema.Text("subtitle").RenamedFrom("headline"),
				)
			},
			want: "also claimed by column",
		},
		{
			name: "column renamed from itself",
			build: func(r *schema.Registry) {
				r.Table("n", schema.UUIDv7("id").PrimaryKey(), schema.Text("title").RenamedFrom("title"))
			},
			want: "RenamedFrom names the column itself",
		},
		{
			name: "table renamed from one that still exists",
			build: func(r *schema.Registry) {
				r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
				r.Table("organisations", schema.UUIDv7("id").PrimaryKey()).RenamedFrom("orgs")
			},
			want: "still declared as a table of its own",
		},
		{
			name: "two tables renamed from the same one",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey()).RenamedFrom("old")
				r.Table("q", schema.UUIDv7("id").PrimaryKey()).RenamedFrom("old")
			},
			want: "also claimed by table",
		},
		{
			name: "unique constraint over a column that does not exist",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey()).Unique("id", "nope")
			},
			want: `unique constraint "p_id_nope_key" references unknown column "nope"`,
		},
		{
			name: "unique constraint with no columns",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey()).UniqueNamed("p_empty_key")
			},
			want: "covers no columns",
		},
		{
			// The derived name concatenates the table and every column, so it
			// reaches 63 bytes without any single part looking long — and a
			// truncated name diffs as a rename forever.
			name: "derived unique constraint name past the identifier limit",
			build: func(r *schema.Registry) {
				r.Table("a_table_with_a_fairly_long_name",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("tenant_identifier"),
					schema.Text("resource_identifier"),
				).Unique("tenant_identifier", "resource_identifier")
			},
			want: "so give it a shorter name with UniqueNamed",
		},
		{
			// Non-positive means "take the package default" everywhere a
			// ceiling is read, so a negative one reads as a tighter bound and
			// behaves as the loosest available.
			name: "a negative cost ceiling",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey()).
					Expose(schema.REST{Ops: schema.OpList, MaxOffset: -1})
			},
			want: "request ceilings must not be negative",
		},
		{
			// The ordering a request gets when it names none. A term nothing
			// can sort by would answer 400 to a client that sent nothing at
			// all, which is the one direction a default must not fail in.
			name: "a default ordering over an unknown column",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey()).
					Expose(schema.REST{Ops: schema.OpList, DefaultSort: []string{"-nope"}})
			},
			want: `DefaultSort "-nope" names no column of this table`,
		},
		{
			// Capabilities are opt-in, so an ordering nothing declared is one
			// no ?sort could have asked for either.
			name: "a default ordering over a column that is not Sortable",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey(), schema.Text("name")).
					Expose(schema.REST{Ops: schema.OpList, DefaultSort: []string{"name"}})
			},
			want: "names a column that is not Sortable",
		},
		{
			name: "a default ordering with an unreadable direction",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey(), schema.Text("name").Sortable()).
					Expose(schema.REST{Ops: schema.OpList, DefaultSort: []string{"name.sideways"}})
			},
			want: "is not a sort direction",
		},
		{
			// The refusal the whole singleton shape rests on. Its row comes
			// from the scope hook and from nothing else — no key in the path,
			// no predicate in the statement — so on an unconfined table the
			// read answers an arbitrary row and the PATCH reaches every row
			// there is (#166).
			name: "a singleton over a table nothing confines",
			build: func(r *schema.Registry) {
				r.Table("settings", schema.UUIDv7("id").PrimaryKey(), schema.Text("theme")).
					Expose(schema.REST{Ops: schema.OpSingleton})
			},
			want: "exposes OpSingleton but declares no Scoped column",
		},
		{
			// GET on the collection path cannot be the caller's row and the
			// collection at once.
			name: "a singleton beside a list",
			build: func(r *schema.Registry) {
				r.Table("settings", schema.UUIDv7("id").PrimaryKey().ReadOnly().Scoped()).
					Expose(schema.REST{Ops: schema.OpSingleton | schema.OpList})
			},
			want: "the same route",
		},
		{
			// A read by id is the question the shape exists to delete, so it
			// is named as a leftover rather than as a conflict.
			name: "a singleton beside a read by id",
			build: func(r *schema.Registry) {
				r.Table("settings", schema.UUIDv7("id").PrimaryKey().ReadOnly().Scoped()).
					Expose(schema.REST{Ops: schema.OpSingleton | schema.OpRead})
			},
			want: "drop OpRead",
		},
		{
			// Deferral is a property of a constraint, and a column with no
			// unique constraint has none of its own to defer.
			name: "Deferred without a unique constraint",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey(),
					schema.Int("position").Deferred())
			},
			want: "Deferred applies to a column's own unique constraint",
		},
		{
			// A typed string is open, and the alternative to refusing an
			// unknown value here is DDL Postgres rejects halfway through a
			// migration.
			name: "an unknown deferrability",
			build: func(r *schema.Registry) {
				r.Table("p", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug")).
					AddUnique(schema.Unique{Columns: []string{"slug"}, Deferrable: "maybe"})
			},
			want: `has an unknown Deferrable "maybe"`,
		},
		{
			name: "SharedAs on a non-enum column",
			build: func(r *schema.Registry) {
				r.Table("q", schema.UUIDv7("id").PrimaryKey(), schema.Text("x").SharedAs("X"))
			},
			want: "SharedAs is only meaningful on an Enum column",
		},
		{
			name: "SharedAs name is not an exported Go identifier",
			build: func(r *schema.Registry) {
				r.Table("q2", schema.UUIDv7("id").PrimaryKey(),
					schema.Enum("status", "draft", "published").SharedAs("status"))
			},
			want: "is not an exported Go identifier",
		},
		{
			// Two tables opting into the same shared type but drifting on what
			// it means — issue #197's whole reason for requiring the name to be
			// declared rather than inferred from matching values. The message
			// names both columns and both value sets, so the fix is legible
			// without opening either schema file.
			//
			// Registry.Tables() sorts by name, so "courses" is validated before
			// "lessons" regardless of the order they are declared in below, and
			// is the one the second declaration is reported against.
			name: "SharedAs value sets disagree",
			build: func(r *schema.Registry) {
				r.Table("lessons", schema.UUIDv7("id").PrimaryKey(),
					schema.Enum("status", "draft", "published", "archived").SharedAs("Status"))
				r.Table("courses", schema.UUIDv7("id").PrimaryKey(),
					schema.Enum("status", "draft", "published").SharedAs("Status"))
			},
			want: `SharedAs("Status") is also declared on courses.status, with a different value set: ` +
				`courses.status has ["draft", "published"], lessons.status has ["draft", "published", "archived"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := schema.NewRegistry()
			tt.build(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected validation to fail with %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// The counterpart to the "SharedAs value sets disagree" case above: two
// columns declaring the identical value set, in the identical order, under one
// SharedAs name validate cleanly. codegen is what turns this agreement into
// one Go type — this only checks that Validate has nothing to say about it.
func TestSharedAsAcceptsIdenticalValueSets(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons", schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").SharedAs("Status"))
	r.Table("courses", schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").SharedAs("Status"))

	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("multi",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Int("n").Searchable(),
		schema.Text("x").Expandable(),
	).Index("missing")

	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	for _, want := range []string{"Searchable requires", "only meaningful on a Ref", "unknown column"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestDuplicateTableNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("declaring the same table twice should panic at init rather than produce confusing DDL")
		}
	}()
	r := schema.NewRegistry()
	r.Table("dup", schema.UUIDv7("id").PrimaryKey())
	r.Table("dup", schema.UUIDv7("id").PrimaryKey())
}

func TestOnDeleteOnNonReferencePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("OnDelete on a plain column should panic")
		}
	}()
	schema.Text("x").OnDelete(schema.Cascade)
}

func TestExposedTablesAreListedSeparately(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("internal_audit", schema.UUIDv7("id").PrimaryKey())
	r.Table("public_docs", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.OpList})

	if got := len(r.Tables()); got != 2 {
		t.Errorf("registry holds %d tables, want 2", got)
	}
	exposed := r.Exposed()
	if len(exposed) != 1 || exposed[0].Name() != "public_docs" {
		t.Errorf("Exposed() = %v, want only public_docs: a table without Expose has no REST surface", exposed)
	}
}

// Reverse relations: what a declared Inverse must satisfy. ADR-0022.
func TestInverseValidation(t *testing.T) {
	tests := []struct {
		name  string
		build func(*schema.Registry)
		want  string
	}{
		{
			// The case the whole record was written for. Two references from
			// one table to another would derive the same reverse name, and an
			// author's posts are not the posts an author reviewed.
			name: "two references claim one name on the target",
			build: func(r *schema.Registry) {
				authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey())
				r.Table("posts",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Ref("author", authors).Inverse("posts").InverseExpandable(),
					schema.Ref("reviewer", authors).Inverse("posts").InverseExpandable(),
				)
			},
			want: "already claimed",
		},
		{
			name: "the name collides with a column of the target",
			build: func(r *schema.Registry) {
				authors := r.Table("authors",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("posts"),
				)
				r.Table("posts",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Ref("author", authors).Inverse("posts"),
				)
			},
			want: "collides with a column",
		},
		{
			name: "exposed without being named",
			build: func(r *schema.Registry) {
				authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey())
				r.Table("posts",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Ref("author", authors).InverseExpandable(),
				)
			},
			want: "InverseExpandable without Inverse",
		},
		{
			// Nothing about the other side of a module boundary is resolvable,
			// which is the same reason ExternalRef cannot be Expandable.
			name: "declared across a module boundary",
			build: func(r *schema.Registry) {
				r.Table("invoices",
					schema.UUIDv7("id").PrimaryKey(),
					schema.ExternalRef("tenant", "tenants.id").Inverse("invoices"),
				)
			},
			want: "cannot declare an Inverse",
		},
		{
			// The easy mistake: an expanded collection is ordered by the rows
			// it collects, which are the referencing table's, not the target's.
			name: "ordered by a column of the wrong table",
			build: func(r *schema.Registry) {
				authors := r.Table("authors",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("name"),
				)
				r.Table("posts",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Ref("author", authors).
						Inverse("posts").
						InverseExpandable(schema.ExpandOrder("name")),
				)
			},
			want: "is not a column of",
		},
		{
			// ExpandOrder/ExpandLimit only mean something when more than one row
			// could match; a unique FK rules that out structurally.
			name: "ExpandOrder on a unique-backed inverse is meaningless",
			build: func(r *schema.Registry) {
				users := r.Table("users", schema.UUIDv7("id").PrimaryKey())
				r.Table("profiles",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Ref("user", users).Unique().
						Inverse("profile").
						InverseExpandable(schema.ExpandOrder("id")),
				)
			},
			want: "has no effect",
		},
		{
			// The one place the library used to silently do the opposite of
			// what the table declares: nothing reads deleted_at, so the
			// generated DELETE removed the row and the column meant to record
			// its removal stayed NULL forever.
			name: "soft delete declared and hard delete exposed",
			build: func(r *schema.Registry) {
				r.Table("posts",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("title"),
					schema.SoftDelete(),
				).Expose(schema.REST{Ops: schema.CRUD})
			},
			want: "hard-deletes the row",
		},
		{
			// A derived index name concatenates the table and every column,
			// so this passes 63 bytes without any single part looking long.
			// Postgres truncates silently, and then every diff proposes
			// renaming the truncated name back to the declared one.
			name: "derived index name exceeds Postgres's identifier limit",
			build: func(r *schema.Registry) {
				r.Table("subscription_invoice_line_items",
					schema.UUIDv7("id").PrimaryKey(),
					schema.Text("subscription_identifier"),
					schema.Text("invoice_identifier"),
					schema.Text("line_item_identifier"),
				).AddIndex(schema.Index{Columns: []string{
					"subscription_identifier", "invoice_identifier", "line_item_identifier"}})
			},
			want: "Postgres truncates at 63",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := schema.NewRegistry()
			tt.build(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected validation to fail with %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// Two references to one table are fine as long as they are named apart, which
// is the point of declaring the name rather than deriving it.
func TestTwoInversesOnOneTargetAreFineWhenNamedApart(t *testing.T) {
	r := schema.NewRegistry()
	authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey())
	posts := r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("author", authors).Filterable().Inverse("written").InverseExpandable(),
		schema.Ref("reviewer", authors).Filterable().Inverse("reviewed").InverseExpandable(),
	).Index("author_id").Index("reviewer_id")
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	_ = posts

	inv := r.Inverses(authors)
	if len(inv) != 2 {
		t.Fatalf("got %d inverses, want 2", len(inv))
	}
	if inv[0].Name != "written" || inv[0].Column != "author_id" {
		t.Errorf("first inverse = %+v", inv[0])
	}
	if inv[1].Name != "reviewed" || inv[1].Column != "reviewer_id" {
		t.Errorf("second inverse = %+v", inv[1])
	}

	// The manifest describes the relationship from the target's side, which is
	// the side that cannot see the declaration.
	m := r.BuildManifest()
	var found int
	for _, tm := range m.Tables {
		if tm.Name != "authors" {
			continue
		}
		found = len(tm.CollectedBy)
	}
	if found != 2 {
		t.Errorf("the manifest describes %d reverse relations on authors, want 2", found)
	}
}

func TestInversesReportsOneToOneFromUniqueFK(t *testing.T) {
	r := schema.NewRegistry()
	users := r.Table("users", schema.UUIDv7("id").PrimaryKey())
	r.Table("profiles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("user", users).Unique().Inverse("profile").InverseExpandable(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	invs := r.Inverses(users)
	if len(invs) != 1 {
		t.Fatalf("got %d inverses, want 1", len(invs))
	}
	if !invs[0].OneToOne {
		t.Errorf("OneToOne = false, want true for a Ref().Unique() FK")
	}
}

// The guard-proven-both-ways companion to the test above: a non-unique FK
// must still report OneToOne = false, or every reverse relation in the
// codebase would silently start rendering as a single object.
func TestInversesReportsCollectionForNonUniqueFK(t *testing.T) {
	r := schema.NewRegistry()
	lists := r.Table("lists", schema.UUIDv7("id").PrimaryKey())
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("list", lists).Inverse("tasks").InverseExpandable(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	invs := r.Inverses(lists)
	if len(invs) != 1 {
		t.Fatalf("got %d inverses, want 1", len(invs))
	}
	if invs[0].OneToOne {
		t.Errorf("OneToOne = true, want false for a non-unique FK")
	}
}

// The manifest is the published, wire-facing description — InverseRelation's
// OneToOne has to reach InverseManifest too, or every consumer of sqlb.json
// (the SKILL.md generator among them) keeps describing a unique FK's reverse
// relation as a capped collection with a limit it will never actually hit.
func TestManifestReportsOneToOneAndOmitsLimitForAUniqueFK(t *testing.T) {
	r := schema.NewRegistry()
	users := r.Table("users", schema.UUIDv7("id").PrimaryKey())
	r.Table("profiles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("user", users).Unique().Inverse("profile").InverseExpandable(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	m := r.BuildManifest()
	var inv *schema.InverseManifest
	for _, tm := range m.Tables {
		if tm.Name != "users" {
			continue
		}
		for i := range tm.CollectedBy {
			if tm.CollectedBy[i].Name == "profile" {
				inv = &tm.CollectedBy[i]
			}
		}
	}
	if inv == nil {
		t.Fatal("no profile inverse found in the users manifest")
	}
	if !inv.OneToOne {
		t.Errorf("OneToOne = false, want true")
	}
	if inv.Limit != 0 {
		t.Errorf("Limit = %d, want 0 (a one-to-one relation has no cap to report)", inv.Limit)
	}
}

// The guard-proven-both-ways companion: an ordinary collection's manifest
// entry must keep reporting OneToOne = false and its resolved cap, or the
// fix above could have suppressed Limit for every relation rather than only
// the one-to-one ones.
func TestManifestStillReportsLimitForAPlainCollection(t *testing.T) {
	r := schema.NewRegistry()
	lists := r.Table("lists", schema.UUIDv7("id").PrimaryKey())
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("list", lists).Inverse("tasks").InverseExpandable(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	m := r.BuildManifest()
	var inv *schema.InverseManifest
	for _, tm := range m.Tables {
		if tm.Name != "lists" {
			continue
		}
		for i := range tm.CollectedBy {
			if tm.CollectedBy[i].Name == "tasks" {
				inv = &tm.CollectedBy[i]
			}
		}
	}
	if inv == nil {
		t.Fatal("no tasks inverse found in the lists manifest")
	}
	if inv.OneToOne {
		t.Errorf("OneToOne = true, want false")
	}
	if inv.Limit == 0 {
		t.Errorf("Limit = 0, want the resolved default cap")
	}
}

// A named inverse that nothing exposed is still a fact about the schema, and it
// is not an error: exposure is a separate decision (ADR-0006).
func TestAnUnexposedInverseIsNamedButNotExpandable(t *testing.T) {
	r := schema.NewRegistry()
	authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.OpList})
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("author", authors).Inverse("posts"),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	inv := r.Inverses(authors)
	if len(inv) != 1 || inv[0].Expandable {
		t.Fatalf("inverses = %+v, want one that is not expandable", inv)
	}
	for _, tm := range r.BuildManifest().Tables {
		if tm.Name != "authors" || tm.REST == nil {
			continue
		}
		for _, name := range tm.REST.Expandable {
			if name == "posts" {
				t.Error("an unexposed inverse reached the ?expand vocabulary")
			}
		}
	}
}

// Array columns. Each refusal below is the cheap direction to be wrong in:
// allowing one later is additive, withdrawing one is not (ADR-0033).

func TestArrayColumn(t *testing.T) {
	r := schema.NewRegistry()
	posts := r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("tags").Array().Filterable(),
		schema.Enum("labels", "red", "green").Array(),
	).AddIndex(schema.Index{Columns: []string{"tags"}, Method: "gin"})

	if err := r.Validate(); err != nil {
		t.Fatalf("valid array schema rejected: %v", err)
	}

	tags := posts.Field("tags").Desc()
	if !tags.Array {
		t.Error("Array() did not set the flag")
	}
	// The descriptor keeps naming the element, which is what lets the filter
	// parser bind `?tags=has.urgent` as text rather than as an array.
	if tags.Type != schema.TypeText {
		t.Errorf("element type = %q, want text", tags.Type)
	}
	if got := tags.GoType(); got != "[]string" {
		t.Errorf("GoType() = %q, want []string", got)
	}
	// An enum array keeps its value set attached to the element, for free.
	if labels := posts.Field("labels").Desc(); len(labels.EnumValues) != 2 {
		t.Errorf("enum values = %v, want two", labels.EnumValues)
	}
}

// A nullable array is still the plain slice: nil says NULL and an empty slice
// says {}, so a pointer would add a third spelling to a two-valued question.
func TestNullableArrayIsStillASlice(t *testing.T) {
	f := schema.Text("tags").Array().Nullable()
	if got := f.Desc().GoType(); got != "[]string" {
		t.Errorf("GoType() = %q, want []string", got)
	}
}

func TestArrayRefusals(t *testing.T) {
	tests := []struct {
		name  string
		field *schema.Field
		want  string
	}{
		{"sortable", schema.Text("tags").Array().Sortable(), "cannot be Sortable"},
		{"searchable", schema.Text("tags").Array().Searchable(), "requires a text column"},
		{"primary key", schema.Text("tags").Array().PrimaryKey(), "cannot be the primary key"},
		{"json elements", schema.JSON("blobs").Array(), "not an array element type"},
		{"bytea elements", schema.Bytes("chunks").Array(), "not an array element type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := schema.NewRegistry()
			r.Table("posts", schema.UUIDv7("id").PrimaryKey(), tt.field)
			err := r.Validate()
			if err == nil {
				t.Fatalf("declaration accepted, want a refusal mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A Filterable array with no GIN index is a sequential scan that returns the
// right rows, so nothing reports it. Validate does. This is ADR-0026's argument
// arriving at a case that costs a fraction as much to get right.
func TestFilterableArrayNeedsAGINIndex(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("tags").Array().Filterable(),
	)
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "needs a GIN index") {
		t.Fatalf("error = %v, want it to require a GIN index", err)
	}

	// A btree index does not satisfy it: it is the wrong access method for
	// containment, so the scan would happen anyway.
	r2 := schema.NewRegistry()
	r2.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("tags").Array().Filterable(),
	).Index("tags")
	if err := r2.Validate(); err == nil {
		t.Error("a btree index satisfied the GIN requirement")
	}

	// A column that is not filterable is not reachable through a filter at
	// all, so it needs no index.
	r3 := schema.NewRegistry()
	r3.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("tags").Array(),
	)
	if err := r3.Validate(); err != nil {
		t.Errorf("an unfilterable array was required to carry an index: %v", err)
	}
}

// Reads is the read-only exposure, and it is exactly OpRead|OpList.
func TestReadsIsReadAndList(t *testing.T) {
	if schema.Reads != schema.OpRead|schema.OpList {
		t.Fatalf("schema.Reads = %v, want read|list", schema.Reads)
	}
	for _, w := range []struct {
		op   schema.Op
		name string
	}{{schema.OpCreate, "create"}, {schema.OpUpdate, "update"}, {schema.OpDelete, "delete"}} {
		if schema.Reads.Has(w.op) {
			t.Errorf("schema.Reads exposes %s: %v", w.name, schema.Reads)
		}
	}
}

// A composite primary key is declarable, and everything that assumes one column
// per row refuses it by name rather than by reporting a missing key.
//
// The refusals are the design, not a shortfall: the tables this exists for are
// association tables where the pair is the row, and natural-key caches nothing
// references. What they needed was to be declarable at all, so that one of them
// stops taking its whole module out of the drift gate (issue #109).
func TestCompositePrimaryKey(t *testing.T) {
	t.Run("declares and reads back", func(t *testing.T) {
		r := schema.NewRegistry()
		models := r.Table("llmcatalog_models",
			schema.Text("provider"),
			schema.Text("model_id"),
			schema.Text("display_name"),
		).PrimaryKeyColumns("provider", "model_id")

		if err := r.Validate(); err != nil {
			t.Fatalf("a composite-key table must validate: %v", err)
		}
		want := []string{"provider", "model_id"}
		if got := models.CompositeKey(); !reflect.DeepEqual(got, want) {
			t.Fatalf("CompositeKey() = %v, want %v", got, want)
		}
		// Every consumer that assumes a single column sees nil, which is the
		// path a keyless table already takes.
		if models.PrimaryKey() != nil {
			t.Error("PrimaryKey() must be nil for a composite key, so single-column callers take the keyless path")
		}
	})

	t.Run("cannot be exposed over REST", func(t *testing.T) {
		r := schema.NewRegistry()
		r.Table("pairs", schema.Text("a"), schema.Text("b")).
			PrimaryKeyColumns("a", "b").
			Expose(schema.REST{Path: "/pairs", Ops: schema.Reads})

		err := r.Validate()
		if err == nil {
			t.Fatal("a composite-key table must not mount as a resource")
		}
		// Named as the reason, because "no primary key" is what every other
		// consumer would say and it points at the wrong fix.
		if !strings.Contains(err.Error(), "composite primary key") {
			t.Errorf("the refusal does not name the cause:\n%v", err)
		}
	})

	t.Run("cannot be the target of a Ref", func(t *testing.T) {
		r := schema.NewRegistry()
		models := r.Table("models", schema.Text("provider"), schema.Text("model_id")).
			PrimaryKeyColumns("provider", "model_id")
		r.Table("runs", schema.UUIDv7("id").PrimaryKey(), schema.Ref("model", models))

		err := r.Validate()
		if err == nil {
			t.Fatal("a reference is single-column, so this target must be refused")
		}
		if !strings.Contains(err.Error(), "composite primary key") {
			t.Errorf("the refusal does not distinguish this from a keyless target:\n%v", err)
		}
	})

	t.Run("refuses one column", func(t *testing.T) {
		r := schema.NewRegistry()
		r.Table("t", schema.Text("a")).PrimaryKeyColumns("a")
		err := r.Validate()
		if err == nil {
			t.Fatal("a one-column table-level key has a spelling already, and two spellings would differ in what they allow")
		}
		if !strings.Contains(err.Error(), "Field.PrimaryKey()") {
			t.Errorf("the refusal does not point at the form to use instead:\n%v", err)
		}
	})

	t.Run("refuses both forms at once", func(t *testing.T) {
		r := schema.NewRegistry()
		r.Table("t", schema.Text("a").PrimaryKey(), schema.Text("b")).
			PrimaryKeyColumns("a", "b")
		if err := r.Validate(); err == nil {
			t.Fatal("one table has one primary key")
		}
	})
}

// #262: TypeName pins the generated identifier independently of the storage
// name codegen would otherwise derive it from.
func TestTypeNameOverride(t *testing.T) {
	r := schema.NewRegistry()
	table := r.Table("board_columns", schema.UUIDv7("id").PrimaryKey())

	if got := table.TypeNameOverride(); got != "" {
		t.Fatalf("TypeNameOverride() = %q before TypeName is ever called, want \"\"", got)
	}

	if got := table.TypeName("KanbanColumn"); got != table {
		t.Fatal("TypeName should return the same *TableDef, for chaining")
	}
	if got := table.TypeNameOverride(); got != "KanbanColumn" {
		t.Fatalf("TypeNameOverride() = %q, want %q", got, "KanbanColumn")
	}
	// The storage name is a separate concern and does not move.
	if table.LocalName() != "board_columns" {
		t.Fatalf("LocalName() = %q, TypeName should not touch it", table.LocalName())
	}
}
