# Working material

Design specs for work that has **not** shipped. Nothing here is documentation:
no reading order, no upkeep, and no promise that it describes the library as it
stands.

A spec leaves this directory in one of two ways. If the work ships, the
reasoning that survives it belongs in
[architecture.md's Decisions](../architecture.md#decisions) — one entry, argued
in the repo's voice, with a revisit trigger — and the spec is deleted. If the
work is abandoned, the spec is deleted and the decision not to build it is
recorded the same way.

Neither outcome keeps the file. `git log` holds it either way, which is the
same argument that folded `docs/adr/` into one page: a document nothing reads
and nothing checks is a document that is quietly wrong, and a step-by-step plan
for code that already exists is wrong the day it merges.

Three plans and two specs were deleted under that rule after their features
shipped — one-to-one relationships, the pluggable auth seam and the WorkOS
adapter, 3,616 lines of instructions for code you can now read. What each one
argued is in the Decisions section: "A unique foreign key is already
one-to-one" and "A Verifier composes with the principal seam".
