package filter_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// TestNotGroupCompiles checks the shape `not` puts around its child. The
// parenthesis is the point: `NOT` binding tighter than the `OR` under it would
// invert a different predicate than the one the caller wrote.
func TestNotGroupCompiles(t *testing.T) {
	cases := []struct{ name, json, want string }{
		{
			"leaf",
			`{"op":"not","children":[{"op":"eq","field":"status","value":"draft"}]}`,
			`NOT ("status" = $1)`,
		},
		{
			"or group",
			`{"op":"not","children":[{"op":"or","children":[` +
				`{"op":"eq","field":"status","value":"draft"},` +
				`{"op":"isnull","field":"published_at"}]}]}`,
			`NOT (("status" = $1) OR ("published_at" IS NULL))`,
		},
		{
			// A `not` under an `and` keeps its own scope rather than negating
			// the conjunction it sits in.
			"beside a sibling",
			`{"op":"and","children":[` +
				`{"op":"gte","field":"views","value":10},` +
				`{"op":"not","children":[{"op":"eq","field":"status","value":"draft"}]}]}`,
			`("views" >= $1) AND (NOT ("status" = $2))`,
		},
		{
			"double negation is not folded",
			`{"op":"not","children":[{"op":"not","children":[` +
				`{"op":"eq","field":"status","value":"draft"}]}]}`,
			`NOT (NOT ("status" = $1))`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := jsonSQL[Article](t, opts(), tc.json)
			if !strings.Contains(sql, tc.want) {
				t.Errorf("SQL %q does not contain %q", sql, tc.want)
			}
		})
	}
}

// TestNotMatchesNegatedOperator is the reason both halves of this change ship
// together. A `not` group wrapped around an operator and the operator's negated
// spelling are two ways to say one thing, and they must reach the same
// statement — otherwise the URL frontend, which cannot nest a `not`, would be
// filtering by a subtly different rule than the tree (ADR-0003).
func TestNotMatchesNegatedOperator(t *testing.T) {
	t.Run("arrays", func(t *testing.T) {
		cases := []struct{ name, wrapped, negated string }{
			{"has", `{"op":"has","field":"tags","value":"go"}`, `{"op":"nhas","field":"tags","value":"go"}`},
			{"hasany", `{"op":"hasany","field":"tags","value":["go","rust"]}`, `{"op":"nhasany","field":"tags","value":["go","rust"]}`},
			{"hasall", `{"op":"hasall","field":"tags","value":["go","rust"]}`, `{"op":"nhasall","field":"tags","value":["go","rust"]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				wSQL, wArgs := jsonSQL[Post](t, postOpts(), `{"op":"not","children":[`+tc.wrapped+`]}`)
				nSQL, nArgs := jsonSQL[Post](t, postOpts(), tc.negated)
				if wSQL != nSQL {
					t.Errorf("SQL differs:\n not-wrapped: %s\n negated op:  %s", wSQL, nSQL)
				}
				if !reflect.DeepEqual(wArgs, nArgs) {
					t.Errorf("args differ:\n not-wrapped: %#v\n negated op:  %#v", wArgs, nArgs)
				}
			})
		}
	})

	t.Run("documents", func(t *testing.T) {
		wSQL, wArgs := jsonSQL[Doc](t, docOpts(),
			`{"op":"not","children":[{"op":"hasdoc","field":"metadata","value":{"lang":"de"}}]}`)
		nSQL, nArgs := jsonSQL[Doc](t, docOpts(),
			`{"op":"nhasdoc","field":"metadata","value":{"lang":"de"}}`)
		if wSQL != nSQL {
			t.Errorf("SQL differs:\n not-wrapped: %s\n negated op:  %s", wSQL, nSQL)
		}
		if !reflect.DeepEqual(wArgs, nArgs) {
			t.Errorf("args differ:\n not-wrapped: %#v\n negated op:  %#v", wArgs, nArgs)
		}
	})
}

// The negated operators have to be reachable from the URL grammar, which is the
// whole reason they exist: a tree can spell `not`, a query string cannot.
func TestNegatedOperatorsMatchAcrossFrontends(t *testing.T) {
	t.Run("arrays", func(t *testing.T) {
		cases := []struct{ name, json, query string }{
			{"nhas", `{"op":"nhas","field":"tags","value":"go"}`, "tags=nhas.go"},
			{"nhasany", `{"op":"nhasany","field":"tags","value":["go","rust"]}`, "tags=nhasany.go,rust"},
			{"nhasall", `{"op":"nhasall","field":"tags","value":["go","rust"]}`, "tags=nhasall.go,rust"},
			{"nhas int", `{"op":"nhas","field":"sizes","value":42}`, "sizes=nhas.42"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				jSQL, jArgs := jsonSQL[Post](t, postOpts(), tc.json)
				uSQL, uArgs := urlSQL[Post](t, postOpts(), tc.query)
				if jSQL != uSQL {
					t.Errorf("SQL differs:\n json: %s\n url:  %s", jSQL, uSQL)
				}
				if !reflect.DeepEqual(jArgs, uArgs) {
					t.Errorf("args differ:\n json: %#v\n url:  %#v", jArgs, uArgs)
				}
			})
		}
	})

	t.Run("document", func(t *testing.T) {
		jSQL, jArgs := jsonSQL[Doc](t, docOpts(), `{"op":"nhasdoc","field":"metadata","value":{"lang":"de"}}`)
		uSQL, uArgs := urlSQL[Doc](t, docOpts(), `metadata=nhasdoc.{"lang":"de"}`)
		if jSQL != uSQL {
			t.Errorf("SQL differs:\n json: %s\n url:  %s", jSQL, uSQL)
		}
		if !reflect.DeepEqual(jArgs, uArgs) {
			t.Errorf("args differ:\n json: %#v\n url:  %#v", jArgs, uArgs)
		}
	})
}

// The empty-value cases are the constants the positive operators return,
// complemented. They are asserted here rather than left to read off the source
// because getting one backwards silently returns every row or none of them.
func TestNegatedArrayEmptySet(t *testing.T) {
	cases := []struct{ name, json, want string }{
		// hasany of nothing overlaps nothing and matches no row, so its negation
		// excludes no row.
		{"nhasany of nothing", `{"op":"nhasany","field":"tags","value":[]}`, "true"},
		// hasall of nothing matches every row, since every array contains the
		// empty one, so its negation excludes every row.
		{"nhasall of nothing", `{"op":"nhasall","field":"tags","value":[]}`, "false"},
		{"hasany of nothing", `{"op":"hasany","field":"tags","value":[]}`, "false"},
		{"hasall of nothing", `{"op":"hasall","field":"tags","value":[]}`, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := jsonSQL[Post](t, postOpts(), tc.json)
			if !strings.Contains(sql, "WHERE "+tc.want) {
				t.Errorf("SQL %q does not contain WHERE %s", sql, tc.want)
			}
			if len(args) != 0 {
				t.Errorf("a constant predicate should bind nothing, got %#v", args)
			}
		})
	}
}

// TestNotArity holds the unary contract. Zero children is the group check every
// group gets; two is the refusal that keeps the tree from having to be read for
// a convention about implicit conjunction.
func TestNotArity(t *testing.T) {
	cases := []struct{ name, json, want string }{
		{"no children", `{"op":"not","children":[]}`, "at least one child"},
		{
			"two children",
			`{"op":"not","children":[` +
				`{"op":"eq","field":"status","value":"draft"},` +
				`{"op":"gte","field":"views","value":10}]}`,
			"takes exactly one child",
		},
		{
			"carrying a field",
			`{"op":"not","field":"status","children":[{"op":"eq","field":"status","value":"a"}]}`,
			"cannot carry a field",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := filter.ParseFilterTree([]byte(tc.json), opts())
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if _, ok := filter.AsErrors(err); !ok {
				t.Fatalf("error is not a filter.Errors: %T", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}

	// The refusal has to say what to do instead, or a caller who meant the
	// conjunction has no way to find the spelling (ADR-0011).
	_, err := filter.ParseFilterTree([]byte(`{"op":"not","children":[`+
		`{"op":"eq","field":"status","value":"draft"},`+
		`{"op":"gte","field":"views","value":10}]}`), opts())
	if err == nil || !strings.Contains(err.Error(), `"and"`) {
		t.Fatalf("the arity refusal should offer the and/or wrapping, got %v", err)
	}
}

// `not` counts against the structural budget like any other group, so a tree
// cannot buy depth or nodes by spelling them as negations.
func TestNotCountsAgainstStructuralLimits(t *testing.T) {
	body := strings.Repeat(`{"op":"not","children":[`, 8) +
		`{"op":"eq","field":"status","value":"a"}` +
		strings.Repeat(`]}`, 8)
	_, err := filter.ParseFilterTree([]byte(body), opts())
	if err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("want depth error, got %v", err)
	}
}

// A negated operator is still gated by column kind. The negation is not a way
// around the array/document/scalar split, and the refusal names the negated
// spellings among the alternatives so a caller can find them.
func TestNegatedOperatorsAreGated(t *testing.T) {
	t.Run("array operator on a scalar column", func(t *testing.T) {
		_, err := filter.ParseFilterTree([]byte(`{"op":"nhas","field":"status","value":"x"}`), opts())
		if err == nil || !strings.Contains(err.Error(), "needs an array column") {
			t.Fatalf("want an array-column refusal, got %v", err)
		}
	})

	t.Run("document operator on a scalar column", func(t *testing.T) {
		_, err := filter.ParseFilterTree([]byte(`{"op":"nhasdoc","field":"status","value":{"a":1}}`), opts())
		if err == nil || !strings.Contains(err.Error(), "needs a JSON document column") {
			t.Fatalf("want a document-column refusal, got %v", err)
		}
	})

	t.Run("refusals offer the negated spellings", func(t *testing.T) {
		_, err := filter.ParseFilterTree([]byte(`{"op":"gt","field":"tags","value":"x"}`), postOpts())
		if err == nil {
			t.Fatal("expected a refusal")
		}
		for _, want := range []string{"nhas", "nhasany", "nhasall"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("array refusal %q does not offer %q", err, want)
			}
		}

		_, err = filter.ParseFilterTree([]byte(`{"op":"gt","field":"metadata","value":1}`), docOpts())
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(err.Error(), "nhasdoc") {
			t.Errorf("document refusal %q does not offer \"nhasdoc\"", err)
		}
	})
}

// Not over the zero predicate stays zero rather than becoming an always-false
// filter, which is what keeps an absent filter absent.
func TestNotOfZeroPred(t *testing.T) {
	if !sqlb.Not(sqlb.Pred{}).IsZero() {
		t.Error("Not(zero) should be zero")
	}
}
