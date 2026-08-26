// Where the site is deployed. This is the one place the deployment path is
// written: astro.config.mjs configures Astro with it, and scripts/sync-docs.mjs
// prefixes generated links with it. Starlight prefixes its own navigation
// automatically, but content links are ours to get right.
//
// Deploying somewhere else — a custom domain, a user page — means changing
// `base` to "/" here and nowhere else. scripts/check-links.mjs verifies the
// result rather than trusting it.
export const site = "https://mind-vm.github.io";
export const base = "/sqlb";
