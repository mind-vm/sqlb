# Cardinality-Aware One-to-One Relationships Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a `Ref(...).Unique()` foreign key generate as a single nullable
object on its reverse (`Inverse`) side — in the schema registry, the query
compiler, the REST wire format, and the Go/TypeScript/Dart clients — instead
of the capped-collection shape (`{items, has_more}`) every reverse relation
gets today regardless of whether more than one row could ever match.

**Architecture:** A single new fact — "this FK is unique, so its reverse
relation is one-to-one" — flows from `schema.FieldDesc.Unique` through
`Registry.Inverses()` into codegen (which changes the generated field's
*type*, not just a tag), and separately into the query compiler's runtime
relation resolution (which reads the *type* codegen emitted via reflection,
plus a new tag token for the cases reflection alone can't disambiguate).
Nothing is stored on `Reference` itself — the "no derived state stored on a
schema struct" precedent (`TableDef.implicitIndexes()`) is followed instead:
`OneToOne` is computed once, where `Registry.Inverses()` already has the
referencing field's `*FieldDesc` in scope.

**Tech Stack:** Go (schema, root query-compiler package, codegen), generated
TypeScript and Dart clients, Huma-driven OpenAPI (no direct change needed
there — see Task 3), Postgres via pgtest/example-app integration tests.

**Spec:** `docs/superpowers/specs/2026-08-15-one-to-one-relationships-design.md`

## Global Constraints

- No new schema verb: one-to-one is inferred from a single-column
  `Field.Unique()` on a `Ref`'s own FK column — never from a composite
  `Unique(a, b)`/`UniqueIndex(a, b)` that merely includes it.
- This deliberately breaks the Frozen list-envelope guarantee
  (`docs/compatibility.md`) for this one relation shape, pre-1.0-or-never,
  following the `Executor` precedent in the architecture doc's "The driver is
  a dependency" section. Task 7 records that decision in a new ADR section
  and a `compatibility.md` carve-out — do not skip it.
- ADR references in this plan point at `docs/architecture.md` anchors
  (`#references-declare-their-inverse` etc.), not `docs/adr/*.md` — this repo
  consolidated ADRs into anchored sections in `docs/architecture.md` and
  removed `docs/adr/` before this plan was written.
- Per the "guards proven both ways" rule
  (`docs/architecture.md#guards-proven-both-ways`): every new branch added in
  this plan needs a test proving the *old* behavior still fires when the new
  condition (`Unique`) is absent, not just a test that the new behavior fires
  when it's present.
- Root-module tests (schema, root package, codegen) are database-free and run
  via `go test ./...` at the repo root. Task 6's REST-level test needs a real
  Postgres — run it via `mise run pg-up` first, from `example/tasks/`, its own
  Go module.

---

### Task 1: Schema layer — `InverseRelation.OneToOne` and its validation rule

**Files:**
- Modify: `schema/registry.go` (`InverseRelation` struct, ~line 1033-1048;
  `Registry.Inverses`, ~line 1060-1084; `validateInverse`, ~line 892-936)
- Test: `schema/schema_test.go` (extend `TestInverseValidation`, ~line
  539-662, and add two new standalone tests)

**Interfaces:**
- Produces: `schema.InverseRelation.OneToOne bool` — read by codegen in Task
  2/3/4 to decide whether to emit `sqlb.Collection[T]` or a bare pointer, and
  by the new validation rule in this task.

- [ ] **Step 1: Write the failing tests**

Add to `schema/schema_test.go`, near `TestInverseValidation`:

```go
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
```

Add a new case to the `tests` table inside `TestInverseValidation`
(`schema_test.go:539-662`), in the same style as the existing
`"ordered by a column of the wrong table"` case:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./schema/... -run 'TestInversesReports|TestInverseValidation' -v`
Expected: `TestInversesReportsOneToOneFromUniqueFK` FAILs (`OneToOne` field
does not exist — compile error), and the new `TestInverseValidation` case
FAILs (validation does not reject it — no `"has no effect"` in the error, or
the whole package fails to compile because `OneToOne` is undefined).

- [ ] **Step 3: Add `OneToOne` to `InverseRelation` and populate it**

In `schema/registry.go`, add the field to the struct (~line 1033-1048):

```go
type InverseRelation struct {
	Name       string    // the name ?expand uses on the target
	Table      *TableDef // the table whose rows are collected
	Column     string    // that table's foreign key column
	Order      string    // ordering column, with a leading "-" for descending
	Limit      int       // cap as declared; zero means DefaultExpandLimit
	Expandable bool      // reachable through ?expand on the target
	// OneToOne reports that Column carries a single-column unique
	// constraint, so at most one row of Table can ever point back here. It
	// is derived, never declared: a unique foreign key is structurally
	// one-to-one whether or not the schema names it that way.
	OneToOne bool
}
```

Populate it in `Registry.Inverses` (~line 1060-1084) — the loop already has
`d := f.Desc()` in scope, which is the referencing field's own descriptor:

```go
func (r *Registry) Inverses(t *TableDef) []InverseRelation {
	if t == nil {
		return nil
	}
	var out []InverseRelation
	for _, src := range r.Tables() {
		for _, f := range src.Fields() {
			d := f.Desc()
			if d.Ref == nil || d.Ref.Inverse == "" || d.Ref.External || d.Ref.Table != t {
				continue
			}
			out = append(out, InverseRelation{
				Name:       d.Ref.Inverse,
				Table:      src,
				Column:     d.Name,
				Order:      d.Ref.InverseOrder,
				Limit:      d.Ref.InverseLimit,
				Expandable: d.Ref.InverseExpandable,
				OneToOne:   d.Unique,
			})
		}
	}
	return out
}
```

- [ ] **Step 4: Add the `ExpandOrder`/`ExpandLimit`-on-unique validation rule**

In `schema/registry.go`, `validateInverse` (~line 892-936), add after the
existing `ExpandLimit`/`ExpandOrder` checks:

```go
	if d.Unique && (ref.InverseOrder != "" || ref.InverseLimit != 0) {
		report(t.name, d.Name,
			"ExpandOrder/ExpandLimit on Inverse %q has no effect: %s.%s is unique, "+
				"so at most one row can ever match; remove ExpandOrder/ExpandLimit",
			ref.Inverse, t.name, d.Name)
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./schema/... -run 'TestInversesReports|TestInverseValidation' -v`
Expected: PASS for all three.

- [ ] **Step 6: Run the full schema package suite**

Run: `go test ./schema/...`
Expected: PASS — confirms no existing test asserted the old struct literal
shape of `InverseRelation{...}` in a way that breaks on the new field.

- [ ] **Step 7: Commit**

```bash
git add schema/registry.go schema/schema_test.go
git commit -m "$(cat <<'EOF'
feat(schema): a unique foreign key already is one-to-one, so Inverses says so

InverseRelation.OneToOne is derived from Field.Unique wherever
Registry.Inverses already has the referencing FieldDesc in scope — no new
schema verb, no state stored on Reference. Also rejects ExpandOrder/
ExpandLimit on a unique-backed Inverse, since neither means anything once
at most one row can match.
EOF
)"
```

---

### Task 2: Root package — resolve and compile one-to-one reverse relations

**Files:**
- Modify: `relation.go` (`RelationInfo` struct, `newRelation`,
  `resolveRelations`, `Target()`, and the `relationTag` tag parser —
  locate the parser first, see Step 1)
- Modify: `expand.go` (`compileExpansions`)
- Test: a new `relation_reverse_test.go` at the repo root (database-free —
  asserts on `.SQL()` output, following the pattern in
  `example_query_test.go`)

**Interfaces:**
- Consumes: nothing from Task 1 directly — this task's `reverse` tag token is
  emitted by codegen in Task 3, but the runtime change here is independently
  testable by hand-writing a struct with the tag.
- Produces: `RelationInfo.Reverse bool` — true for both capped collections
  and one-to-one reverse relations (anything whose FK lives on the target,
  not on the base row). `RelationInfo.Collection` keeps its current meaning
  (capped, many rows) and is `false` for a one-to-one reverse relation, so
  existing code that branches on `Collection` alone (e.g.
  `compileExpansionSelections`) is unaffected. A one-to-one reverse relation
  is declared today only via the `reverse` struct-tag token — Task 3 wires
  codegen to emit it.

- [ ] **Step 1: Locate the relation struct-tag parser**

Run: `grep -n "relationTag" relation.go`

This finds the struct definition and parsing function backing `expands=`,
`order=`, `limit=` today (confirmed to exist and feed `newRelation(sf,
index, rt relationTag)`, but not pre-read for this plan). Read it before
Step 3 below — the new `reverse` token must be added to the same
comma-separated grammar the parser already implements for `order=`/`limit=`.

- [ ] **Step 2: Write the failing test**

Create `/Users/jryannel/dev/github.com/mind-vm/sqlb/relation_reverse_test.go`:

```go
package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

type oneToOneUser struct {
	ID      string          `db:"id"`
	Profile *oneToOneProfile `db:"-" json:"profile,omitempty" sqlb:"expands=user_id,reverse"`
}

type oneToOneProfile struct {
	ID     string `db:"id"`
	UserID string `db:"user_id"`
}

// The guard-proven-both-ways companion lives beside it: a plain forward
// relation and a capped collection must keep their existing SQL shape, so
// this new branch cannot be the only path exercised.
func TestReverseTagJoinsOnTheTargetsForeignKey(t *testing.T) {
	q := sqlb.Query[oneToOneUser]().Expand("profile")
	got, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if !strings.Contains(got, `LEFT JOIN "profiles" AS "__ex_profile"`) {
		t.Errorf("missing the expected join:\n%s", got)
	}
	if !strings.Contains(got, `"__ex_profile"."user_id" = "one_to_one_users"."id"`) {
		t.Errorf("join condition should be target.FK = base.PK, got:\n%s", got)
	}
	if strings.Contains(got, "has_more") {
		t.Errorf("a one-to-one reverse relation must not use the capped-collection envelope:\n%s", got)
	}
}
```

(Table/alias name literals above are illustrative — adjust to match this
repo's actual snake_case table-naming convention once `go test` shows the
real generated SQL; the assertions on join direction and the absence of
`has_more` are the ones that matter.)

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test . -run TestReverseTagJoinsOnTheTargetsForeignKey -v`
Expected: FAIL — either a parse error on the unrecognized `reverse` tag
token, or a wrong/missing join, or a panic resolving `rel.FK` against the
wrong table.

- [ ] **Step 4: Add the `reverse` tag token and `RelationInfo.Reverse`**

In `relation.go`, add a field next to `Collection` (~line 73-75):

```go
	// Collection reports that this relation is the reverse direction — many
	// rows of the target pointing back at one row of this model.
	Collection bool
	// Reverse reports that this relation's foreign key lives on the target,
	// not on this row — true for both a capped Collection and a one-to-one
	// reverse relation. Collection narrows it further to "and there may be
	// more than one". FK resolution and the query compiler's join direction
	// both key off Reverse; only the capped-envelope machinery keys off
	// Collection specifically.
	Reverse bool
```

Add the parsed `reverse` token to whatever struct the parser located in Step
1 populates as `rt.reverse bool`, following the exact pattern used for its
existing boolean-ish tokens.

In `newRelation` (~line 236-279), set `Reverse` in both existing branches and
add the new one:

```go
	if elem, isCollection := collectionElem(sf.Type); isCollection {
		r.Collection = true
		r.Reverse = true
		r.Elem = elem
		return r, nil
	}

	if rt.reverse {
		if rt.order != "" || rt.limit != 0 {
			return nil, fmt.Errorf(
				"sqlb: field %s expands %q with reverse and declares order or limit, which only a "+
					"capped collection uses; a one-to-one reverse relation has no order to break ties "+
					"in and nothing to cap",
				sf.Name, rt.fk)
		}
		r.Reverse = true
		elem := sf.Type
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			return nil, fmt.Errorf(
				"sqlb: field %s expands %q with reverse but is %s, want a struct or a pointer to one",
				sf.Name, rt.fk, sf.Type.Kind())
		}
		r.Elem = elem
		return r, nil
	}

	if rt.order != "" || rt.limit != 0 {
		return nil, fmt.Errorf(
			"sqlb: field %s expands %q and declares order or limit, which only a collection uses; "+
				"make it a *sqlb.Collection[T] to expand the reverse direction, or drop the options",
			sf.Name, rt.fk)
	}
	// ... existing forward-relation fallthrough is unchanged below this point.
```

- [ ] **Step 5: Swap `Collection` for `Reverse` in FK-resolution gating**

In `resolveRelations` (~line 293-321), the line that currently reads
`if r.Collection { continue }` (skipping FK resolution for anything whose FK
isn't a column of the base model) must become:

```go
		if r.Reverse {
			continue
		}
```

In `Target()` (~line 98-137), the line that currently reads
`if r.err != nil || !r.Collection { return }` must become:

```go
		if r.err != nil || !r.Reverse {
			return
		}
```

This makes a one-to-one reverse relation resolve its `FK` lazily against the
target — exactly like a capped collection does — instead of (incorrectly)
eagerly against the base model.

- [ ] **Step 6: Swap the join direction in `compileExpansions`**

In `expand.go` (~line 298-339), the `ON` clause currently assumes every
non-`Collection` relation is forward (`target.PK = base.FK`). Change it to
branch on `Reverse`:

```go
		alias := expandAlias(name)
		c.write(" LEFT JOIN ")
		c.ident(target.Table)
		c.write(" AS ")
		c.ident(alias)
		c.write(" ON ")
		if rel.Reverse {
			// The mirror image of the forward case, and of the correlated
			// WHERE compileCollection writes for a capped collection: the
			// foreign key lives on the target, not on this row.
			c.column(Column{Table: alias, Name: rel.FK.Name})
			c.write(" = ")
			c.column(Column{Table: b.from(), Name: b.model.PK.Name})
		} else {
			c.column(Column{Table: alias, Name: target.PK.Name})
			c.write(" = ")
			c.column(Column{Table: b.from(), Name: rel.FK.Name})
		}
```

`compileExpansionSelections` needs **no change** — its `CASE WHEN
alias.PK IS NULL THEN NULL ELSE json_build_object(...) END` check is
agnostic to which column populated the join; it only depends on whether the
joined alias row exists.

- [ ] **Step 7: Run the test to verify it passes**

Run: `go test . -run TestReverseTagJoinsOnTheTargetsForeignKey -v`
Expected: PASS. Inspect the actual printed SQL from a `t.Log(got)` if the
table/alias literals in Step 2 needed adjusting, then remove the log line.

- [ ] **Step 8: Run the full root-package suite**

Run: `go test ./...` (from the repo root, first module only per
`CLAUDE.md`)
Expected: PASS — this is the guard-proven-both-ways check: every existing
forward-relation and capped-collection test must still pass unchanged, since
`Collection` still means exactly what it did before and only `Reverse` is
new.

- [ ] **Step 9: Commit**

```bash
git add relation.go expand.go relation_reverse_test.go
git commit -m "$(cat <<'EOF'
feat(query): a one-to-one reverse relation joins like a forward one, not like a capped collection

RelationInfo.Reverse separates "foreign key lives on the target" (true for
both a capped Collection and a one-to-one reverse relation) from
Collection's narrower "and there may be more than one". FK resolution and
compileExpansions's join direction both key off Reverse; the envelope
machinery in compileCollection is untouched.
EOF
)"
```

---

### Task 3: Go client codegen — emit a bare pointer for a one-to-one inverse

**Files:**
- Modify: `codegen/models.go` (`inverse` struct ~line 307-312, `inversesOf`
  ~line 314-354, the field-emission call site ~line 177-180)
- Test: `codegen/codegen_test.go` (extend near `inverseFixture()`,
  ~line 684-738)

**Interfaces:**
- Consumes: `schema.InverseRelation.OneToOne` (Task 1); emits the `reverse`
  struct-tag token `relation.go` parses (Task 2).
- Produces: nothing new consumed elsewhere in this plan, but this is the
  reference implementation Task 4 (TypeScript) and Task 5 (Dart) mirror.

- [ ] **Step 1: Write the failing test**

Add to `codegen/codegen_test.go`, alongside `inverseFixture()`:

```go
// oneToOneFixture is a Ref().Unique() FK — a genuine one-to-one, unlike
// inverseFixture's ordinary one-to-many "authors"→"posts" relation.
func oneToOneFixture() *schema.Registry {
	r := schema.NewRegistry()
	users := r.Table("users", schema.UUIDv7("id").PrimaryKey())
	r.Table("profiles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("user", users).Unique().
			Expandable().Inverse("profile").InverseExpandable(),
	)
	return r
}

func TestOneToOneInverseEmitsAPointerNotACollection(t *testing.T) {
	files := generate(t, oneToOneFixture())
	models := files["models_gen.go"]
	if !contains(models, `Profile *Profile `+"`"+`db:"-" json:"profile,omitempty" sqlb:"expands=user_id,reverse"`+"`") {
		t.Errorf("one-to-one inverse should emit a bare *Profile with a reverse tag, got:\n%s", models)
	}
	if contains(models, "sqlb.Collection[Profile]") {
		t.Errorf("one-to-one inverse must not emit the capped-collection type:\n%s", models)
	}
}

// The guard-proven-both-ways companion: an ordinary (non-unique) inverse
// must keep emitting the collection shape unchanged.
func TestNonUniqueInverseStillEmitsACollection(t *testing.T) {
	files := generate(t, inverseFixture())
	models := files["models_gen.go"]
	if !contains(models, "sqlb.Collection[Post]") {
		t.Errorf("non-unique inverse should still emit sqlb.Collection[Post], got:\n%s", models)
	}
}
```

- [ ] **Step 2: Run the tests to verify the first fails**

Run: `go test ./codegen/... -run 'TestOneToOneInverse|TestNonUniqueInverse' -v`
Expected: `TestOneToOneInverseEmitsAPointerNotACollection` FAILs (current code
always emits `sqlb.Collection[Profile]`); `TestNonUniqueInverseStillEmitsACollection`
already PASSes (recording the pre-change baseline).

- [ ] **Step 3: Add `oneToOne` to the codegen `inverse` struct and `inversesOf`**

In `codegen/models.go` (~line 307-312):

```go
type inverse struct {
	field    string // Go field name, e.g. "Tasks"
	target   string // Go type of the child model, e.g. "Task"
	relation string // name on the wire, e.g. "tasks"
	tag      string // the sqlb tag, e.g. "expands=list_id,order=-created_at"
	oneToOne bool   // emit a bare pointer instead of *sqlb.Collection[T]
}
```

In `inversesOf` (~line 314-354), replace the tag-building block:

```go
		tag := "expands=" + inv.Column
		if inv.OneToOne {
			tag += ",reverse"
		} else {
			if inv.Order != "" {
				tag += ",order=" + inv.Order
			}
			tag += ",limit=" + strconv.Itoa(inv.Cap())
		}
		out = append(out, inverse{
			field:    name,
			target:   TypeName(inv.Table.LocalName()),
			relation: inv.Name,
			tag:      tag,
			oneToOne: inv.OneToOne,
		})
```

- [ ] **Step 4: Branch the field emission on `oneToOne`**

At the field-emission call site (~line 177-180):

```go
		for _, inv := range inverses {
			if inv.oneToOne {
				fmt.Fprintf(b, "\t%s *%s `db:\"-\" json:%q sqlb:%q` // filled in by ?expand=%s\n",
					inv.field, inv.target, inv.relation+",omitempty", inv.tag, inv.relation)
				continue
			}
			fmt.Fprintf(b, "\t%s *sqlb.Collection[%s] `db:\"-\" json:%q sqlb:%q` // filled in by ?expand=%s\n",
				inv.field, inv.target, inv.relation+",omitempty", inv.tag, inv.relation)
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./codegen/... -run 'TestOneToOneInverse|TestNonUniqueInverse' -v`
Expected: PASS for both.

- [ ] **Step 6: Run the full codegen suite**

Run: `go test ./codegen/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add codegen/models.go codegen/codegen_test.go
git commit -m "$(cat <<'EOF'
feat(codegen): a one-to-one inverse generates a pointer, not a collection

inversesOf now reads InverseRelation.OneToOne and emits a bare *Target with
a `reverse` tag instead of *sqlb.Collection[Target] with order/limit —
matching the shape relation.go now knows how to resolve and join.
EOF
)"
```

---

### Task 4: TypeScript client codegen — `Target | null` instead of `Collection<Target>`

**Files:**
- Modify: `codegen/tsclient.go` (`tsRelation` struct ~line 252-259,
  `tsRelationOf` ~line 354-375, `tsRowTypes`'s inverse-relation emission
  ~line 405-434, `tsRelationType` ~line 1047-1055)
- Test: `codegen/tsclient_test.go` (extend near the existing inverse-relation
  assertions, ~line 170-189)

**Interfaces:**
- Consumes: `schema.InverseRelation.OneToOne` (Task 1) via `reg.Inverses(t)`,
  same as Task 3.
- Produces: nothing consumed elsewhere in this plan.

- [ ] **Step 1: Write the failing test**

Add to `codegen/tsclient_test.go`:

```go
func oneToOneTSFixture() *schema.Registry {
	r := schema.NewRegistry()
	users := r.Table("users", schema.UUIDv7("id").PrimaryKey())
	r.Table("profiles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("user", users).Unique().
			Expandable().Inverse("profile").InverseExpandable(),
	)
	return r
}

func TestTSOneToOneInverseIsNullableObjectNotCollection(t *testing.T) {
	files := generateTS(t, oneToOneTSFixture())
	client := files["client.gen.ts"]
	if !contains(client, `profile?: Profile | null;`) {
		t.Errorf("one-to-one inverse should type as Profile | null, got:\n%s", client)
	}
	if contains(client, "profile?: Collection<Profile>") {
		t.Errorf("one-to-one inverse must not use the Collection<T> envelope:\n%s", client)
	}
}

// Guard-proven-both-ways companion — ordinary inverse relations keep their
// existing Collection<T> shape.
func TestTSNonUniqueInverseStillUsesCollection(t *testing.T) {
	files := generateTS(t, tsFixture())
	client := files["client.gen.ts"]
	if !contains(client, "Collection<Post>") {
		t.Errorf("non-unique inverse should still use Collection<Post>, got:\n%s", client)
	}
}
```

- [ ] **Step 2: Run the tests to verify the first fails**

Run: `go test ./codegen/... -run 'TestTSOneToOneInverse|TestTSNonUniqueInverse' -v`
Expected: `TestTSOneToOneInverseIsNullableObjectNotCollection` FAILs.

- [ ] **Step 3: Add `oneToOne` to `tsRelation` and thread it through**

In `codegen/tsclient.go` (~line 252-259):

```go
type tsRelation struct {
	name     string // wire name, e.g. "list"
	target   string // TypeScript type of the expanded rows
	forward  bool   // a reference on this table, rather than one pointing at it
	nullable bool   // the reference column is nullable, so the relation may be null
	oneToOne bool   // an inverse relation backed by a unique FK — one row or null
}
```

In `tsRelationOf` (~line 354-375), set it on the inverse branch:

```go
	for _, inv := range reg.Inverses(t) {
		if inv.Expandable && inv.Name == name {
			return tsRelation{
				name:     name,
				target:   TypeName(inv.Table.LocalName()),
				oneToOne: inv.OneToOne,
			}, nil
		}
	}
```

- [ ] **Step 4: Branch `tsRelationType` on `oneToOne`**

Replace (~line 1047-1055):

```go
func tsRelationType(rel tsRelation) string {
	if !rel.forward {
		if rel.oneToOne {
			return rel.target + " | null"
		}
		return "Collection<" + rel.target + ">"
	}
	if rel.nullable {
		return rel.target + " | null"
	}
	return rel.target
}
```

- [ ] **Step 5: Stop hard-coding `Collection<T>` in the row-interface emitter**

In `tsRowTypes` (~line 405-434), the inverse-relation branch currently
bypasses `tsRelationType` entirely:

```go
	for _, inv := range reg.Inverses(t) {
		if !inv.Expandable {
			continue
		}
		fmt.Fprintf(b, "  /** Filled in by `expand: ['%s']`, absent otherwise. */\n", inv.Name)
		if inv.OneToOne {
			fmt.Fprintf(b, "  %s?: %s | null;\n", tsProp(inv.Name), TypeName(inv.Table.LocalName()))
			continue
		}
		fmt.Fprintf(b, "  /** Capped at %d rows. */\n", inv.Cap())
		fmt.Fprintf(b, "  %s?: Collection<%s>;\n", tsProp(inv.Name), TypeName(inv.Table.LocalName()))
	}
```

(This inlines the same logic `tsRelationType` now encodes, because this
emitter builds the doc comment differently per branch — the "capped at N
rows" line only makes sense for the collection case.)

`tsRowType`'s per-relation widening (~line 625-645) already calls
`tsRelationType(rel)` generically and needs no change — it will pick up the
new `| null` shape automatically once `rel.oneToOne` is populated per Step 3.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./codegen/... -run 'TestTSOneToOneInverse|TestTSNonUniqueInverse' -v`
Expected: PASS for both.

- [ ] **Step 7: Run the full codegen suite**

Run: `go test ./codegen/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add codegen/tsclient.go codegen/tsclient_test.go
git commit -m "$(cat <<'EOF'
feat(codegen): a one-to-one inverse types as Target | null in TypeScript

Mirrors the Go client change: tsRelation carries OneToOne from
InverseRelation, and both tsRelationType and the row-interface emitter stop
hard-coding Collection<T> for an inverse relation whose FK is unique.
EOF
)"
```

---

### Task 5: Dart client codegen — `Target?` instead of `Collection<Target>?`

**Files:**
- Modify: `codegen/dartclient.go` (`dartRelation` struct ~line 198-205,
  `dartRelationOf` ~line 288-313, the row-member emission loop ~line
  407-467, `dartForwardGetter`/`dartInverseGetter` ~line 595-603, the
  `${base}Expand` enum doc-string branch ~line 850-868)
- Test: `codegen/dartclient_test.go` (extend near ~line 169-182)

**Interfaces:**
- Consumes: `schema.InverseRelation.OneToOne` (Task 1) via `reg.Inverses(t)`.
- Produces: nothing consumed elsewhere in this plan.

- [ ] **Step 1: Write the failing test**

Add to `codegen/dartclient_test.go`:

```go
func TestDartOneToOneInverseIsNullableGetterNotCollection(t *testing.T) {
	src := generateDart(t, oneToOneFixture())
	if !contains(src, "Profile? get profile => _one('profile', Profile.fromJson);") {
		t.Errorf("one-to-one inverse should use _one(...), got:\n%s", src)
	}
	if contains(src, "Collection<Profile>? get profile") {
		t.Errorf("one-to-one inverse must not use the Collection<T> getter:\n%s", src)
	}
}

// Guard-proven-both-ways companion.
func TestDartNonUniqueInverseStillUsesCollectionGetter(t *testing.T) {
	src := generateDart(t, tsFixture())
	if !contains(src, "Collection<Post>? get posts => _many('posts', Post.fromJson);") {
		t.Errorf("non-unique inverse should still use _many(...), got:\n%s", src)
	}
}
```

(`oneToOneFixture` is the one added in Task 3's `codegen_test.go`, in the
same `codegen_test` package Dart's tests already share with TS's per the
existing `dartclient_test.go:43` pattern of reusing `tsFixture()`.)

- [ ] **Step 2: Run the tests to verify the first fails**

Run: `go test ./codegen/... -run 'TestDartOneToOneInverse|TestDartNonUniqueInverse' -v`
Expected: `TestDartOneToOneInverseIsNullableGetterNotCollection` FAILs.

- [ ] **Step 3: Add `oneToOne` to `dartRelation` and `dartRelationOf`**

In `codegen/dartclient.go` (~line 198-205):

```go
type dartRelation struct {
	name     string // wire name, e.g. "list"
	member   string // Dart getter, e.g. "list"
	target   string // Dart type of the expanded rows
	forward  bool   // a reference on this table, rather than one pointing at it
	oneToOne bool   // an inverse relation backed by a unique FK — one row or null
}
```

In `dartRelationOf` (~line 288-313), the inverse branch:

```go
	for _, inv := range reg.Inverses(t) {
		if inv.Expandable && inv.Name == name {
			return dartRelation{
				name:     name,
				member:   dartMember(name),
				target:   dartRowType(inv.Table),
				oneToOne: inv.OneToOne,
			}, nil
		}
	}
```

- [ ] **Step 4: Branch the row-member emission (~line 407-467)**

Replace the inverse-relation loop at the end of the member-building
function:

```go
	for _, inv := range reg.Inverses(t) {
		if !inv.Expandable {
			continue
		}
		member := dartMember(inv.Name)
		if err := claim(member, "inverse relation "+inv.Name); err != nil {
			return nil, err
		}
		if inv.OneToOne {
			out = append(out, dartRowMember{
				doc:    fmt.Sprintf("Filled in by expand: [%sExpand.%s], null otherwise.", base, member),
				getter: dartForwardGetter(dartRelation{name: inv.Name, member: member, target: dartRowType(inv.Table)}),
			})
			continue
		}
		out = append(out, dartRowMember{
			doc: fmt.Sprintf("Filled in by expand: [%sExpand.%s], null otherwise. Capped at %d rows;\nCollection.hasMore reports truncation.",
				base, member, inv.Cap()),
			getter: dartInverseGetter(inv.Name, member, dartRowType(inv.Table)),
		})
	}
```

`dartForwardGetter` (~line 595-598) already emits exactly the `_one(...)`
getter shape a one-to-one inverse needs — no change needed to that function,
only to which branch calls it.

- [ ] **Step 5: Fix the `${base}Expand` enum's doc-string branch (~line 850-868)**

The enum currently picks its doc string purely by `!rel.forward` (true for
every inverse relation, one-to-one or not):

```go
		for _, rel := range r.relations {
			kind := "The %s relation, one row."
			if !rel.forward && !rel.oneToOne {
				kind = "The %s relation, a capped collection."
			}
			docs = append(docs, fmt.Sprintf(kind, rel.name))
			names = append(names, rel.member)
			wires = append(wires, rel.name)
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./codegen/... -run 'TestDartOneToOneInverse|TestDartNonUniqueInverse' -v`
Expected: PASS for both.

- [ ] **Step 7: Run the full codegen suite**

Run: `go test ./codegen/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add codegen/dartclient.go codegen/dartclient_test.go
git commit -m "$(cat <<'EOF'
feat(codegen): a one-to-one inverse gets a nullable getter, not a Collection, in Dart

Mirrors the Go and TypeScript client changes: dartRelation carries
OneToOne, and both the row-member emitter and the Expand enum's doc string
stop treating every inverse relation as a capped collection.
EOF
)"
```

---

### Task 6: End-to-end verification — REST response shape, OpenAPI, restcompat

**Files:**
- Modify: `example/tasks/taskschema/schema.go` (add a one-to-one fixture:
  a `profiles` table with `Ref("user", ...).Unique()`, or reuse an existing
  users/accounts table if `example/tasks` already has one suited to this —
  read the file first to decide, since the plan should not invent a second
  parallel user concept if one already exists)
- Modify: `example/tasks/cmd/migrate/main.go` (add the migration for the new
  table, following the file's existing hand-composed `migrate.Migration`
  pattern noted in the design spec's research)
- Test: `example/tasks/app/expand_test.go` (new test, modeled on
  `TestExpandJoinsTheList` / `TestExpandCollectsTheTasksOfAList`,
  ~line 21-49 and ~189-263)
- Test: `restcompat/restcompat_test.go` (new test, modeled on
  `TestRenameIsAWireBreak`, ~line 117-125, using the existing `opts` struct
  and `blog()` helper, ~line 1-107)

**Interfaces:**
- Consumes: the full stack from Tasks 1-3 (schema declaration through Go
  codegen) — this task is where they're proven to work together against a
  real Postgres instance and a real generated OpenAPI document, not just
  proven independently.

- [ ] **Step 1: Regenerate `example/tasks`' schema-derived code**

Read `example/tasks/taskschema/schema.go` first to confirm the right place
to add a `Ref(...).Unique()` fixture (e.g. a `profiles` table under
`users`, if one exists, or the closest equivalent). Add the table, run the
project's own generate command (`mise run generate` or the project-local
equivalent named in `example/tasks/`'s own tooling), and confirm
`models_gen.go`/`rest_gen.go`/the TS and Dart clients under
`example/tasks/` regenerate with the new pointer-shaped field, using Task 3's
change.

- [ ] **Step 2: Write the migration**

Add the new table's migration to `example/tasks/cmd/migrate/main.go`,
following the file's existing pattern for hand-composed
`migrate.Migration{}` entries alongside generated ones.

- [ ] **Step 3: Write the failing REST test**

Add to `example/tasks/app/expand_test.go`:

```go
func TestExpandOfAOneToOneRelationIsAnObjectNotAnEnvelope(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	// ... create a profile row for alice's user, using whatever helper
	// pattern this file already uses for other fixture rows (see
	// alice.listID/alice.taskID above it in the same file).

	got := alice.get("/users?expand=profile").expect(http.StatusOK).list()
	if len(got.Items) != 1 {
		t.Fatalf("got %d users, want 1: %s", len(got.Items), mustJSON(got.Items))
	}

	profile, ok := got.Items[0]["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected profile to be a plain object, got %T: %s",
			got.Items[0]["profile"], mustJSON(got.Items[0]))
	}
	if _, hasEnvelope := profile["items"]; hasEnvelope {
		t.Errorf("a one-to-one expansion must not use the {items, has_more} envelope: %s", mustJSON(profile))
	}
}

func TestExpandOfAOneToOneRelationIsNullWhenAbsent(t *testing.T) {
	server := newServer(t, freshDB(t))
	alice := account(t, server, "alice@example.com", "Acme")
	// alice's user has no profile row created for this test.

	got := alice.get("/users?expand=profile").expect(http.StatusOK).list()
	if got.Items[0]["profile"] != nil {
		t.Errorf("expected profile to be null when absent, got: %s", mustJSON(got.Items[0]))
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

From `example/tasks/`: `mise run pg-up` (if not already running), then
`go test ./app/... -run TestExpandOfAOneToOne -v`
Expected: FAIL, or a compile/generation error if Step 1's regeneration
wasn't completed correctly — fix that first if so.

- [ ] **Step 5: Run the tests to verify they pass**

Same command as Step 4.
Expected: PASS. If not, the most likely gaps are: the fixture table/migration
from Steps 1-2, or a mismatch between what Task 2's `relation.go` change
expects in the `reverse` tag and what Task 3's codegen emits — re-check
those two tasks' Interfaces sections against each other.

- [ ] **Step 6: Write and run the `restcompat` test**

Add to `restcompat/restcompat_test.go`, using the file's existing `opts`
struct/`blog()` helper pattern (~line 1-107) — add an `authorUnique bool`
field to `opts`, thread it into the `authors`/`posts` fixture's `Ref(...)`
call (`.Unique()` when set), and assert the shape change:

```go
// A unique FK's Inverse changing shape from a collection envelope to a
// nullable object is a response-facet break, the same category a rename is
// — a client reading `.items` off it would break, just as one reading a
// renamed field would.
func TestUniqueFKChangesInverseFromCollectionToObject(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{authorUnique: true}))
	assertBreaking(t, breaks, restcompat.FacetExpand, "posts")
}
```

Run: `go test ./restcompat/... -run TestUniqueFKChangesInverseFromCollectionToObject -v`
Expected: PASS once the fixture/opts plumbing is in place — if `FacetExpand`
doesn't yet classify this specific change as breaking, that's a real gap:
extend `restcompat.Diff`'s expand-facet comparison to treat a
collection-to-object (or object-to-collection) shape change as breaking,
following the existing pattern for cap-delta and add/remove classification
cited in the file (~line 271, 287, 346).

- [ ] **Step 7: Run each affected module's full suite**

Run: `go test ./...` (repo root), then from `example/tasks/`: `go test ./...`
Expected: PASS in both.

- [ ] **Step 8: Commit**

```bash
git add example/tasks/ restcompat/restcompat_test.go
git commit -m "$(cat <<'EOF'
test(e2e): a unique FK's ?expand= returns an object or null, proven end to end

Adds a one-to-one fixture to example/tasks and asserts the REST response
shape directly, and adds a restcompat test proving the collection-to-object
shape change is classified as a breaking change on FacetExpand.
EOF
)"
```

---

### Task 7: Documentation — new ADR section, compatibility.md, release notes

**Files:**
- Modify: `docs/architecture.md` (append a new anchored ADR section,
  following the existing section format and anchor-slug convention —
  e.g. the "The driver is a dependency" section, `#the-driver-is-a-dependency`
  at line 2282, is the closest structural precedent for "a Frozen surface
  broken on purpose, with the reasoning recorded")
- Modify: `docs/compatibility.md` (carve-out note on the list-envelope Frozen
  entry, mirroring how the `Executor` entry documents its own prior break)
- Modify: `docs/releases.md` or the in-progress release notes location this
  repo uses next (check `docs/releases.md`'s structure first — this plan
  does not assume a specific unreleased-notes file exists yet)

**Interfaces:** none — this task has no code interface, only prose that
must accurately describe Tasks 1-6's actual shipped behavior. Do this task
last, after Tasks 1-6 are committed, so the prose describes what was
actually built rather than what was planned.

- [ ] **Step 1: Read the two closest precedent sections in full**

Read `docs/architecture.md`'s "The driver is a dependency" section in full
(anchor `#the-driver-is-a-dependency`, ~line 2282) and "References declare
their inverse" (anchor `#references-declare-their-inverse`, ~line 1182) —
the new section should match their tone and structure (Context / Decision /
Consequences / What would change our mind, or whatever heading pattern
those two sections actually use — confirm by reading, since the ADR-to-doc
consolidation may have changed the heading conventions from the historical
`docs/adr/*.md` template).

- [ ] **Step 2: Write the new ADR section**

Append a new `###`-level section to `docs/architecture.md`, in the same
file position/ordering convention the existing sections use (check whether
sections are ordered chronologically by decision date or thematically before
picking where to insert it). Content must cover, in the repo's own voice:
- What was frozen (the list envelope, for a unique-backed Inverse relation)
  and why breaking it now beats leaving the shape wrong until 1.0 — the same
  "pre-1.0-or-never" argument `#the-driver-is-a-dependency` makes.
- That `OneToOne` is derived from `Unique`, never declared as a separate
  verb, and why (a unique FK is structurally one-to-one; this isn't a
  capability opt-in like `Filterable`/`Sortable`).
- What it costs: existing generated clients reading `.items`/`.has_more` off
  a one-to-one relation (there should be none in this codebase's own
  examples after Task 6, but any external adopter who hand-declared
  `Ref().Unique()` before this change shipped would need to regenerate and
  fix call sites).

- [ ] **Step 3: Update `docs/compatibility.md`**

Add a carve-out note to the "The list envelope" Frozen entry, in the same
style the `Executor` entry uses to document its own prior break (quote its
"This entry broke once, deliberately, before 1.0" framing and adapt it).
Link to the new ADR section added in Step 2.

- [ ] **Step 4: Add the release-notes / mechanical-fix entry**

Read `docs/releases.md` to find the correct place for an unreleased or
next-version entry, and add one stating the mechanical fix: regenerate
Go/TypeScript/Dart clients; any code reading `.items`/`.has_more` (Go),
`.items`/`.hasMore` (TS/Dart) off a one-to-one (`Ref().Unique()`) relation's
expansion now reads the value directly (Go: nil-check the pointer; TS/Dart:
null-check).

- [ ] **Step 5: Run the docs check**

Run: `mise run site-check`
Expected: PASS — this catches a moved/renamed anchor or a broken link
introduced by the new section, per the "a docs link can break across two
green pull requests" trap `CLAUDE.md` names.

- [ ] **Step 6: Commit**

```bash
git add docs/architecture.md docs/compatibility.md docs/releases.md
git commit -m "$(cat <<'EOF'
docs(architecture): a one-to-one relation breaking the list envelope is recorded, not silent

New ADR section records the decision to break the Frozen list-envelope
guarantee for unique-backed Inverse relations, pre-1.0-or-never, following
the Executor precedent. compatibility.md and releases.md carry the same
decision forward as a carve-out and a mechanical fix, respectively.
EOF
)"
```

---

## Plan self-review notes

- **Spec coverage:** every "In scope" bullet in the design spec maps to a
  task — schema-layer inference (Task 1), REST/wire (Task 2 + verified in
  Task 6), OpenAPI (verified in Task 6, no direct code change needed per
  the REST research), Go/TS/Dart codegen (Tasks 3-5), the new ADR and
  compatibility carve-out (Task 7), and guard-proven-both-ways tests
  (present in every task).
- **Known research gap, called out rather than papered over:** Task 2 Step 1
  asks the implementer to locate the `relationTag` parser rather than
  quoting it verbatim — every other piece of code touched by this plan was
  pulled and verified from the actual repo, but this one parser's exact
  current text was not fetched during planning. It's a single, small,
  well-scoped lookup (grep + read one function) before the only step that
  depends on it, not an open design question.
- **Type/name consistency check:** `OneToOne` (schema/codegen field name),
  `Reverse` (root-package `RelationInfo` field name), and the `reverse` tag
  token (lowercase, as it appears literally in a struct tag string) are
  three different spellings of related-but-distinct concepts, used
  consistently within their own layer across Tasks 1-5 — verified by
  re-reading each task's Interfaces block against the ones before it.
