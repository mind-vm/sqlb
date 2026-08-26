# site

The documentation site: [Astro](https://astro.build) with
[Starlight](https://starlight.astro.build).

```bash
mise run site-dev      # serve with live reload
mise run site-build    # build into site/dist, then check every link resolves
mise run site-check    # can the docs be published as they stand? (no npm install)
```

## Where the content lives

**The markdown under `docs/` is the source of truth.** It is plain markdown with
no frontmatter, so it renders on GitHub and is readable in a checkout — which is
where most people will meet it. `scripts/sync-docs.mjs` derives the Starlight
collection from it on every build, one route per source:

| Source | Route | Contents |
|---|---|---|
| `docs/start/` | `/start/` | Overview, quickstart, a worked first app, structs-first adoption |
| `docs/concepts/` | `/concepts/` | The five ideas the rest of it rests on |
| `docs/schema/` | `/schema/` | Declaring tables, capabilities, references |
| `docs/queries/` | `/queries/` | Queries, typed columns, paging, mutations, hooks, inspecting |
| `docs/rest/` | `/rest/` | Mounting, filtering, pagination, expansion, rejections |
| `docs/typescript/` | `/typescript/` | The generated client |
| `docs/cli/` | `/cli/` | The generated cobra tree |
| `docs/migrations/` | `/migrations/` | Diffing, rollout, adopting a database |
| `docs/` | `/project/` | A named list: vision, architecture (including its Decisions section), compatibility, comparisons, with-sqlc, the adoption review |

Every section but the last two names its own reading order in `SOURCES`, so a
new page has to be placed deliberately and one missing from the list is
reported rather than silently appended. The last is a named list rather than a
glob so that a new file in `docs/` is not published by accident — what belongs
on the site is a decision each time.

Two directories under `src/content/docs/` are **hand-written** and are not
generated from anything:

| | |
|---|---|
| `index.mdx` | The landing page |
| `examples/` | The gallery and one page per example. Four of the six examples live outside this repository, so those pages describe rather than link |
| `reference/` | Lookup tables — filter operators, column types, capabilities, codegen options, the CLI, rejections |

They are MDX because they are site-shaped rather than checkout-shaped: card
grids, and tables about a repository the reader is not in. Everything that reads
well in a checkout stays plain markdown under `docs/`.

Adding a generated section is an entry in `SOURCES` plus a sidebar group in
`astro.config.mjs` — and `sync-docs.mjs` fails if you do one without the other.
Adding a hand-written one is a directory, a sidebar group, and an entry in that
script's `HAND_WRITTEN` set.

That means:

- **Edit the markdown under `docs/`.** The matching directory under
  `site/src/content/docs/` is generated and gitignored. The "Edit this page"
  link on the site points at the source, not at the copy.
- **Nothing generated is committed**, so there is no drift to gate against. The
  pages are rebuilt from scratch each time, which is also how a page deleted from
  the source leaves the site. Each generated directory carries a `.gitignore`
  written by the script that owns it, so there is no list of them to maintain
  in `site/.gitignore`.

## What the scripts guard

Neither script is a formality; each one has a failure it exists to catch, and
both were checked to fail before being relied on.

**`sync-docs.mjs`** rewrites repo-relative links into web links by *resolving*
them, not by matching patterns. The same target is written differently depending
on where the file sits — `architecture.md` from `docs/`, `../architecture.md`
from `docs/queries/` — so each link is resolved against the repository and
then looked up:

- **published here** → an internal route, which is why a cross-reference to
  one of architecture.md's decisions stays on the site instead of leaving for
  GitHub;
- **exists in the repo** → a GitHub URL, file or directory as appropriate;
- **neither** → a **hard error**.

That last case earns its keep twice: it catches a link that would 404 after
deploy, and a link to a repository file that is simply not there. It also fails
on a page with no H1 to take a title from, and on a page missing from its
section's `sequence` list, so adding one is a deliberate act here too.

It skips inline code as well as fenced blocks, which is not fussiness:
compatibility.md contains `OnIn[T](r)`, Go generics that are indistinguishable
from a markdown link to a pass that only skips fences.

It also **pairs every route with a sidebar group**, in both directions. The
pages of a section left out of `astro.config.mjs` still build and are still
cross-linked from the rest of the guide, so nothing downstream notices that a
whole section is missing from the navigation — proven by deleting the Migrations
group and watching the build stay green until this check existed.

**`check-links.mjs`** reads the built HTML and verifies every internal link
resolves to a file that exists. This is the one that matters, because its
failure mode is invisible locally: links are rewritten *and* the deployment sits
under a base path, so a link can be well-formed markdown, survive the build, and
still break in production. It caught exactly that during setup, twice.

It then reports any page **linked from nowhere at all** — the hand-written
directory nobody wired up. That is the narrow claim and the true one: a section
merely missing from the sidebar is still cross-linked from prose, which is why
that case is caught at the source instead, above.

## Deployment

Nothing deploys at present — see the workflow section below. What follows is
what the configuration is set up for, and is what a return to publishing would
use rather than re-derive.

`site.config.mjs` holds `site` and `base`. `astro.config.mjs` configures Astro
from it, `sync-docs.mjs` prefixes generated links with it, and Starlight prefixes
its own navigation automatically. It is currently set for a GitHub Pages project
site at `/sqlb`.

Moving to a custom domain or a user page means setting `base` to `"/"` there —
and also editing every link written out in full in the hand-written pages: the
hero `link:` values in `src/content/docs/index.mdx`, which Starlight does not
apply the base to because they are frontmatter, and the internal links in
`examples/` and `reference/`, which are markdown rather than navigation.
Those are the places the base is repeated, so they are the places that can drift.
Do not try to hold it in your head: change `base`, run `mise run site-build`, and
the link check names every href that no longer resolves. It found exactly this
when the base was first switched to `"/"` as a test.

`.github/workflows/site.yml` builds on a push to `main` or a pull request that
touches `site/**` or `docs/**`, and on `workflow_dispatch`. It runs
`npm run build`, so the link check is what it is for: a link that resolves to
nothing fails there.

It used to deploy to GitHub Pages, and does not any more — the repository is
private, and a private repository on this plan cannot serve Pages, so the
deploy step answered 404 on every push and made the workflow permanently red.
The build survives the deployment because the build was always the half that
caught anything. Nothing here is wasted if Pages comes back: the deploy job was
six lines, and `site.config.mjs` still holds the URL it would publish under.

It is deliberately separate from `ci`. The site is Astro, so folding it into the
gate would make Node a build dependency of a Go library whose whole argument is
that it imposes none — and a red site build should not be able to say the
library is broken.

The path filter is the whole of `docs/`, not the published subset. It listed
`docs/guide/**` when the guide was the only source and silently outgrew that:
an ADR was committed, CI was green, and the page 404d because no deploy ran.
Building on an unpublished `docs/` change is cheap; not building on a published
one is invisible.

`mise run site-check` is the same link check without `npm ci`, and is what to
run locally — see CLAUDE.md. The workflow is the version that also compiles the
Astro site, which is the part `site-check` cannot do.
