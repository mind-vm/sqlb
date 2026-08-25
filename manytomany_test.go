package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// Many-to-many, which sqlb has no keyword for and does not need one for
// (ADR-0056). A junction table is an ordinary table with two references, and
// the far side is reached by querying the junction and expanding forward.
//
// This is a test rather than a paragraph because the shape is the documented
// answer, and a documented answer with nothing exercising it is one that drifts.
// If any of these stop compiling or stop rendering the statement below, the
// recommendation in ADR-0056 is wrong and this is what says so.

type m2mPost struct {
	ID    string `db:"id" json:"id" sqlb:"pk"`
	Title string `db:"title" json:"title" sqlb:"filter"`

	Tagged *sqlb.Collection[m2mPostTag] `db:"-" json:"tagged,omitempty" sqlb:"expands=post_id"`
}

func (m2mPost) TableName() string { return "m2m_posts" }

type m2mTag struct {
	ID   string `db:"id" json:"id" sqlb:"pk"`
	Name string `db:"name" json:"name" sqlb:"filter"`
}

func (m2mTag) TableName() string { return "m2m_tags" }

// The junction. Both columns are references and both are expandable, which is
// what makes it queryable from either direction; the pair is the real key and
// the surrogate exists because ADR-0034 asks for one only when a row is
// addressed, which this test does not do.
type m2mPostTag struct {
	ID     string `db:"id" json:"id" sqlb:"pk"`
	PostID string `db:"post_id" json:"post_id" sqlb:"filter,expand"`
	TagID  string `db:"tag_id" json:"tag_id" sqlb:"filter,expand"`

	Post *m2mPost `db:"-" json:"post,omitempty" sqlb:"expands=post_id"`
	Tag  *m2mTag  `db:"-" json:"tag,omitempty" sqlb:"expands=tag_id"`
}

func (m2mPostTag) TableName() string { return "m2m_post_tags" }

// The recommended traversal: one statement, from the junction, expanding the far
// side. Every capability the builder has applies to it, because it is an
// ordinary query over an ordinary table.
func TestTheFarSideOfAJunctionIsOneStatementFromTheJunction(t *testing.T) {
	got, args, err := sqlb.Query[m2mPostTag]().
		Where(sqlb.F("post_id").Eq("p1")).
		Expand("tag").
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		`LEFT JOIN "m2m_tags" AS "__ex_tag" ON "__ex_tag"."id" = "m2m_post_tags"."tag_id"`,
		`json_build_object('id', "__ex_tag"."id", 'name', "__ex_tag"."name")`,
		`WHERE "m2m_post_tags"."post_id" = $1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the traversal is missing %s:\n%s", want, got)
		}
	}
	if len(args) != 1 || args[0] != "p1" {
		t.Errorf("args = %v, want [p1]", args)
	}
}

// Narrowing applies to it like any other expansion, which is what makes the
// junction row cheap: the far side contributes the one column the caller wanted.
func TestAJunctionTraversalNarrowsLikeAnyExpansion(t *testing.T) {
	got, _, err := sqlb.Query[m2mPostTag]().
		Where(sqlb.F("post_id").Eq("p1")).
		ExpandOnly("tag", "name").
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `json_build_object('name', "__ex_tag"."name")`) {
		t.Errorf("the far side was not narrowed: %s", got)
	}
}

// The other direction needs no second declaration: the same junction answers
// "which posts carry this tag".
func TestTheJunctionIsSymmetric(t *testing.T) {
	got, _, err := sqlb.Query[m2mPostTag]().
		Where(sqlb.F("tag_id").Eq("t1")).
		Expand("post").
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `LEFT JOIN "m2m_posts" AS "__ex_post" ON "__ex_post"."id" = "m2m_post_tags"."post_id"`) {
		t.Errorf("the reverse traversal did not join posts: %s", got)
	}
}

// Selecting posts *by* a tag without carrying the junction rows is the other
// half of the answer, and it is what nesting is for (ADR-0055): the junction
// becomes a set the database computes rather than rows the caller reads.
func TestPostsCarryingATagAreASubqueryOverTheJunction(t *testing.T) {
	tagged := sqlb.Query[m2mPostTag]().
		Select(sqlb.F("post_id")).
		Where(sqlb.F("tag_id").Eq("t1"))

	got, args, err := sqlb.Query[m2mPost]().Where(sqlb.F("id").InQuery(tagged)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `WHERE "id" IN (SELECT "m2m_post_tags"."post_id" FROM "m2m_post_tags" ` +
		`WHERE "m2m_post_tags"."tag_id" = $1)`
	if !strings.Contains(got, want) {
		t.Errorf("SQL:\n got %s\nwant it to contain %s", got, want)
	}
	if len(args) != 1 || args[0] != "t1" {
		t.Errorf("args = %v, want [t1]", args)
	}
}

// What the shape does not give, stated as a test so that it changes visibly if
// it ever does: an expansion is one level, so a post cannot reach its tags in
// one hop through the junction. Expand names a relation of the model being
// queried, and "tagged.tag" is not one.
func TestAPostCannotReachItsTagsInOneHop(t *testing.T) {
	_, _, err := sqlb.Query[m2mPost]().Expand("tagged.tag").SQL()
	if err == nil {
		t.Fatal("a nested expansion was accepted; ADR-0025 and ADR-0056 both need editing")
	}
	if !strings.Contains(err.Error(), "no such relation") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// Expanding the junction alone does work, and carries the foreign key the
	// second hop is made from.
	got, _, err := sqlb.Query[m2mPost]().Expand("tagged").SQL()
	if err != nil {
		t.Fatalf("expanding the junction: %v", err)
	}
	if !strings.Contains(got, `'tag_id', "__ex_tagged"."tag_id"`) {
		t.Errorf("the junction rows do not carry the far-side key: %s", got)
	}
}
