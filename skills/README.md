# Agent skills

Skills an agent can load when working on a project that uses sqlb. These are the
**static** half — the part that is true in every repository.

The project-specific half is not here, because it cannot be: which columns your
resources accept is different in every project, so it is **generated**. Set
`Options.SkillDir` and `sqlb generate` writes
`<SkillDir>/sqlb-schema/SKILL.md` — the tables, the mounted paths, what each
resource may be filtered, sorted, searched and expanded on, the declared verbs,
and the obligations the schema carries. `sqlb check` names it when it has
drifted, which is the only reason writing instructions into a repository is
safe. [`example/tasks`](../example/tasks/.claude/skills/sqlb-schema/SKILL.md) has
one committed. [ADR-0049](../docs/architecture.md#the-skill-is-generated) is the
argument.

A repository with several registries wants one `SkillDir` per registry, placed
beside the module it describes: a nested `.claude/skills` is directory-scoped, so
sixteen skills all named `sqlb-schema` are sixteen correctly-scoped skills rather
than a collision. To share one directory instead — the repository root, so every
skill is offered from the first turn — give each registry its own
`Options.SkillName`.

| Skill | Covers |
|---|---|
| [`sqlb-authoring`](sqlb-authoring/SKILL.md) | The DSL's own vocabulary for *writing* a schema: column types, capability flags (`Filterable`/`Sortable`/`Scoped`/`Hidden`/…), predicates, hooks and the escape hatches around each — checked against the DSL by `skill-check`, not tied to any one project's schema |
| [`sqlb-queries`](sqlb-queries/SKILL.md) | Where the builder ends and `Raw`, sqlc or hand-written SQL begins — plus four failure modes that compile, pass their tests, and are wrong at runtime |
| [`sqlb-adoption`](sqlb-adoption/SKILL.md) | Whether an existing codebase should adopt sqlb at all: a five-step census producing a ratio and a pilot, with the two stop conditions that end the evaluation early |

They answer different questions and none subsumes another: `sqlb-adoption`
runs once per codebase and mostly decides *whether*; `sqlb-authoring` applies
when someone is declaring or changing a table; `sqlb-queries` applies every
time someone writes a query afterwards. `sqlb-authoring` is also the
hand-maintained sibling of the *generated* `sqlb-schema` skill below — that one
says which columns *this project's* schema actually declared capabilities on,
this one says what the capability vocabulary is in the first place. Load the
generated one for "can I filter `tasks.author_id`" and this one for "does
`Filterable` exist" or "what does `Scoped` enforce".

The generated skill says so itself, in a "Where the rest of it is" section that
names all three of these and how to install them. That is deliberate placement
rather than politeness: it is the only sqlb artefact guaranteed to be in a
consumer's repository and in front of an agent from the first turn, and while it
named none of them, a consumer finished an entire port making mistakes
`sqlb-authoring` covers by name without learning it existed (#291). The list is
generated from `codegen/skill.go` and checked against this directory, so a skill
added or renamed here cannot leave every consumer's repository advertising one
that is not there.

## Installing

The ecosystem CLI ([`skills`](https://github.com/vercel-labs/skills)) takes an
`owner/repo` shorthand and places files where each agent tool expects them:

```bash
npx skills add mind-vm/sqlb
```

One skill by name, rather than all of them:

```bash
npx skills add mind-vm/sqlb --skill sqlb-queries
```

Project scope is the default, which is what you want — the skill lands in the
repository, so the team and any cloud agents share it. `--global` installs across
all projects instead.

Note that `npx` here is *your* invocation, not part of sqlb's build. Nothing in
this repository depends on Node, and adding a skill does not change that.

**Or just copy it.** A skill is a directory with a `SKILL.md` in it, so for
Claude Code:

```bash
mkdir -p .claude/skills && cp -r skills/sqlb-queries .claude/skills/
```

## Why these are written down at all

This repository prefers a failing check to a documented rule — `generate-check`,
`eject-check`, `impact-check` and the rest exist because a convention that is
only written down drifts. A skill is a written-down rule, so it needs the
argument.

`sqlb-queries` is the case where no check is possible. When a query reaches past
the row, the wrong code compiles, passes its tests, and answers the request:
an aggregate over an empty set scans `NULL` into an `int64`, a day filter against
`timestamptz` matches nothing and returns no error, `OnConflictDoNothing` makes a
retried payment arrive as "not found". Nothing in CI will catch those, which is
why they are written down rather than gated.

`sqlb-adoption` is a different exception: it is about a codebase sqlb has not
been told about yet, so there is nothing in *this* repository for a check to run
against. Its failure mode is not a wrong number but a missing stop condition —
an evaluation that reports "sqlb replaces the API" when the honest answer is
"the least novel third of it", or that surveys the routes before finding out the
tables are blocked.

`sqlb-authoring` is the third case, and it used to argue differently than the
other two: a check *could* in principle enumerate `Field`'s methods, this said,
but the DSL's vocabulary changes at the rate of a minor release rather than a
schema edit, so a hand-written document is safe here — nothing in it is a fact
any particular registry could get out of sync with.

That argument was wrong, and #293 said why before the evidence arrived: prose
duplicating something the source already enumerates is the worst of both,
because it is the copy that can be wrong. It was. Almost every line number in
that file pointed somewhere else — `Filterable()` at a blank line, `Hidden()` at
`Field.Comment` — twenty-one of thirty-nine `*Field` methods were missing while
the file's own description claimed the DSL's whole surface, and two "not
expressible" entries had shipped in the meantime (`sqlb.Near` for a vector
column, `AddExclude` for an exclusion constraint). A minor-release cadence is
slow enough to feel safe and fast enough to be wrong within one.

So `mise run skill-check` now enumerates `Field`'s methods after all. Forward: a
method the DSL declares and the skill does not name fails the gate, unless it is
in the accessors list, which makes the omission a decision. Reverse: a table row
naming something the DSL does not declare fails too. The line numbers are gone —
they rot on any edit and tell a reader nothing they could not get by grepping
the name — so what is cited is a file and a symbol, which is what the check can
actually verify. See "Keeping it honest".

## Keeping it honest

Every code sample in `sqlb-queries/SKILL.md` was compiled and rendered against
the tree, not written from memory — which caught three wrong signatures and one
stale claim in the process. The traps carry their evidence: the `timestamptz`
one is asserted by `pgtest/census_test.go`, and that test fails loudly if the
missing cast is ever added, so the skill's claim and the code cannot silently
disagree.

Every method, flag and line reference in `sqlb-authoring/SKILL.md` was checked
against the source at the time it was written — `field.go`, `expr.go`,
`hooks.go`, `db.go`, `registry.go`, `rest/scope.go` and `rest/rest.go` line
numbers included — rather than described from memory. A renumbered file makes
an entry wrong in a way nothing here catches automatically; treat a stale
line number as a signal to recheck the claim next to it, not just the number.

Every shell command in `sqlb-adoption/SKILL.md` was run against synthetic
fixtures on BSD awk, which is what the stated platform has, and its `sqlb survey`
flags were checked against the command's own help rather than the prose.

`docs/special-cases.md` is the measured census these boundaries come from, and it
says of itself that its status column will rot. It has: two rows have closed
since it was written. Prefer this skill for *behaviour*, the census for
*proportion* — how much of a real corpus each shape accounts for.
