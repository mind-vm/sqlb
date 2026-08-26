// Ejected from a sqlb schema by `sqlb eject`. This file is yours now:
// edit it, delete parts of it, or keep regenerating it — `sqlb eject -check`
// reports drift for as long as you want it to and is meant to be dropped
// from CI on the day you stop.
//
// net/http handlers, one per exposed operation.

package ejected

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The endpoints. Every handler is a function you can read top to bottom: parse,
// confine, run one statement, write JSON.

// Options carries the seams the handlers cannot supply for themselves.
//
// In sqlb these were hooks on a registry; here they are function fields, which
// is the same seam with the machinery removed. A resource whose table declared
// Scoped or SoftDelete will not register until they are set — see Register.
type Options struct {
	Authors AuthorsHooks
	Orgs    OrgsHooks
	Posts   PostsHooks
}

// AuthorsHooks are the seams for /authors.
type AuthorsHooks struct {
	// Confine narrows every statement this resource issues. Nil means
	// unconfined, which is only allowed when the schema declared nothing.
	Confine func(*http.Request) ([]Condition, error)
	// Assign supplies column values a create must set that no request body
	// carries. It runs before the insert and its values win.
	Assign func(*http.Request) (map[string]any, error)
}

// OrgsHooks are the seams for /orgs.
type OrgsHooks struct {
	// Confine narrows every statement this resource issues. Nil means
	// unconfined, which is only allowed when the schema declared nothing.
	Confine func(*http.Request) ([]Condition, error)
	// Assign supplies column values a create must set that no request body
	// carries. It runs before the insert and its values win.
	Assign func(*http.Request) (map[string]any, error)
}

// PostsHooks are the seams for /posts.
//
// Confine is required here (deleted_at declares a soft delete), and returns the conditions every read
// and write is narrowed by — the predicate a BeforeQuery hook used to add.
type PostsHooks struct {
	// Confine narrows every statement this resource issues. Nil means
	// unconfined, which is only allowed when the schema declared nothing.
	Confine func(*http.Request) ([]Condition, error)
	// Assign supplies column values a create must set that no request body
	// carries. It runs before the insert and its values win.
	Assign func(*http.Request) (map[string]any, error)
}

// Register mounts every resource the schema exposed.
//
// It returns an error rather than panicking, and it returns one for a missing
// obligation before it registers anything: a resource that declared a tenant
// column and has nothing to confine it with would serve every tenant's rows
// with a 200 next to them, and that is the failure this check exists for.
func Register(mux *http.ServeMux, db DB, opts Options) error {
	if err := registerAuthors(mux, db, opts.Authors); err != nil {
		return err
	}
	if err := registerOrgs(mux, db, opts.Orgs); err != nil {
		return err
	}
	if err := registerPosts(mux, db, opts.Posts); err != nil {
		return err
	}
	return nil
}

// authorLimits are the ceilings authors declared.
var authorLimits = Limits{DefaultPageSize: 25, MaxPageSize: 100, MaxFilters: 0, MaxSortTerms: 0, MaxOffset: 0}

// authorInsertDefaults are the columns an insert must set that no request body
// carries and the database has no default for. sqlb wrote the row's zero value;
// so does this, and this is where to write something better.
var authorInsertDefaults = map[string]any{
	"password_hash": "",
}

// decodeAuthorCreate reads a create body for authors: which columns it named, and
// what each one carried. An unknown property is refused with the list of the
// ones that would have worked.
func decodeAuthorCreate(data []byte) (map[string]any, error) {
	allowed := []string{"org_id", "email", "name"}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, badRequest("body", "request body is not a JSON object: "+err.Error(), allowed)
	}
	out := map[string]any{}
	for name, msg := range raw {
		switch name {
		case "org_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["org_id"] = *v
		case "email":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["email"] = *v
		case "name":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["name"] = *v
		default:
			return nil, badRequest("body."+name, "unknown property", allowed)
		}
	}
	if _, ok := out["org_id"]; !ok {
		return nil, badRequest("body.org_id", "this property is required", allowed)
	}
	if _, ok := out["email"]; !ok {
		return nil, badRequest("body.email", "this property is required", allowed)
	}
	if _, ok := out["name"]; !ok {
		return nil, badRequest("body.name", "this property is required", allowed)
	}
	return out, nil
}

// decodeAuthorPatch reads a patch body for authors: which columns it named, and
// what each one carried. An unknown property is refused with the list of the
// ones that would have worked.
func decodeAuthorPatch(data []byte) (map[string]any, error) {
	allowed := []string{"org_id", "email", "name"}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, badRequest("body", "request body is not a JSON object: "+err.Error(), allowed)
	}
	out := map[string]any{}
	for name, msg := range raw {
		switch name {
		case "org_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["org_id"] = *v
		case "email":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["email"] = *v
		case "name":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["name"] = *v
		default:
			return nil, badRequest("body."+name, "unknown property", allowed)
		}
	}
	return out, nil
}

// registerAuthors mounts /authors.
func registerAuthors(mux *http.ServeMux, db DB, h AuthorsHooks) error {

	// GET /authors — filter, sort and page. The operators are the ones that are a
	// single SQL fragment; ?cursor, ?select, ?expand and ?filter are refused by
	// name rather than ignored.
	mux.HandleFunc("GET /authors", func(w http.ResponseWriter, r *http.Request) {
		req, err := ParseList(r.URL.Query(), authorColumns, authorLimits)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		req.Query.Where = append(req.Query.Where, confine...)

		rows, err := ListAuthor(r.Context(), db, req.Query)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// One row past the page was fetched so that has_more costs a row
		// rather than a count.
		hasMore := len(rows) > req.PerPage
		if hasMore {
			rows = rows[:req.PerPage]
		}
		page := Page[Author]{Items: rows, Page: req.Page, PerPage: req.PerPage, HasMore: hasMore}
		if rows == nil {
			page.Items = []Author{}
		}
		if req.Count {
			total, err := CountAuthor(r.Context(), db, req.Query.Where)
			if err != nil {
				WriteProblem(w, err)
				return
			}
			page.Total = &total
		}
		WriteJSON(w, http.StatusOK, page)
	})

	// GET /authors/{id}
	mux.HandleFunc("GET /authors/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), authorColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		row, err := GetAuthor(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound("authors"))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})

	// POST /authors
	mux.HandleFunc("POST /authors", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		values, err := decodeAuthorCreate(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}

		// The columns no body carries and the database does not default.
		for k, v := range authorInsertDefaults {
			if _, named := values[k]; !named {
				values[k] = v
			}
		}
		assigned, err := assignFor(r, h.Assign)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// The assignment wins: it is the server's own statement about the row,
		// and a request that named the same column was never allowed to.
		for k, v := range assigned {
			values[k] = v
		}

		row, err := InsertAuthor(r.Context(), db, values)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, row)
	})

	// PATCH /authors/{id}
	mux.HandleFunc("PATCH /authors/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), authorColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		changes, err := decodeAuthorPatch(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}

		row, err := UpdateAuthor(r.Context(), db, id, changes, confine)
		switch {
		case errors.Is(err, ErrNoChanges):
			WriteProblem(w, badRequest("body", "the request body changed nothing", nil))
			return
		case errors.Is(err, ErrNotFound):
			WriteProblem(w, notFound("authors"))
			return
		case err != nil:
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})

	// DELETE /authors/{id}
	mux.HandleFunc("DELETE /authors/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), authorColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		err = DeleteAuthor(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound("authors"))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusNoContent, nil)
	})
	return nil
}

// orgLimits are the ceilings orgs declared.
var orgLimits = Limits{DefaultPageSize: 0, MaxPageSize: 0, MaxFilters: 0, MaxSortTerms: 0, MaxOffset: 0}

// registerOrgs mounts /orgs.
func registerOrgs(mux *http.ServeMux, db DB, h OrgsHooks) error {

	// GET /orgs — filter, sort and page. The operators are the ones that are a
	// single SQL fragment; ?cursor, ?select, ?expand and ?filter are refused by
	// name rather than ignored.
	mux.HandleFunc("GET /orgs", func(w http.ResponseWriter, r *http.Request) {
		req, err := ParseList(r.URL.Query(), orgColumns, orgLimits)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		req.Query.Where = append(req.Query.Where, confine...)

		rows, err := ListOrg(r.Context(), db, req.Query)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// One row past the page was fetched so that has_more costs a row
		// rather than a count.
		hasMore := len(rows) > req.PerPage
		if hasMore {
			rows = rows[:req.PerPage]
		}
		page := Page[Org]{Items: rows, Page: req.Page, PerPage: req.PerPage, HasMore: hasMore}
		if rows == nil {
			page.Items = []Org{}
		}
		if req.Count {
			total, err := CountOrg(r.Context(), db, req.Query.Where)
			if err != nil {
				WriteProblem(w, err)
				return
			}
			page.Total = &total
		}
		WriteJSON(w, http.StatusOK, page)
	})

	// GET /orgs/{id}
	mux.HandleFunc("GET /orgs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), orgColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		row, err := GetOrg(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound("orgs"))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
	return nil
}

// postLimits are the ceilings posts declared.
var postLimits = Limits{DefaultPageSize: 20, MaxPageSize: 100, MaxFilters: 12, MaxSortTerms: 4, MaxOffset: 5000, DefaultSort: []Order{{Column: "created_at", Desc: true}}}

// decodePostCreate reads a create body for posts: which columns it named, and
// what each one carried. An unknown property is refused with the list of the
// ones that would have worked.
func decodePostCreate(data []byte) (map[string]any, error) {
	allowed := []string{"org_id", "author_id", "title", "body", "status", "published_at"}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, badRequest("body", "request body is not a JSON object: "+err.Error(), allowed)
	}
	out := map[string]any{}
	for name, msg := range raw {
		switch name {
		case "org_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["org_id"] = *v
		case "author_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["author_id"] = *v
		case "title":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["title"] = *v
		case "body":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["body"] = *v
		case "status":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["status"] = *v
		case "published_at":
			var v *time.Time
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			out["published_at"] = v
		default:
			return nil, badRequest("body."+name, "unknown property", allowed)
		}
	}
	if _, ok := out["org_id"]; !ok {
		return nil, badRequest("body.org_id", "this property is required", allowed)
	}
	if _, ok := out["author_id"]; !ok {
		return nil, badRequest("body.author_id", "this property is required", allowed)
	}
	if _, ok := out["title"]; !ok {
		return nil, badRequest("body.title", "this property is required", allowed)
	}
	if _, ok := out["body"]; !ok {
		return nil, badRequest("body.body", "this property is required", allowed)
	}
	return out, nil
}

// decodePostPatch reads a patch body for posts: which columns it named, and
// what each one carried. An unknown property is refused with the list of the
// ones that would have worked.
func decodePostPatch(data []byte) (map[string]any, error) {
	allowed := []string{"org_id", "author_id", "title", "body", "status", "published_at"}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, badRequest("body", "request body is not a JSON object: "+err.Error(), allowed)
	}
	out := map[string]any{}
	for name, msg := range raw {
		switch name {
		case "org_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["org_id"] = *v
		case "author_id":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["author_id"] = *v
		case "title":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["title"] = *v
		case "body":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["body"] = *v
		case "status":
			var v *string
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			if v == nil {
				return nil, badRequest("body."+name, "this column is not nullable", nil)
			}
			out["status"] = *v
		case "published_at":
			var v *time.Time
			if err := json.Unmarshal(msg, &v); err != nil {
				return nil, badRequest("body."+name, err.Error(), nil)
			}
			out["published_at"] = v
		default:
			return nil, badRequest("body."+name, "unknown property", allowed)
		}
	}
	return out, nil
}

// registerPosts mounts /posts.
func registerPosts(mux *http.ServeMux, db DB, h PostsHooks) error {
	if h.Confine == nil {
		return fmt.Errorf("ejected: %s: Confine is required (%s)", "/posts", "deleted_at declares a soft delete")
	}

	// GET /posts — filter, sort and page. The operators are the ones that are a
	// single SQL fragment; ?cursor, ?select, ?expand and ?filter are refused by
	// name rather than ignored.
	mux.HandleFunc("GET /posts", func(w http.ResponseWriter, r *http.Request) {
		req, err := ParseList(r.URL.Query(), postColumns, postLimits)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		req.Query.Where = append(req.Query.Where, confine...)

		rows, err := ListPost(r.Context(), db, req.Query)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// One row past the page was fetched so that has_more costs a row
		// rather than a count.
		hasMore := len(rows) > req.PerPage
		if hasMore {
			rows = rows[:req.PerPage]
		}
		page := Page[Post]{Items: rows, Page: req.Page, PerPage: req.PerPage, HasMore: hasMore}
		if rows == nil {
			page.Items = []Post{}
		}
		if req.Count {
			total, err := CountPost(r.Context(), db, req.Query.Where)
			if err != nil {
				WriteProblem(w, err)
				return
			}
			page.Total = &total
		}
		WriteJSON(w, http.StatusOK, page)
	})

	// GET /posts/{id}
	mux.HandleFunc("GET /posts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), postColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		row, err := GetPost(r.Context(), db, id, confine)
		if errors.Is(err, ErrNotFound) {
			WriteProblem(w, notFound("posts"))
			return
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})

	// POST /posts
	mux.HandleFunc("POST /posts", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		values, err := decodePostCreate(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		assigned, err := assignFor(r, h.Assign)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		// The assignment wins: it is the server's own statement about the row,
		// and a request that named the same column was never allowed to.
		for k, v := range assigned {
			values[k] = v
		}

		row, err := InsertPost(r.Context(), db, values)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, row)
	})

	// PATCH /posts/{id}
	mux.HandleFunc("PATCH /posts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"), postColumns, "id")
		if err != nil {
			WriteProblem(w, err)
			return
		}
		body, err := readBody(r)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		changes, err := decodePostPatch(body)
		if err != nil {
			WriteProblem(w, err)
			return
		}
		confine, err := confineFor(r, h.Confine)
		if err != nil {
			WriteProblem(w, err)
			return
		}

		row, err := UpdatePost(r.Context(), db, id, changes, confine)
		switch {
		case errors.Is(err, ErrNoChanges):
			WriteProblem(w, badRequest("body", "the request body changed nothing", nil))
			return
		case errors.Is(err, ErrNotFound):
			WriteProblem(w, notFound("posts"))
			return
		case err != nil:
			WriteProblem(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, row)
	})
	return nil
}
