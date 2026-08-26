import { defineCollection } from "astro:content";
import { docsLoader } from "@astrojs/starlight/loaders";
import { docsSchema } from "@astrojs/starlight/schema";

/**
 * A page's route is its path under src/content/docs, unchanged.
 *
 * Astro's default would slugify each path segment with github-slugger, which is
 * lossy: `release-1.0.md` becomes `release-10`. That matters here because the
 * routes are not Astro's to decide. scripts/sync-docs.mjs resolves every link in
 * docs/ against a route index it builds from the filenames, so a page whose
 * filename and route disagree is linked at an address nothing serves — which is
 * exactly what `docs/release-1.0.md` did, from ten pages, until this override.
 *
 * This is the same derivation Astro does (drop the extension, let a README-style
 * `index` name its directory) with the slugify step removed, so it changes the
 * route of no page that was already slug-safe — `release-1.0` is the only route
 * in the site that moves. The trade is that a filename now decides a URL
 * literally, where slugifying used to launder it: a space or a capital under
 * docs/ would reach the web as written. Every file there is lowercase, digits
 * and hyphens today, and that is the convention to keep.
 */
const routeIsThePath = ({ entry }: { entry: string }) =>
  entry.replace(/\.[^./]+$/, "").replace(/\/index$/, "");

export const collections = {
  docs: defineCollection({
    loader: docsLoader({ generateId: routeIsThePath }),
    schema: docsSchema(),
  }),
};
