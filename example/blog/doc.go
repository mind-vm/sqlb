// Package blog is the generated half of the blog example, plus the
// hand-written code that sits beside it.
//
// models_gen.go, columns_gen.go and rest_gen.go come from blogschema by
// `sqlb generate` and are not edited here. deletes.go, hooks.go and
// post_ext.go are the seam a generated project always needs: a soft-delete
// endpoint no hook can express, the BeforeQuery registration that makes it
// stick, and a domain method the generator has no column for.
package blog
