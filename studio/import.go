package studio

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mind-vm/sqlb/schema"
)

// Import supports JSON and CSV, not SQL. Export's SQL format is text
// formatting only (export.go's writeSQLExport) — studio never holds a raw
// database connection, so there is nothing here that could execute an
// INSERT statement even if a user uploaded one back.

type importResult struct {
	Row   int
	OK    bool
	Error string
}

type importPage struct {
	pageHeader
	Table   schema.TableManifest
	Back    string
	Error   string
	Results []importResult
	Created int
	Failed  int
}

func (s *Server) handleRowsImportForm(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "create") {
		http.NotFound(w, r)
		return
	}
	s.render(w, s.importTpl, importPage{
		pageHeader: s.header(r),
		Table:      *t,
		Back:       s.url("/tables/" + t.Name + "/rows"),
	})
}

// handleRowsImportSubmit sends one Create per parsed row — there is no
// transaction across them, since each is an independent POST to the target
// API, the same as calling it by hand N times would be. A partial import
// (some rows created, some rejected) is reported, not rolled back.
func (s *Server) handleRowsImportSubmit(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "create") {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}
	back := s.url("/tables/" + t.Name + "/rows")

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		s.renderImportError(w, r, t, back, "reading the upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.renderImportError(w, r, t, back, "no file was uploaded")
		return
	}
	defer file.Close()

	var rows []map[string]any
	switch r.FormValue("format") {
	case "json":
		rows, err = parseImportJSON(file)
	case "csv":
		rows, err = parseImportCSV(t, file)
	default:
		err = fmt.Errorf("format must be json or csv")
	}
	if err != nil {
		s.renderImportError(w, r, t, back, err.Error())
		return
	}

	var results []importResult
	created, failed := 0, 0
	for i, body := range rows {
		if _, err := client.Create(r.Context(), t.REST.Path, body); err != nil {
			failed++
			results = append(results, importResult{Row: i + 1, Error: err.Error()})
			continue
		}
		created++
		results = append(results, importResult{Row: i + 1, OK: true})
	}

	s.render(w, s.importTpl, importPage{
		pageHeader: s.header(r),
		Table:      *t,
		Back:       back,
		Results:    results,
		Created:    created,
		Failed:     failed,
	})
}

func (s *Server) renderImportError(w http.ResponseWriter, r *http.Request, t *schema.TableManifest, back, msg string) {
	s.render(w, s.importTpl, importPage{
		pageHeader: s.header(r),
		Table:      *t,
		Back:       back,
		Error:      msg,
	})
}

// parseImportJSON expects exactly the shape writeJSONExport wrote: an array
// of row objects, wire-keyed. Values arrive already typed, so they pass
// through to Create as-is — validation is the target API's job, the same
// actionable-errors posture the rest of studio already leans on rather than
// pre-filtering client-side.
func parseImportJSON(r io.Reader) ([]map[string]any, error) {
	var rows []map[string]any
	if err := json.NewDecoder(r).Decode(&rows); err != nil {
		return nil, fmt.Errorf("not a JSON array of row objects: %w", err)
	}
	return rows, nil
}

// parseImportCSV expects exactly the shape writeCSVExport wrote: a header
// naming known columns (by Name or Wire), each cell in the same comma-joined
// text format editValue renders — encodeFieldValue is the same decode
// buildFormFields' submit path already uses, so the two formats agree by
// sharing code rather than by having to be kept in sync by hand.
func parseImportCSV(t *schema.TableManifest, r io.Reader) ([]map[string]any, error) {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading the header row: %w", err)
	}
	cols := make([]*schema.ColumnManifest, len(header))
	for i, h := range header {
		c := findColumn(t.Columns, h)
		if c == nil {
			for j := range t.Columns {
				if wireOf(t.Columns[j]) == h {
					c = &t.Columns[j]
					break
				}
			}
		}
		if c == nil {
			return nil, fmt.Errorf("column %q in the header row is not a column of %s", h, t.Name)
		}
		cols[i] = c
	}

	var rows []map[string]any
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, cell := range rec {
			if cell == "" {
				continue // no value: the column's own default applies, the same rule parseFormBody's blank fields follow
			}
			val, err := encodeFieldValue(*cols[i], cell)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", header[i], err)
			}
			row[wireOf(*cols[i])] = val
		}
		rows = append(rows, row)
	}
	return rows, nil
}
