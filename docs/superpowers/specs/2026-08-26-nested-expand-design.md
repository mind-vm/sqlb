# Nested (N-hop) `?expand=`

Status: exploratory — for discussion, no scope committed yet
Date: 2026-08-26

## Problem

Comparing sqlb against Drizzle's relational query API
(`db.query.users.findMany({with: {posts: {with: {comments: true}}}})`) and
against `docs/comparisons.md`'s own entrest counter-example surfaced the same
question from two directions: sqlb's `?expand=` goes exactly one hop, forward
or reverse, and stops. A caller who wants `list → tasks → comments` in one
response makes three round trips today, or writes the join by hand outside
the schema-declared surface.

This is not a decision to reverse. [ADR-0022](../../architecture.md#references-declare-their-inverse)
scopes one hop each direction on purpose, and its revisit trigger — "if
forward expansion turns out to be all anyone asks for in practice, or if the
cap and order need to vary per request" — doesn't cover depth either way.
Depth was never decided against; it was just never asked. This memo is that
question, asked properly, before any code changes a Stable-tier wire format.

## What N-hop has to preserve, precisely

The property worth protecting is named in
[ADR-0025](../../architecture.md#expansion-is-one-statement) and repeated in
`docs/comparisons.md`'s ent comparison: **one statement, one snapshot**.
Forward expansion compiles to a `LEFT JOIN` plus a `json_build_object`
selected as one JSON column (`expand.go:16-30`); reverse expansion compiles
to a correlated subquery in the projection, chosen specifically over
`IN(...)` because a second query "runs at a later snapshot, so a row can
vanish between them" (`expand.go:24-30`). Both already run the target's
`BeforeQuery` hooks — forward splices them into the join's `ON`
(`expand.go:339-344`, so a scoped-out target nulls rather than drops the
parent row), reverse puts them in the subquery's `WHERE`
(`expand.go:555-559`). `TestExpandRunsTheTargetsQueryHooks`
(`expand_test.go:485-528`) pins this; the comparisons-doc paragraph that used
to describe this as a conceded gap has been corrected (see this session's
addendum to `docs/comparisons.md`) — hooks already apply at one hop.

The reason N-hop is worth taking seriously rather than dismissing as "too
big" is that both mechanisms are already recursive in shape: a
`json_build_object` can nest another `json_build_object`, and a correlated
subquery's own `SELECT` can itself carry a nested expansion. Unlike ent's
N+1 model — where going deeper is mechanically free (issue one more query
per level) but the single-snapshot property was never available to trade
away — sqlb's one-statement property is *not* automatically lost by going
deeper. It has to be verified level by level, not assumed to survive, but
it is not structurally incompatible the way it would be for a per-relation
extra query. That is the central technical claim this memo rests on, and it
should be checked by hand-compiling what a 2-hop statement should look like
before any `expand.go` change:

```sql
-- list → tasks → comments, forward+reverse mixed, hand-sketched
SELECT "lists".*, 
  (SELECT json_agg(json_build_object(
      't', "tasks".*, 
      'comments', (SELECT json_agg(...) FROM "comments" 
                   WHERE "comments"."task_id" = "tasks"."id" AND <comments hook>)
    )) FROM "tasks" WHERE "tasks"."list_id" = "lists"."id" AND <tasks hook>
  ) AS "__expand_tasks"
FROM "lists" WHERE <lists hook>
```

If this shape holds — each level's hook predicate scoped to that level's
correlated subquery or join, nested rather than flattened — the one-snapshot
property survives to N hops. Whether `json_build_object`/`json_agg` nesting
nested this way is exactly what today's single-level renderer already knows
how to emit two of, recursively, or whether it needs new machinery per level,
is implementation work this memo is not attempting — it is the first thing
whoever picks this up should spend an afternoon on before writing the design
doc's Design section.

## The wire-format weight

`?expand=list` is part of a frozen response shape per ADR-0022's own words,
and `filter` — which owns `?expand=` parsing — is in the architecture doc's
Stable tier ("API surface": changes are breaking changes and are treated as
such). Any N-hop syntax is a Stable-tier commitment the moment it ships. That
is the actual reason this got a memo instead of a quick add — the compiler
work sketched above is plausibly a contained change; the wire grammar is
forever, the way `?expand=list` already is.

Three candidate syntaxes, roughly in order of how little they cost the
existing grammar:

1. **Dotted path**: `?expand=list.tasks.comments`. Cheapest to parse (split
   on `.`, walk the relation chain), and consistent with `?sort=-col` and
   `?expand=list` already being flat strings rather than structured
   parameters. Weakest at expressing "expand tasks under list, but only
   comments under this specific nested collection, not tags" — a dotted path
   has no place to hang per-level `ExpandOnly`-style column narrowing
   (`Builder.ExpandOnly` already exists at one hop) without inventing a
   second delimiter.
2. **Repeated `expand` with depth prefix**:
   `?expand=list&expand=list.tasks&expand=list.tasks.comments`, each hop
   named explicitly and cumulatively. More verbose, but makes "expand tasks
   under list without their comments" a request that just omits the third
   parameter, rather than something a dotted-path parser has to represent as
   an absence.
3. **Nested JSON body param** (closest to Drizzle's own `with:` shape, and
   to entrest's), e.g. `?expand={"list":{"tasks":{"comments":true}}}` or a
   POST-body equivalent for a `/query` style endpoint. Most expressive —
   naturally carries per-level `ExpandOnly` and (eventually) per-level
   cap/order overrides, which the ADR-0022 revisit trigger already flags as
   a live want independent of depth — but the least consistent with every
   other sqlb query parameter being a flat string, and the hardest to
   document in the manifest's worked-example-request style
   (`schema/manifest.go`'s `operatorDocs`/example-request pattern).

**Leaning, not a decision**: option 1 (dotted path) for the first cut, since
it costs the least against the existing grammar and every other multi-value
sqlb parameter is already a flat string; treat per-level `ExpandOnly`
narrowing as a fast-follow once the basic nesting compiles, rather than
blocking on designing it up front. This is a lean for someone to argue with,
not a scope line.

## Open questions for whoever picks this up

- Does the hand-sketched nested-JSON-aggregate shape above actually compile
  and return the right rows against a real multi-level fixture, or does
  Postgres's query planner do something surprising with nested correlated
  subqueries at 2-3 levels that the single-level version never had to face?
- What's the cap semantics at depth — does `has_more` at level 2 count
  correctly when level 1's own cap already truncated the parent set, or does
  a capped-then-expanded parent set produce a `has_more` that lies about the
  deeper collection?
- Does a hook that can't be requalified (raw SQL, a table outside the join)
  fail the *whole* nested statement, or just the branch under it? The
  one-hop rule ("fails the query outright rather than silently dropping")
  needs a level-aware restatement.
- Is there real demand for this, or was one hop actually "all anyone asks
  for in practice" — the exact condition ADR-0022's revisit trigger names?
  Nothing in this memo establishes demand; it only establishes that the
  one-statement property doesn't rule it out.
