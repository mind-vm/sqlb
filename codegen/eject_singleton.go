package codegen

import (
	"bytes"
	"fmt"

	"github.com/mind-vm/sqlb/schema"
)

// The exit for a singleton resource.
//
// These are the same three handlers as their collection peers with the id block
// removed: no path segment to parse, and no key condition to prepend, because
// the confining conditions are the whole address. That is exactly what the
// mounted version does (rest/singleton.go), and the mount's own refusal — a
// singleton over a table with no Scoped column — is what makes `confine` here
// guaranteed to carry something. The exit keeps that guarantee: `h.Confine` is
// required for any table with an obligation, which a singleton table always has
// (#166).

func ejectSingletonReadHandler(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	fmt.Fprintf(b, `
	// GET %s — the caller's own row. No {id}: the conditions are the address.
	mux.HandleFunc("GET %s", func(w http.ResponseWriter, r *http.Request) {
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		row, err := Get%s(r.Context(), db, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound(%q))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
`, t.Rest().Path, t.Rest().Path, typeName, t.LocalName())
}

func ejectSingletonUpdateHandler(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	fmt.Fprintf(b, `
	// PATCH %s — write the caller's own row.
	mux.HandleFunc("PATCH %s", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		changes, err := decode%sPatch(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}

		row, err := Update%s(r.Context(), db, changes, confine)
		switch {
		case errors.Is(err, ErrNoChanges):
			WriteProblem(w, badRequest("body", "the request body changed nothing", nil))
			return
		case errors.Is(err, ErrNotFound):
			WriteProblem(w, notFound(%q))
			return
		case err != nil:
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
`, t.Rest().Path, t.Rest().Path, typeName, typeName, t.LocalName())
}

func ejectSingletonDeleteHandler(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	fmt.Fprintf(b, `
	// DELETE %s — remove the caller's own row.
	mux.HandleFunc("DELETE %s", func(w http.ResponseWriter, r *http.Request) {
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		err = Delete%s(r.Context(), db, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound(%q))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusNoContent, nil)
	})
`, t.Rest().Path, t.Rest().Path, typeName, t.LocalName())
}
