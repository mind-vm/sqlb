package pgtest

import (
	"context"
	"net/url"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/schema"
)

// Searching across a relation, against a real Postgres.
//
// The engine's tests check that the expression reaches the fan-out. What they
// cannot check is that `(subquery) ILIKE $1` is a statement Postgres accepts in
// a WHERE built by OR-ing it with ordinary column predicates, and that it
// matches the rows a person would expect — a direct message with no name of its
// own, found by typing the name of somebody in it (#93).

type SearchChat struct {
	ID               int64   `db:"id" sqlb:"type:bigint,pk,default"`
	Name             *string `db:"name" sqlb:"type:text,search"`
	ParticipantNames string  `db:"participant_names" sqlb:"type:text,search,readonly"`
}

func (SearchChat) TableName() string { return "searchchats" }

func (SearchChat) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{{
		Name: "participant_names",
		Expr: "(SELECT string_agg(m.display_name, ' ') FROM searchmembers m " +
			"WHERE m.chat_id = searchchats.id)",
	}}
}

func searchRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("searchchats",
		schema.BigInt("id").PrimaryKey().Default(schema.Expr("nextval('searchchats_id_seq')")),
		schema.Text("name").Nullable().Searchable(),
		schema.Computed("participant_names", schema.TypeText,
			schema.FromSQL("(SELECT string_agg(m.display_name, ' ') FROM searchmembers m "+
				"WHERE m.chat_id = searchchats.id)")).Searchable(),
	)
	return r
}

func TestSearchMatchesThroughAComputedColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	mustExec(t, raw, `CREATE SEQUENCE searchchats_id_seq`)
	applySchema(t, raw, searchRegistry())
	mustExec(t, raw, `CREATE TABLE searchmembers (chat_id bigint NOT NULL, display_name text NOT NULL)`)

	// A named chat, and a direct message with no name at all — the row the
	// old fan-out could never find.
	mustExec(t, raw, `INSERT INTO searchchats (name) VALUES ('Release planning')`)
	mustExec(t, raw, `INSERT INTO searchchats (name) VALUES (NULL)`)
	mustExec(t, raw, `INSERT INTO searchmembers (chat_id, display_name) VALUES (1, 'Grace Hopper')`)
	mustExec(t, raw, `INSERT INTO searchmembers (chat_id, display_name) VALUES (2, 'Ada Lovelace')`)

	db := sqlb.New(raw)
	find := func(term string) []SearchChat {
		t.Helper()
		values, _ := url.ParseQuery("search=" + url.QueryEscape(term))
		q, err := filter.Parse(values, filter.Options{
			Model:    sqlb.ModelOf[SearchChat](),
			Computed: []string{"participant_names"},
		})
		if err != nil {
			t.Fatalf("parsing ?search=%s: %v", term, err)
		}
		rows, err := filter.Apply(sqlb.Query[SearchChat](), q).All(ctx, db)
		if err != nil {
			t.Fatalf("searching for %q: %v", term, err)
		}
		return rows
	}

	// The case the issue is about: the chat has no name, and is found anyway.
	if got := find("Ada"); len(got) != 1 || got[0].ID != 2 {
		t.Errorf("searching a participant's name found %d rows, want the unnamed chat", len(got))
	}
	// The chat's own column still works, which is the half that must not break.
	if got := find("Release"); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("searching the chat's own name found %d rows, want the named chat", len(got))
	}
	// And a term matching neither matches nothing, so the fan-out is not just
	// returning everything.
	if got := find("Babbage"); len(got) != 0 {
		t.Errorf("a term matching nothing returned %d rows", len(got))
	}
}
