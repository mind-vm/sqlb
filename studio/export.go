package studio

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// handleRowsExport serves the grid's current filter/sort/search view (the
// same query combineFilters builds for handleRows) as a downloadable file.
// It loops every page client.List reports HasMore for rather than capping at
// one — each request is already bounded by the target app's own per-resource
// per_page ceiling, so a large export is many bounded requests rather than
// one unbounded query.
func (s *Server) handleRowsExport(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "list") {
		http.NotFound(w, r)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "json" && format != "csv" && format != "sql" {
		http.Error(w, "studio: format must be json, csv or sql", http.StatusBadRequest)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}

	q := combineFilters(t, r.URL.Query())
	var rows []map[string]any
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		result, err := client.List(r.Context(), t.REST.Path, q)
		if err != nil {
			s.renderAPIError(w, r, err)
			return
		}
		rows = append(rows, result.Items...)
		if !result.HasMore {
			break
		}
	}

	switch format {
	case "json":
		writeJSONExport(w, t, rows)
	case "csv":
		writeCSVExport(w, t, rows)
	case "sql":
		writeSQLExport(w, t, rows)
	}
}

func attachmentHeaders(w http.ResponseWriter, contentType, filename string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
}

func writeJSONExport(w http.ResponseWriter, t *schema.TableManifest, rows []map[string]any) {
	attachmentHeaders(w, "application/json", t.Name+".json")
	if rows == nil {
		rows = []map[string]any{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rows)
}

// writeCSVExport's cells go through editValue, the same rendering the edit
// form's text input already uses (comma-joined arrays, plain scalars) —
// which is what lets a CSV import (import.go) decode a cell with
// encodeFieldValue unchanged, rather than inventing a second format the two
// have to agree on by hand.
func writeCSVExport(w http.ResponseWriter, t *schema.TableManifest, rows []map[string]any) {
	attachmentHeaders(w, "text/csv", t.Name+".csv")
	cw := csv.NewWriter(w)
	header := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		header[i] = wireOf(c)
	}
	_ = cw.Write(header)
	for _, row := range rows {
		rec := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			rec[i] = editValue(c, row[wireOf(c)])
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// writeSQLExport renders one INSERT per row over writableColumns rather than
// every column, the same set the create form offers — a computed or
// read-only column (a serial primary key, a generated column) would conflict
// if replayed literally into a fresh table. This is text formatting only: no
// database connection is involved, which is what makes SQL an export format
// here and not an import one (see import.go's doc comment).
func writeSQLExport(w http.ResponseWriter, t *schema.TableManifest, rows []map[string]any) {
	attachmentHeaders(w, "application/sql", t.Name+".sql")
	cols := writableColumns(t)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(wireOf(c))
	}
	colList := strings.Join(names, ", ")
	for _, row := range rows {
		vals := make([]string, len(cols))
		for i, c := range cols {
			vals[i] = sqlLiteral(c, row[wireOf(c)])
		}
		fmt.Fprintf(w, "INSERT INTO %s (%s) VALUES (%s);\n", quoteIdent(t.Name), colList, strings.Join(vals, ", "))
	}
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

var arrayEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// sqlLiteral renders one decoded JSON value as a Postgres literal. An array
// column becomes a '{"el1","el2"}' array-literal rather than a JSON array —
// the wire value is JSON, but the column it is going into is not.
func sqlLiteral(c schema.ColumnManifest, v any) string {
	if v == nil {
		return "NULL"
	}
	if c.Array {
		arr, ok := v.([]any)
		if !ok {
			return "NULL"
		}
		parts := make([]string, len(arr))
		for i, e := range arr {
			parts[i] = `"` + arrayEscaper.Replace(dispValue(e)) + `"`
		}
		return "'{" + strings.Join(parts, ",") + "}'"
	}
	switch t := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "NULL"
		}
		return "'" + strings.ReplaceAll(string(b), "'", "''") + "'"
	}
}
