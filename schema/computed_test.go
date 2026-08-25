package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The three tiers ADR-0041 names, declared and accepted: a row-local
// expression, a correlated subquery, and one whose answer depends on who is
// asking.
func TestComputedDeclaration(t *testing.T) {
	r := schema.NewRegistry()
	projects := r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Date("due_date").Filterable().Sortable(),
		schema.Int("open_tasks").Filterable(),

		schema.Computed("is_overdue", schema.TypeBool,
			schema.FromSQL("due_date < current_date AND open_tasks > 0")).
			Filterable(),
		schema.Computed("total_tasks", schema.TypeInt,
			schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)")),
		schema.Computed("is_starred", schema.TypeBool,
			schema.FromSQL("EXISTS (SELECT 1 FROM stars s "+
				"WHERE s.project_id = projects.id AND s.member_id = ?)")).
			Needs("viewer").Filterable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})

	if err := r.Validate(); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}

	// A computed column is a column: it is in Fields, so it reaches the model,
	// the clients and the CLI.
	if got, want := len(projects.Fields()), 6; got != want {
		t.Errorf("projects has %d fields, want %d", got, want)
	}
	// It is not storage, so the DDL and the diff do not see it.
	if got, want := len(projects.StoredFields()), 3; got != want {
		t.Errorf("projects has %d stored fields, want %d", got, want)
	}
	if projects.StoredField("is_overdue") != nil {
		t.Error("a computed column must not look like storage")
	}

	// Nothing writes an expression, so the declaration is ReadOnly whether or
	// not the author said so — which is what keeps it out of the generated
	// create and update bodies.
	if !projects.Field("is_overdue").Desc().ReadOnly {
		t.Error("a computed column should be ReadOnly")
	}
	if got := projects.Field("is_starred").Desc().Needs; len(got) != 1 || got[0] != "viewer" {
		t.Errorf("Needs = %v, want [viewer]", got)
	}
}

func TestComputedRefusals(t *testing.T) {
	tests := []struct {
		name  string
		build func(*schema.Registry)
		want  string
	}{
		{
			// Not "a computed column cannot be Searchable" any more — that
			// refusal was a claim about type wearing a claim about expressions,
			// and the general rule makes it directly (#93).
			name: "searchable over a non-text expression",
			build: func(r *schema.Registry) {
				r.Table("a", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("ok", schema.TypeBool, schema.FromSQL("n > 0")).Searchable())
			},
			want: "Searchable requires a text column",
		},
		{
			name: "sortable over a volatile expression",
			build: func(r *schema.Registry) {
				r.Table("b", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("is_overdue", schema.TypeBool,
						schema.FromSQL("due_date < now()")).Sortable())
			},
			want: "does not hold still between pages",
		},
		{
			name: "bind count disagrees with Needs",
			build: func(r *schema.Registry) {
				r.Table("c", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("mine", schema.TypeBool,
						schema.FromSQL("owner_id = ?")).Needs("viewer", "org"))
			},
			want: "takes 1 bind(s) but Needs names 2",
		},
		{
			name: "Needs without an expression",
			build: func(r *schema.Registry) {
				r.Table("d", schema.UUIDv7("id").PrimaryKey(),
					schema.Text("name").Needs("viewer"))
			},
			want: "only meaningful on a Computed column",
		},
		{
			name: "primary key",
			build: func(r *schema.Registry) {
				r.Table("e", schema.Computed("id", schema.TypeText, schema.FromSQL("'x'")).PrimaryKey())
			},
			want: "cannot be the primary key",
		},
		{
			name: "default",
			build: func(r *schema.Registry) {
				r.Table("f", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("n", schema.TypeInt, schema.FromSQL("1")).Default(schema.Value(0)))
			},
			want: "cannot have a Default",
		},
		{
			name: "indexed",
			build: func(r *schema.Registry) {
				r.Table("g", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("n", schema.TypeInt, schema.FromSQL("1"))).Index("n")
			},
			want: "covers a computed column",
		},
		{
			name: "enum",
			build: func(r *schema.Registry) {
				r.Table("h", schema.UUIDv7("id").PrimaryKey(),
					schema.Computed("state", schema.TypeEnum, schema.FromSQL("'open'")))
			},
			want: "cannot be an Enum",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := schema.NewRegistry()
			tc.build(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Sortable over a stable expression is fine — it is the volatile one the
// keyset cannot page.
func TestComputedSortableIsAllowedWhenStable(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("a", schema.UUIDv7("id").PrimaryKey(),
		schema.Int("total").Filterable(),
		schema.Computed("progress", schema.TypeInt,
			schema.FromSQL("(done * 100 / NULLIF(total, 0))")).Sortable())
	if err := r.Validate(); err != nil {
		t.Fatalf("a stable computed column should be sortable: %v", err)
	}
}

// A computed column is nullable unless it says otherwise, which is the opposite
// of a stored one (#147).
//
// The default that assumed non-null was not merely unhelpful: a correlated
// subquery matching nothing produced a 500 at scan time, from a declaration
// `sqlb generate` accepted and the drift gate ignored, on rows a fixture is
// unlikely to contain. Nullable is the direction that fails safely — a pointer
// scans a non-null value fine, and the reverse is the 500.
func TestComputedIsNullableUnlessItSaysOtherwise(t *testing.T) {
	r := schema.NewRegistry()
	projects := r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Int("open_tasks"),

		// The shape from the issue: a cross-module lookup with no foreign key,
		// so a row pointing at nothing is ordinary rather than exceptional.
		schema.Computed("project_name", schema.TypeText,
			schema.FromSQL("(SELECT p.name FROM projects p WHERE p.id = time_entries.project_id)")),
		// count(*) over a subquery is 0, never NULL.
		schema.Computed("total_tasks", schema.TypeInt,
			schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)")).
			NotNull(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}

	if !projects.Field("project_name").Desc().Nullable {
		t.Error("a computed column defaulted to NOT NULL; an expression that matches nothing is NULL, and the model has to be able to hold it")
	}
	if projects.Field("total_tasks").Desc().Nullable {
		t.Error("NotNull did not take on a computed column")
	}
	// The default runs the other way for storage, where the DDL carries the
	// answer and the round trip checks it.
	if projects.Field("open_tasks").Desc().Nullable {
		t.Error("a stored column defaulted to nullable")
	}
}

// The manifest is what a program reads to answer "what does this endpoint
// serve, and what did the server have to do to serve it".
func TestComputedInManifest(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Computed("is_starred", schema.TypeBool,
			schema.FromSQL("EXISTS (SELECT 1 FROM stars s WHERE s.member_id = ?)")).
			Needs("viewer").Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})

	raw, err := json.Marshal(r.BuildManifest())
	if err != nil {
		t.Fatalf("marshalling the manifest: %v", err)
	}
	for _, want := range []string{`"computed":true`, `"needs":["viewer"]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("manifest is missing %s:\n%s", want, raw)
		}
	}
}

// The lint rules about indexes have nothing to say about a column that cannot
// be indexed, so they say the one thing that is true instead.
func TestComputedLint(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Computed("total_tasks", schema.TypeInt,
			schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)")).
			Filterable(),
	)

	var found bool
	for _, d := range r.Lint() {
		switch d.Rule {
		case "computed-without-index":
			found = true
			if !strings.Contains(d.Message, "runs a subquery") {
				t.Errorf("a subquery's cost should be named: %s", d.Message)
			}
		case "unindexed-filter", "unindexed-sort":
			t.Errorf("an index rule fired on a column that cannot be indexed: %s", d)
		}
	}
	if !found {
		t.Error("a filterable computed column should be reported once")
	}
}

// The declaration #93 needs, and the one the blanket refusal made impossible: a
// text expression over a related table, searchable.
//
// A chat is named in the UI by whoever is in it — a direct message has no name
// column at all — so "type a colleague's name to find the conversation" cannot
// be answered by fanning out over the chat's own columns. It can be answered by
// one text expression that renders the participants' names, and that expression
// is a computed column.
func TestATextComputedColumnMayBeSearchable(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("chats",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Nullable().Searchable(),
		schema.Computed("participant_names", schema.TypeText,
			schema.FromSQL("(SELECT string_agg(m.display_name, ' ') FROM members m "+
				"WHERE m.id = ANY(chats.participant_ids))")).
			Searchable(),
	).Expose(schema.REST{Path: "/chats", Ops: schema.OpList})

	if err := r.Validate(); err != nil {
		t.Fatalf("a searchable text expression was refused: %v", err)
	}
}

// And the guard that has to survive it: the cost of searching a correlated
// subquery is real, so it stays a per-resource decision rather than something
// the declaration imposes on every reader. That is enforced in filter, not
// here, but the declaration being legal is what makes the pairing testable.
func TestASearchableComputedColumnIsStillReadOnly(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("chats",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Computed("participant_names", schema.TypeText,
			schema.FromSQL("(SELECT 'x')")).Searchable(),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range r.Tables()[0].Fields() {
		if d := f.Desc(); d.Name == "participant_names" && !d.ReadOnly {
			t.Error("a searchable computed column is writable")
		}
	}
}
