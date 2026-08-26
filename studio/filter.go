package studio

import (
	"net/url"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// operatorsFor is the operator set the grid's filter form offers for c,
// narrowed by type the same way form.go's scalarValue switches on it: ordered
// types get the comparison set, text types get the pattern-match set, every
// column gets the equality set, and Nullable adds isnull/notnull. An Enum
// column is a closed set, so it gets neither the ordered nor the text
// additions — matching operatorDocs' own categories in schema/manifest.go.
func operatorsFor(c schema.ColumnManifest) []string {
	ops := []string{"eq", "ne", "in", "nin"}
	switch {
	case len(c.Enum) > 0:
	case isOrderedType(c.Type):
		ops = append(ops, "gt", "gte", "lt", "lte", "between")
	case isTextType(c.Type):
		ops = append(ops, "contains", "startswith", "endswith", "like", "ilike")
	}
	if c.Nullable {
		ops = append(ops, "isnull", "notnull")
	}
	return ops
}

func isOrderedType(t string) bool {
	switch t {
	case "smallint", "int", "bigint", "real", "float", "numeric", "timestamptz", "date", "time":
		return true
	}
	return false
}

func isTextType(t string) bool {
	switch t {
	case "text", "varchar":
		return true
	}
	return false
}

// filterField is one row of the grid's filter form: an operator select over
// operatorsFor(Column), a value text input, and whatever the operator/value
// were on the request just rendered, so the form redisplays the filter it is
// currently applying rather than reverting to blank.
type filterField struct {
	Column    schema.ColumnManifest
	Operators []string
	Op, Val   string
}

// filterFieldName and filterValueName are the two form-field names one
// filterable column's control pair submits under — chosen so plain HTML GET
// can carry an operator and a value as two params without any JS to combine
// them into the wire's single "op.value" spelling; combineFilters does that
// combining server-side once the request lands.
func filterFieldName(c schema.ColumnManifest) string { return "f_" + c.Name + "_op" }
func filterValueName(c schema.ColumnManifest) string { return "f_" + c.Name + "_val" }

// buildFilterFields renders one filterField per Filterable column, reading
// back whatever operator/value the incoming request already carries so a
// filter survives pagination and stays visible in the form after "Apply".
func buildFilterFields(t *schema.TableManifest, q url.Values) []filterField {
	if t.REST == nil {
		return nil
	}
	var fields []filterField
	for _, name := range t.REST.Filterable {
		c := findColumn(t.Columns, name)
		if c == nil {
			continue
		}
		ops := operatorsFor(*c)
		op := q.Get(filterFieldName(*c))
		if op == "" {
			op = ops[0]
		}
		fields = append(fields, filterField{
			Column:    *c,
			Operators: ops,
			Op:        op,
			Val:       q.Get(filterValueName(*c)),
		})
	}
	return fields
}

// combineFilters turns the incoming request's f_<col>_op/f_<col>_val pairs
// into the REST API's own "<wire>=<op>.<value>" filter params, alongside
// sort/search passed through unchanged. A column whose value is blank is
// left out entirely, rather than sent as an empty-string filter.
func combineFilters(t *schema.TableManifest, q url.Values) url.Values {
	out := url.Values{}
	if s := q.Get("sort"); s != "" {
		out.Set("sort", s)
	}
	if s := q.Get("search"); s != "" {
		out.Set("search", s)
	}
	if t.REST == nil {
		return out
	}
	for _, name := range t.REST.Filterable {
		c := findColumn(t.Columns, name)
		if c == nil {
			continue
		}
		val := strings.TrimSpace(q.Get(filterValueName(*c)))
		if val == "" {
			continue
		}
		op := q.Get(filterFieldName(*c))
		if op == "" {
			op = operatorsFor(*c)[0]
		}
		out.Set(wireOf(*c), op+"."+val)
	}
	return out
}

// sortOptions is every value the grid's sort select offers: each Sortable
// column ascending and, prefixed "-", descending — the same spelling
// schema/manifest.go's own worked example (?sort=-col) uses.
func sortOptions(t *schema.TableManifest) []string {
	if t.REST == nil {
		return nil
	}
	var out []string
	for _, name := range t.REST.Sortable {
		out = append(out, name, "-"+name)
	}
	return out
}
