// Derive the Starlight content collection from the markdown in docs/.
//
// The files under docs/ are the source of truth: plain markdown, no frontmatter,
// rendering on GitHub, which is where most people meet them. This turns them
// into what Starlight wants — frontmatter, web paths instead of file paths —
// without a second copy for anyone to edit. Nothing written here is committed;
// it is regenerated on every build, so there is no drift to gate.
//
// What it does gate is links, and it does so by resolving them rather than by
// matching patterns. A relative link means something different depending on
// which directory the file sits in — `architecture.md` from docs/,
// `../architecture.md` from docs/queries/ name the same page — so each
// link is resolved against the repository and then looked up:
//
//   published here    → an internal route
//   exists in the repo → a GitHub URL, since the site has no page for it
//   neither            → a hard error
//
// The last case is the one worth having. It catches a link that 404s after
// deploy, and also a link to a repository file that simply is not there.
// `--check` runs the transform and reports without writing.

import { existsSync, statSync } from "node:fs";
import { mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join, posix, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { base } from "../site.config.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "../..");
const contentRoot = join(here, "../src/content/docs");

// Trailing slash stripped so `${prefix}/start/...` is well-formed for base "/".
const prefix = base.replace(/\/$/, "");

const GITHUB = "https://github.com/mind-vm/sqlb";
const BLOB = `${GITHUB}/blob/main`;
const TREE = `${GITHUB}/tree/main`;

/**
 * Each source becomes one route on the site.
 *
 * `files` restricts a directory to named files, for docs/ where only some of the
 * loose markdown is published. `order` returns a sidebar position and `label`
 * the sidebar text when it should differ from the page's H1; both take the
 * page's slug — the filename without .md, or "index" for a README.
 */
const SOURCES = [
  {
    dir: "docs/start",
    route: "start",
    // Explicit everywhere below, because each section is a reading order rather
    // than a list: a new page should be placed deliberately, and one missing
    // from here is reported rather than silently appended at the end.
    sequence: ["index", "quickstart", "first-app", "structs-first"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
  },
  {
    dir: "docs/concepts",
    route: "concepts",
    sequence: [
      "index",
      "queries-are-values",
      "one-grammar",
      "capabilities",
      "domain-logic",
      "generated-not-hidden",
    ],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
  },
  {
    // One page, and deliberately no more. Every recipe it names already lives
    // in the section that owns its subject, because that is where someone
    // reading about migrations should find the rollout page. What is missing
    // without this is the other reader: the one who arrives with a task rather
    // than a subject, and cannot know which of seven surfaces owns it. Django's
    // howto/ is the same idea with the pages moved; moving them here would cost
    // each section its reading order to buy one index.
    dir: "docs/howto",
    route: "howto",
    sequence: ["index"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "By task" : null),
  },
  {
    // One page, like the three client sections below: the whole surface
    // compressed into a lookup table, for a reader who knows what they are
    // building and needs the spelling. It sits after the concepts and before
    // the sections it indexes, because it is the fastest route into any of
    // them and the slowest route into understanding why they are shaped that
    // way.
    dir: "docs/cheatsheet",
    route: "cheatsheet",
    sequence: ["index"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
  {
    dir: "docs/schema",
    route: "schema",
    sequence: ["index", "capabilities", "references", "libraries"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
  },
  {
    dir: "docs/queries",
    route: "queries",
    sequence: ["index", "typed-columns", "paging", "mutations", "hooks", "inspecting", "testing"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
  },
  {
    dir: "docs/rest",
    route: "rest",
    sequence: ["index", "filtering", "pagination", "expand", "actions", "auth", "admin", "events", "webhooks", "errors", "compatibility"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
  },
  {
    dir: "docs/typescript",
    route: "typescript",
    sequence: ["index"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    // "# TypeScript client" is the right heading on GitHub, where the file is
    // read on its own, and reads as "TypeScript SDK › TypeScript client" in a
    // sidebar whose group already says so. Same for the two below.
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
  {
    dir: "docs/dart",
    route: "dart",
    sequence: ["index"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
  {
    dir: "docs/cli",
    route: "cli",
    sequence: ["index"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
  {
    dir: "docs/migrations",
    route: "migrations",
    // refactoring-a-database.md sits here rather than under /project/ because
    // it is the narrative half of this section: index documents the API, and it
    // documents what an actual schema change costs. It goes last because it
    // assumes the other three.
    sequence: ["index", "rollout", "adopting", "refactoring-a-database"],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Diffing and rendering" : null),
  },
  {
    dir: "docs",
    route: "project",
    // Named rather than globbed, so a new file in docs/ is not published by
    // accident — what belongs on the site is a decision each time.
    //
    // Read in this order: what it is for, what it believes, how it is built,
    // what it promises,
    // how to leave, what it has shipped, what it has to do before that promise
    // becomes permanent, how it sits beside sqlc, how to move one endpoint
    // across, how many endpoints there are to move, and what an outside reader
    // made of it. Those three run consecutively because they are one argument
    // in three parts: with-sqlc.md says which queries belong on which side,
    // refactoring-from-sqlc.md says what moving one costs, and
    // surveying-a-codebase.md multiplies the second by the first over a whole
    // repository. The review is a dated snapshot and says so in its own first
    // paragraph, which is what makes it publishable rather than misleading.
    //
    // The exit sits directly after the compatibility promise on purpose: the
    // two answer the same reader's question, one page apart. The releases
    // follow, because compatibility.md promises that each break is described in
    // the release notes and that page is where they are.
    files: [
      "vision.md",
      // Directly after the vision because it is the same argument one level
      // down: vision.md says what sqlb is for, and this says what it believes
      // about schema and API design -- and, per rule, whether the DSL enforces
      // that belief or merely recommends it. Django files the equivalent under
      // its project section as "Design philosophies" for the same reason.
      "best-practices.md",
      "architecture.md",
      "compatibility.md",
      "eject.md",
      "releases.md",
      "release-1.0.md",
      "comparisons.md",
      "with-sqlc.md",
      "refactoring-from-sqlc.md",
      "surveying-a-codebase.md",
      // After the sqlc trio because it is the same kind of page one platform
      // over: how sqlb sits beside something a project already has. It is the
      // only one of these that is also an operational document — which
      // connection each component needs, what the shadow database has to be —
      // so it publishes rather than staying a checkout-only note.
      "supabase.md",
      "review-adoption-readiness.md",
    ],
    order(slug) {
      return this.files.indexOf(`${slug}.md`);
    },
  },
  {
    // Hand-written MDX until the site was deleted; the pages were folded into
    // docs/ as plain markdown at that point, so they generate from there now
    // like every other section. That is strictly better than restoring the
    // MDX: one copy, and it reads in a checkout too.
    dir: "docs/examples",
    route: "examples",
    sequence: [
      "index",
      "blog",
      "tasks",
      "fxapp",
      "library",
      "library-sqlc-chi",
      "library-sqlc-gin",
      "exchange",
    ],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
  {
    // Same story as examples/ above.
    dir: "docs/reference",
    route: "reference",
    sequence: [
      "index",
      "filter-grammar",
      "column-types",
      "capabilities",
      "codegen",
      "cli",
      "rejections",
      "glossary",
    ],
    order(slug) {
      return this.sequence.indexOf(slug);
    },
    label: (slug) => (slug === "index" ? "Overview" : null),
  },
];

const check = process.argv.includes("--check");

/**
 * Split markdown into code and prose runs, so rewrites never touch code.
 *
 * Both fenced blocks and inline spans count. The inline case is not
 * hypothetical: ADR-0020 contains `sqlb.QueryIn[T](tx)` and compatibility.md
 * contains `OnIn[T](r)` — Go generics that a link-shaped regex reads as links.
 */
function splitCode(md) {
  const parts = [];
  const fence = /^(```|~~~).*$/gm;
  let index = 0;
  let open = null;
  let match;
  while ((match = fence.exec(md)) !== null) {
    if (open === null) {
      parts.push({ code: false, text: md.slice(index, match.index) });
      open = match[1];
      index = match.index;
    } else if (match[0].startsWith(open)) {
      const end = match.index + match[0].length;
      parts.push({ code: true, text: md.slice(index, end) });
      open = null;
      index = end;
    }
  }
  parts.push({ code: open !== null, text: md.slice(index) });

  // Split the prose runs again, on inline code spans.
  const out = [];
  for (const part of parts) {
    if (part.code) {
      out.push(part);
      continue;
    }
    for (const piece of part.text.split(/(`+[^`]*`+)/)) {
      if (piece !== "") out.push({ code: piece.startsWith("`"), text: piece });
    }
  }
  return out;
}

/** Route for a page, given its source and slug. */
function routeFor(source, slug) {
  return slug === "index" ? `${prefix}/${source.route}/` : `${prefix}/${source.route}/${slug}/`;
}

/** List the markdown a source publishes, honouring an explicit file list. */
async function filesOf(source) {
  if (source.files) return source.files;
  return (await readdir(join(repo, source.dir))).filter((f) => f.endsWith(".md")).sort();
}

/**
 * Map every published file to its route, keyed by repo-relative path. A
 * directory holding a README maps too, so a link to the directory reaches its
 * index.
 */
async function buildRouteIndex() {
  const routes = new Map();
  for (const source of SOURCES) {
    for (const file of await filesOf(source)) {
      const slug = file === "README.md" ? "index" : file.replace(/\.md$/, "");
      routes.set(posix.join(source.dir, file), routeFor(source, slug));
      if (slug === "index") routes.set(source.dir, routeFor(source, "index"));
    }
  }
  return routes;
}

/**
 * Resolve one link as written, from a file in `fromDir`, to its web form.
 * Returns null when the target cannot be found at all, which is the error case.
 */
function rewrite(link, fromDir, routes) {
  if (link.startsWith("#")) return link;
  if (/^(https?:|mailto:)/.test(link)) return link;

  const [rawPath, fragment] = link.split("#");
  const hash = fragment ? `#${fragment}` : "";

  // Resolve against the containing directory, then key on the repo-relative
  // path — the same target written three different ways lands on one key.
  const resolved = posix.normalize(posix.join(fromDir, rawPath)).replace(/\/$/, "");
  if (resolved.startsWith("..")) return null; // outside the repository

  const route = routes.get(resolved);
  if (route) return route + hash;

  // Not published, but real: link out to the repository.
  const onDisk = join(repo, resolved);
  if (existsSync(onDisk)) {
    const root = statSync(onDisk).isDirectory() ? TREE : BLOB;
    return `${root}/${resolved}${hash}`;
  }

  return null;
}

/** First H1 becomes the title; the H1 itself is dropped, Starlight renders it. */
function extractTitle(md) {
  const m = /^#\s+(.+)$/m.exec(md);
  if (!m) return null;
  return { title: m[1].trim(), body: md.replace(m[0], "").trimStart() };
}

/** First prose paragraph, flattened, for the page description. */
function extractDescription(body) {
  for (const block of body.split(/\n\s*\n/)) {
    const text = block.trim();
    if (!text || /^[#|>`-]/.test(text)) continue;
    return text
      .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1")
      .replace(/[*`_]/g, "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 300);
  }
  return null;
}

async function transform(source, routes, problems) {
  const files = await filesOf(source);
  if (files.length === 0) throw new Error(`sync-docs: no markdown found in ${source.dir}`);

  const pages = [];
  for (const file of files) {
    const path = join(repo, source.dir, file);
    if (!existsSync(path)) {
      problems.push(`${source.dir}/${file}: listed in SOURCES but not on disk`);
      continue;
    }
    const raw = await readFile(path, "utf8");
    const extracted = extractTitle(raw);
    if (!extracted) {
      problems.push(`${source.dir}/${file}: no H1, so the page has no title`);
      continue;
    }
    const { title, body } = extracted;
    const slug = file === "README.md" ? "index" : file.replace(/\.md$/, "");

    let rewritten = "";
    for (const part of splitCode(body)) {
      if (part.code) {
        rewritten += part.text;
        continue;
      }
      rewritten += part.text.replace(/\]\(([^)\s]+)\)/g, (whole, link) => {
        const next = rewrite(link, source.dir, routes);
        if (next === null) {
          problems.push(`${source.dir}/${file}: ${link} resolves to nothing in the repository`);
          return whole;
        }
        return `](${next})`;
      });
    }

    const order = source.order(slug);
    if (order < 0) {
      problems.push(`${source.dir}/${file}: no sidebar position, so its order is undefined`);
    }

    const label = source.label?.(slug, title) ?? null;
    const description = extractDescription(rewritten);
    // Per-page, because Starlight would otherwise derive it from the generated
    // file's location and send "edit this page" at the copy.
    const editUrl = `${GITHUB}/edit/main/${source.dir}/${file}`;

    const frontmatter = [
      "---",
      `title: ${JSON.stringify(title)}`,
      description ? `description: ${JSON.stringify(description)}` : null,
      `editUrl: ${JSON.stringify(editUrl)}`,
      "sidebar:",
      label ? `  label: ${JSON.stringify(label)}` : null,
      `  order: ${order}`,
      "---",
      "",
      "",
    ]
      .filter((line) => line !== null)
      .join("\n");

    pages.push({
      path: join(contentRoot, source.route, `${slug}.md`),
      body: frontmatter + rewritten,
    });
  }
  return pages;
}

/**
 * Every route must have a sidebar group, and every sidebar group a route.
 *
 * The pages of an unlisted section still build and are still cross-linked from
 * the guide, so `check-links` sees nothing wrong — a whole section can be
 * invisible in the navigation and nothing downstream notices. The two lists are
 * in two files by necessity (one is Astro's config), so the pairing is checked
 * here, where both are knowable.
 *
 * A regex over the config rather than an import: astro.config.mjs imports
 * `astro/config`, which is not resolvable from a bare `node scripts/...` run,
 * and `site-check` is specified to need no npm install.
 */
async function checkSidebar(problems) {
  const config = await readFile(join(here, "../astro.config.mjs"), "utf8");
  const grouped = new Set(
    [...config.matchAll(/autogenerate:\s*{\s*directory:\s*"([^"]+)"/g)].map((m) => m[1]),
  );

  for (const source of SOURCES) {
    if (!grouped.has(source.route)) {
      problems.push(
        `${source.dir}: publishes /${source.route}/ with no sidebar group, so the whole section is unreachable from the navigation`,
      );
    }
  }
  // The other direction catches a group left behind by a renamed or deleted
  // source, which Starlight reports as an empty group rather than an error.
  const routes = new Set(SOURCES.map((s) => s.route));
  for (const dir of grouped) {
    if (!routes.has(dir) && !HAND_WRITTEN.has(dir)) {
      problems.push(`astro.config.mjs: sidebar group "${dir}" has no source in SOURCES and is not hand-written`);
    }
  }
}

/**
 * Every loose markdown file at the root of docs/ is either published or listed
 * as deliberately not.
 *
 * The `files` list above already says publishing is a decision per file. What it
 * did not say is anything about the files it leaves out, and docs/ had drifted
 * into holding both — published prose beside dated review notes and working
 * artefacts, with nothing to tell a reader or a writer which was which. A new
 * file was silently unpublished, which is the wrong default for a directory
 * whose stated purpose is prose for humans.
 *
 * So the two lists have to partition the directory: a file in neither is an
 * error that names both options. Subdirectories are out of scope on purpose —
 * each is a whole section with its own SOURCES entry, and docs/superpowers/
 * holds specs for unshipped work, which is working material by definition and
 * is deleted rather than published once the work lands (see its README).
 */
const UNPUBLISHED = new Map([
  // Dated snapshots of one reader's attempt to adopt sqlb, kept because the
  // tests and examples they produced cite them by path -- pgtest/pgtype_test.go
  // names one, and a dozen example READMEs name the census. They are working
  // notes rather than documentation: no reading order, no upkeep, and true only
  // of the day they were written.
  ["review-2026-07-28.md", "dated adoption snapshot, cited by the work it produced"],
  ["review-2026-07-31-external.md", "dated outside read, superseded by review-adoption-readiness.md"],
  ["review-adoption-existing-app.md", "dated adoption snapshot"],
  ["review-adoption-multi-app.md", "dated adoption snapshot"],
  ["review-adoption-port.md", "dated adoption snapshot, cited by pgtest/pgtype_test.go"],
  ["review-adoption-port-multi-app.md", "dated adoption snapshot"],
  // The census of cases the examples exist to settle. Cited by name from
  // example/*/README.md and from the pgtest suites, so it is load-bearing --
  // but it argues about which examples to write, which is a question for
  // whoever writes them rather than for anyone using sqlb.
  ["special-cases.md", "census of cases for the example suite, cited from example/ and pgtest/"],
  ["special-cases-subject-go.md", "the same census for one subject, cited from pgtest/"],
  ["codebase-review-2026-08-02.md", "dated main-branch review snapshot, true only of the revision it names"],
  ["django-orm-comparison-2026-08-15.md", "dated capability comparison from a point-in-time discussion, not upkept documentation"],
  ["auth-support-2026-08-21.md", "point-in-time report on the auth seam, not upkept documentation"],
]);

async function checkDocsRoot(problems) {
  const project = SOURCES.find((s) => s.dir === "docs");
  const published = new Set(project.files);
  const onDisk = (await readdir(join(repo, "docs"))).filter((f) => f.endsWith(".md"));

  for (const file of onDisk) {
    if (published.has(file) || UNPUBLISHED.has(file)) continue;
    problems.push(
      `docs/${file}: neither published nor listed as deliberately unpublished — ` +
        `add it to the "docs" source's files list, or to UNPUBLISHED with a reason`,
    );
  }
  // The other direction: a reason left behind by a file that was published or
  // deleted, which would otherwise sit there describing nothing.
  for (const file of UNPUBLISHED.keys()) {
    if (published.has(file)) {
      problems.push(`docs/${file}: listed in UNPUBLISHED and also published`);
    } else if (!onDisk.includes(file)) {
      problems.push(`docs/${file}: listed in UNPUBLISHED but not on disk`);
    }
  }
}

/** Directories under src/content/docs that are written by hand, not generated. */
/**
 * Directories under src/content/docs that are written by hand, not generated.
 * Empty: examples/ and reference/ were the last two, and they now generate
 * from docs/. Only index.mdx (a file, not a directory) is still hand-written.
 */
const HAND_WRITTEN = new Set();

async function main() {
  const routes = await buildRouteIndex();
  const problems = [];
  const bySource = [];
  for (const source of SOURCES) {
    bySource.push({ source, pages: await transform(source, routes, problems) });
  }
  await checkSidebar(problems);
  await checkDocsRoot(problems);

  if (problems.length > 0) {
    console.error("sync-docs: the docs cannot be published as they stand:");
    for (const p of problems) console.error(`  ${p}`);
    console.error("\nFix the link, publish its target by adding it to SOURCES, give the section a sidebar group, or say why a file stays unpublished.");
    process.exit(1);
  }

  const total = bySource.reduce((n, { pages }) => n + pages.length, 0);
  if (check) {
    for (const { source, pages } of bySource) {
      console.log(`  ${source.dir} → /${source.route}/  ${pages.length} pages`);
    }
    console.log(`sync-docs: ${total} pages transform cleanly, every link resolves`);
    return;
  }

  for (const { source, pages } of bySource) {
    const dir = join(contentRoot, source.route);
    // Rebuilt from scratch, so a page deleted from docs/ leaves the site too.
    await rm(dir, { recursive: true, force: true });
    await mkdir(dir, { recursive: true });
    // Each generated directory ignores itself. That is one line per route
    // written by the code that owns the route, rather than a list in
    // site/.gitignore that has to be remembered when SOURCES grows — and it
    // leaves the hand-written directories beside it committable with no
    // exception rule to maintain.
    await writeFile(join(dir, ".gitignore"), "# Generated by scripts/sync-docs.mjs. Edit " + source.dir + " instead.\n*\n");
    for (const { path, body } of pages) await writeFile(path, body);
    console.log(`sync-docs: wrote ${pages.length} pages to src/content/docs/${source.route}/`);
  }
}

await main();
