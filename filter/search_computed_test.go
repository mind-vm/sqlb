package filter_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// The shape from #93: a chat is named in the UI by whoever is in it, so a direct
// message has no name of its own and "type a colleague's name to find the
// conversation" cannot be answered from the chat's own columns.
//
// participant_names renders the related names into one text value, which is
// what makes it reachable by the same ?search that already covers name.
type Chat struct {
	ID               string  `db:"id" sqlb:"type:uuid,pk"`
	Name             *string `db:"name" sqlb:"type:text,search,filter"`
	ProjectName      string  `db:"project_name" sqlb:"type:text,search,readonly"`
	ParticipantNames string  `db:"participant_names" sqlb:"type:text,search,readonly"`
}

func (Chat) TableName() string { return "chats" }

func (Chat) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{
		{
			Name: "project_name",
			Expr: "(SELECT p.name FROM projects p WHERE p.id = chats.project_id)",
		},
		{
			Name: "participant_names",
			Expr: "(SELECT string_agg(m.display_name, ' ') FROM members m " +
				"WHERE m.id = ANY(chats.participant_ids))",
		},
	}
}

func chatSQL(t *testing.T, query string, computed ...string) string {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, filter.Options{
		Model:    sqlb.ModelOf[Chat](),
		Computed: computed,
	})
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Chat](), q).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql
}

// The fan-out reaches the expression, so one ?search covers the chat's own name
// and the names of the people in it.
func TestSearchFansOutOverASearchableComputedColumn(t *testing.T) {
	sql := chatSQL(t, "search=ada", "project_name", "participant_names")

	for _, want := range []string{
		`"name" ILIKE`,
		`string_agg(m.display_name, ' ')`,
		`SELECT p.name FROM projects p`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the search did not reach %q:\n%s", want, sql)
		}
	}
	// One disjunction, not three separate predicates.
	if !strings.Contains(sql, " OR ") {
		t.Errorf("the fan-out is not a disjunction:\n%s", sql)
	}
}

// The cost stays a decision the resource makes. A mount that does not select
// the expression does not search it either — otherwise the opt-in from #92
// would govern the projection and leak on the one path that runs the subquery
// per candidate row.
func TestSearchDoesNotReachAComputedColumnTheResourceDidNotSelect(t *testing.T) {
	sql := chatSQL(t, "search=ada")

	if !strings.Contains(sql, `"name" ILIKE`) {
		t.Errorf("the stored searchable column dropped out of the fan-out:\n%s", sql)
	}
	for _, absent := range []string{"string_agg", "FROM projects p"} {
		if strings.Contains(sql, absent) {
			t.Errorf("the search reached %q on a resource that does not select it:\n%s", absent, sql)
		}
	}
}

// Selecting one and not the other is the useful middle case: a resource pays
// for the expression it needs and not for the one beside it.
func TestSearchReachesOnlyTheSelectedComputedColumns(t *testing.T) {
	sql := chatSQL(t, "search=ada", "participant_names")

	if !strings.Contains(sql, "string_agg") {
		t.Errorf("the selected expression is missing from the fan-out:\n%s", sql)
	}
	if strings.Contains(sql, "FROM projects p") {
		t.Errorf("an unselected expression joined the fan-out:\n%s", sql)
	}
}

// The term is escaped on the way into every branch of the fan-out, including
// the ones that are expressions — the branch is new, the discipline is not.
func TestTheSearchTermIsStillBoundAndEscaped(t *testing.T) {
	values, _ := url.ParseQuery("search=50%")
	q, err := filter.Parse(values, filter.Options{
		Model:    sqlb.ModelOf[Chat](),
		Computed: []string{"participant_names"},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sql, args, err := filter.Apply(sqlb.Query[Chat](), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "50") {
		t.Errorf("the search term reached the SQL text:\n%s", sql)
	}
	for _, a := range args {
		if s, ok := a.(string); ok && !strings.Contains(s, `\%`) {
			t.Errorf("the LIKE metacharacter was not escaped: %q", s)
		}
	}
}
