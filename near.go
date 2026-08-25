package sqlb

// Similarity search over a vector column.
//
// The distance expression appears three times in one statement — projected as a
// score, compared against a threshold, and ordered by — and the three must
// agree or the query is quietly wrong: a score computed one way and an ordering
// computed another produce rows sorted by something other than the number
// beside them. So [Near] returns a handle that yields all three, and the vector
// is written once:
//
//	near := sqlb.Near(sqlb.F("embedding"), queryVec)
//
//	rows, err := sqlb.Collect[Hit](ctx, db, sqlb.Query[Chunk]().
//	    Select(sqlb.F("id"), sqlb.F("body"), near.Similarity()).
//	    Where(sqlb.F("workspace_id").Eq(ws), near.AtLeast(0.75)).
//	    OrderBy(near.Nearest()).
//	    Limit(10))
//
// # This is an exact scan
//
// There is no index kind in the schema DSL yet, which [ADR-0026] stages as a
// second decision to be taken when a corpus outgrows exact search. So the
// statement above sorts every row the filter selected. That is the right shape
// at the size the module this was designed against actually runs, and it has
// the property an approximate index does not: the answer is the answer. A
// filtered search over an ANN index silently returns fewer rows than it was
// asked for, which is measured in pgtest/pgvector_test.go and is the failure
// the index half exists to refuse.
//
// # The metric is cosine, and is not an argument
//
// Every operator below is `<=>`. There is deliberately no way to ask for L2 or
// inner product, even though an exact scan would answer any of them correctly —
// because ADR-0026 puts the metric on the *index declaration*, so that a query
// cannot ask for one no index serves. Shipping a metric argument now and
// insisting on the declaration later would break every caller; adding the
// argument later, if the index declaration turns out to need it, is additive.
// That asymmetry is the same one ADR-0017 used to start enums from text.
//
// [ADR-0026]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#vectors-declare-their-index

// Nearness is a similarity comparison between a vector column and a query
// vector, from which the projection, the threshold and the ordering are all
// derived. Build one with [Near].
type Nearness struct {
	col  Expr
	slot *sharedValue
}

// sharedValue is one bind parameter that several expressions refer to. The
// compiler binds it on first sight and reuses the placeholder afterwards, so a
// value written three times in a statement is sent once.
type sharedValue struct{ value any }

// sharedParam is the expression node that refers to one.
type sharedParam struct{ slot *sharedValue }

func (sharedParam) exprNode() {}

// Near compares a vector column against a query vector.
//
// The vector binds as one parameter, cast to `vector` — which is what makes the
// comparison work against a column whose type Postgres knows and whose
// parameter type it would otherwise have to guess.
//
// One parameter, not three. The handle names the vector in the projection, the
// threshold and the ordering, and all three render the same placeholder: an
// embedding is about twenty kilobytes and sending it once per mention would
// treble every search's payload for nothing.
func Near(f Field, v Vector) Nearness {
	return Nearness{col: f.Column(), slot: &sharedValue{value: v}}
}

// distance is `col <=> $n`, the expression everything here is built from.
func (n Nearness) distance() Expr {
	return Binary{
		Op:    "<=>",
		Left:  n.col,
		Right: Cast{Inner: sharedParam{slot: n.slot}, Type: "vector"},
	}
}

// score is `1 - (col <=> $n)`: cosine *similarity* rather than cosine distance,
// so that larger is closer. Distance is the number Postgres computes and
// similarity is the number people reason about, and reporting the one while
// calling it the other is a bug that looks like a ranking problem.
func (n Nearness) score() Expr {
	return Binary{Op: "-", Left: Raw{SQL: "1"}, Right: n.distance()}
}

// Similarity selects the score, aliased `similarity`. Larger is closer, and it
// is in [0, 2] for cosine — 1 for an identical direction, 0 for an orthogonal
// one, and above 1 only for vectors pointing away from each other.
//
// Rename it with As if the destination struct calls it something else.
func (n Nearness) Similarity() Selection {
	return Sel(n.score()).As("similarity")
}

// Nearest orders by distance ascending, which is closest first.
//
// It is the distance that is ordered by and not the score, though the two are
// equivalent orderings: `ORDER BY col <=> $1` is the shape an ANN index can
// serve, and writing it this way means adding an index later changes the plan
// rather than the statement.
func (n Nearness) Nearest() Order { return OrderBy(n.distance()) }

// AtLeast keeps rows whose similarity is at or above score.
//
// Note what this does to a shortfall: with a threshold applied, a query
// returning fewer rows than its limit is the *normal* case, so counting rows
// cannot tell "nothing was similar enough" from "the search did not look hard
// enough". That distinction does not matter under an exact scan, where the
// second cannot happen. It is why ADR-0026 says an under-recall signal must
// count before the threshold cut rather than after — a thing to remember when
// the index half is built, and harmless until then.
func (n Nearness) AtLeast(score float64) Pred {
	return pred(Binary{Op: ">=", Left: n.score(), Right: Param{Value: score}})
}

// Distance selects the raw distance, aliased `distance`, for a caller that
// wants the number Postgres computed rather than the one people read.
//
// Offered because re-ranking against another system's scores needs the
// comparable quantity, and computing `1 - similarity` back is a rounding error
// nobody should have to think about.
func (n Nearness) Distance() Selection {
	return Sel(n.distance()).As("distance")
}
