// Package recipes is a collection of small, single-topic examples: one file
// per aspect of sqlb, each answering a question someone actually has.
//
// The other directories under example/ are whole applications. They answer
// "what does this look like assembled" — which is the right question once,
// and the wrong one when you already know what you are building and need to
// know how one piece is spelled. That is what this directory is for.
//
// Every recipe is a Go example function, so none of them can drift: `go test
// ./example/recipes` compares the printed output against the comment, and a
// recipe describing an API that changed fails the build rather than misleading
// its next reader. Nothing here needs Docker or a database — the few recipes
// that must execute a statement rather than compile one run against a
// recording executor, for the reason [Builder.SQL] exists: the compiled text
// and its bind parameters are the thing worth showing.
//
// # Finding one
//
// The file names are the index, and README.md holds the same list with a
// sentence each. Grep is the intended entry point:
//
//	rg -l 'cursor' example/recipes      # the files about keyset paging
//	go doc github.com/mind-vm/sqlb/example/recipes
//
// # Adding one
//
// Keep it to one point. A recipe that shows three things is three recipes, and
// the reader searching for the second one will not find it inside the first.
// Print the clause the recipe is about rather than the whole statement — the
// helpers in helpers_test.go do that — and say in the comment *why* the API is
// shaped this way, not only what it does. The comment is the recipe; the code
// is the proof that the comment is still true.
package recipes
