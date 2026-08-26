// Verify every internal link in the built site resolves to a page that exists.
//
// This is the guard that matters, because the failure it catches is invisible
// locally. The guide's links are rewritten from repo paths to web paths by
// sync-docs.mjs, and the deployment lives under a base path — so a link can be
// well-formed markdown, survive the build, and still 404 once deployed. Reading
// the built HTML is the only check that sees what a visitor sees.
//
// It is separate from sync-docs on purpose: that script gates the transform, this
// one gates the result, and the second is what proves the first was right.

import { readdir, readFile } from "node:fs/promises";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { base } from "../site.config.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const dist = resolve(here, "../dist");
const prefix = base.replace(/\/$/, "");

async function htmlFiles(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await htmlFiles(path)));
    else if (entry.name.endsWith(".html")) out.push(path);
  }
  return out;
}

/** Map a site-absolute href onto the file the server would return. */
function targets(href) {
  const path = href.replace(/[?#].*$/, "");
  const trimmed = path.startsWith(prefix) ? path.slice(prefix.length) : null;
  if (trimmed === null) return null; // outside the base — reported by the caller
  const rel = trimmed.replace(/^\//, "");
  if (rel === "" || rel.endsWith("/")) return [join(dist, rel, "index.html")];
  // Either a file, or a route Astro emitted as a directory.
  return [join(dist, rel), join(dist, `${rel}.html`), join(dist, rel, "index.html")];
}

async function main() {
  let pages;
  try {
    pages = await htmlFiles(dist);
  } catch {
    console.error(`check-links: no build to check at ${dist}; run \`npm run build\` first`);
    process.exit(1);
  }
  if (pages.length === 0) {
    console.error("check-links: the build produced no HTML, so nothing was verified");
    process.exit(1);
  }

  const { existsSync } = await import("node:fs");
  const broken = [];
  // Every page some other page links to. What is missing from this set is a
  // page reachable only by typing its URL — see the orphan check below.
  const linked = new Set();
  let checked = 0;

  for (const page of pages) {
    const html = await readFile(page, "utf8");
    const from = relative(dist, page);
    for (const [, href] of html.matchAll(/(?:href|src)="(\/[^"]*)"/g)) {
      // Astro emits hashed asset paths under _astro; they are not navigation.
      if (href.startsWith("/_astro/") || href.startsWith(`${prefix}/_astro/`)) continue;
      checked++;
      const candidates = targets(href);
      if (candidates === null) {
        broken.push(`${from}: ${href} is outside the base path ${prefix}/`);
        continue;
      }
      const found = candidates.find((c) => existsSync(c));
      if (found === undefined) broken.push(`${from}: ${href}`);
      else linked.add(relative(dist, found));
    }
  }

  if (checked === 0) {
    console.error("check-links: found no internal links at all, so nothing was verified");
    process.exit(1);
  }

  if (broken.length > 0) {
    console.error(`check-links: ${broken.length} link(s) do not resolve:`);
    for (const b of [...new Set(broken)].sort()) console.error(`  ${b}`);
    process.exit(1);
  }

  // A page nothing links to is reachable only by typing its URL.
  //
  // The narrower claim is the true one: this catches a page in no sidebar group
  // *and* linked from no prose — a hand-written directory nobody wired up. It
  // does not catch a section merely missing from the sidebar, because the
  // guide's pages cross-link heavily and one link is enough to satisfy this.
  // That failure is caught at the source instead, by sync-docs.mjs, which knows
  // both the routes and the sidebar groups.
  //
  // 404 is exempt: the server reaches it, no page links to it.
  const orphans = pages
    .map((p) => relative(dist, p))
    .filter((p) => p !== "index.html" && p !== "404.html" && !linked.has(p));

  if (orphans.length > 0) {
    console.error(`check-links: ${orphans.length} page(s) are linked from nowhere:`);
    for (const o of orphans.sort()) console.error(`  ${o}`);
    console.error("\nGive the directory a sidebar group in astro.config.mjs, or link the page from one that is reachable.");
    process.exit(1);
  }

  console.log(
    `check-links: ${checked} internal links across ${pages.length} pages all resolve, and every page is reachable`,
  );
}

await main();
