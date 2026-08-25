package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/rest"
)

// A verb whose answer is not the row (#312).
//
// The case is grading: the envelope still fetches the row, still runs the verb
// inside the transaction, still persists the declared write set — and the
// response is the score, because a score is not a Post.

// GradePostResult is what codegen emits for a declared Returns.
type GradePostResult struct {
	Score int    `json:"score"`
	Grade string `json:"grade"`
}

func gradeSpec() rest.ActionSpec {
	return rest.ActionSpec{
		Name:    "grade",
		Path:    "/posts/{id}/grade",
		Field:   "GradePost",
		Summary: "Grade a post",
		Writes:  []string{"status"},
		HasBody: true,
	}
}

func mountGrade(t *testing.T, db sqlb.Executor, spec rest.ActionSpec,
	do func(context.Context, *Post, CompletePost) (GradePostResult, error)) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.ActionReturning[Post, CompletePost, GradePostResult](api, db, postOptions(), spec, do); err != nil {
		t.Fatalf("mounting the action: %v", err)
	}
	return api
}

// Everything Action does, and one thing it cannot: the response is the verb's
// answer rather than the row.
func TestActionReturningAnswersWithTheDeclaredResult(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})

	var seen *Post
	api := mountGrade(t, db.db, gradeSpec(), func(_ context.Context, p *Post, _ CompletePost) (GradePostResult, error) {
		seen = p
		p.Status = "graded"
		return GradePostResult{Score: 7, Grade: "B"}, nil
	})

	resp := api.Post("/posts/p1/grade", map[string]any{"note": "ok"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if seen == nil || seen.ID != "p1" {
		t.Fatalf("the verb was handed %+v, want the fetched row", seen)
	}

	var got GradePostResult
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, resp.Body)
	}
	if got.Score != 7 || got.Grade != "B" {
		t.Errorf("body = %+v, want the verb's answer", got)
	}
	// The row is not in it. Asserting on a column name rather than on the
	// decode, because a struct decode ignores what it does not know and would
	// pass on a response carrying the whole post.
	if strings.Contains(resp.Body.String(), `"title"`) {
		t.Errorf("the response carries the row as well as the result: %s", resp.Body)
	}

	// The write set is still persisted: the response changed, the envelope did
	// not. This is the half a "return something else" feature is most likely to
	// drop.
	stmt := db.lastStatement()
	if !strings.HasPrefix(stmt, `UPDATE "posts"`) {
		t.Fatalf("the declared write set was not persisted:\n%s", stmt)
	}
	set, _, _ := strings.Cut(stmt, " WHERE ")
	if !strings.Contains(set, `"status"`) {
		t.Errorf("the update does not write the declared column:\n%s", stmt)
	}
}

// A verb that fails answers with its error and writes nothing, exactly as the
// row-returning form does — the result is only reached when there is no error.
func TestActionReturningRefusesWithoutWriting(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})

	api := mountGrade(t, db.db, gradeSpec(), func(_ context.Context, p *Post, _ CompletePost) (GradePostResult, error) {
		p.Status = "graded"
		return GradePostResult{Score: 7}, huma.Error409Conflict("already graded")
	})

	resp := api.Post("/posts/p1/grade", map[string]any{})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), `"score"`) {
		t.Errorf("a refused verb answered with its result anyway: %s", resp.Body)
	}
	if stmt := db.lastStatement(); strings.HasPrefix(stmt, `UPDATE`) {
		t.Errorf("a refused verb persisted its write set:\n%s", stmt)
	}
}

// The collection half: a verb with no row to answer with, answering with what
// it computed instead of 204 (#310).
type MarkAllReadResult struct {
	Marked int `json:"marked"`
}

func markAllSpec() rest.ActionSpec {
	return rest.ActionSpec{
		Name:    "mark-all-read",
		Path:    "/posts/mark-all-read",
		Field:   "MarkAllReadPost",
		Summary: "Mark all posts read",
	}
}

func TestCollectionActionReturningAnswers200(t *testing.T) {
	db := newFakeDB(t)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.CollectionActionReturning[CompletePost, MarkAllReadResult](api, db.db, postOptions(), markAllSpec(),
		func(context.Context, CompletePost) (MarkAllReadResult, error) {
			return MarkAllReadResult{Marked: 12}, nil
		})
	if err != nil {
		t.Fatalf("mounting the action: %v", err)
	}

	resp := api.Post("/posts/mark-all-read")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	var got MarkAllReadResult
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, resp.Body)
	}
	if got.Marked != 12 {
		t.Errorf("body = %+v, want the verb's answer", got)
	}
}

// The other direction, and the reason the returning forms are separate
// functions rather than a flag: the default answers are unchanged. A collection
// verb with no declared result still answers 204 with no body at all, which is
// what every client generated before this expects.
func TestCollectionActionStillAnswers204(t *testing.T) {
	db := newFakeDB(t)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.CollectionAction[CompletePost](api, db.db, postOptions(), markAllSpec(),
		func(context.Context, CompletePost) error { return nil })
	if err != nil {
		t.Fatalf("mounting the action: %v", err)
	}

	resp := api.Post("/posts/mark-all-read")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	if resp.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %s", resp.Body)
	}
}

// A nil func is refused at mount by both returning forms, for the reason it is
// refused by the others: Actions{} compiles, so the compiler cannot get the
// last word and the request must not be the thing that finds out.
func TestReturningActionsRefuseANilFunc(t *testing.T) {
	db := newFakeDB(t)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.ActionReturning[Post, CompletePost, GradePostResult](api, db.db, postOptions(), gradeSpec(), nil)
	if err == nil || !strings.Contains(err.Error(), "GradePost") {
		t.Errorf("mounting with a nil func: %v, want a refusal naming the field", err)
	}

	err = rest.CollectionActionReturning[CompletePost, MarkAllReadResult](api, db.db, postOptions(), markAllSpec(), nil)
	if err == nil || !strings.Contains(err.Error(), "MarkAllReadPost") {
		t.Errorf("mounting a collection verb with a nil func: %v, want a refusal naming the field", err)
	}
}
