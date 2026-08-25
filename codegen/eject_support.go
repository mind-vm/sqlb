package codegen

// support.go is the only part of the exit that is the same in every project: a
// few hundred lines of query-string parsing, WHERE assembly and JSON writing
// that any filterable list endpoint needs and that nobody enjoys writing twice.
//
// It is emitted as source rather than imported from a package here, and that is
// the whole design. A library sqlb published for ejected code to import would
// be sqlb again under another name — the adopter would still have the
// dependency, still have the version skew, and still be unable to change how a
// filter parses without opening a pull request against someone else's
// repository. Emitted, it is theirs: delete the operators they do not serve,
// change the envelope, or keep it as it is.

import "strings"

// ejectSupportSource renders support.go for a package.
//
// The template holds "@@" where the emitted file needs a backtick, because a Go
// raw string literal cannot contain one and the emitted struct tags are full of
// them. It is replaced in exactly one place, here, rather than being worked
// around at each tag.
func ejectSupportSource(pkg string) string {
	return strings.ReplaceAll(
		strings.Replace(ejectSupportTemplate, "%PACKAGE%", pkg, 1),
		"@@", "`")
}

const ejectSupportTemplate = `// Ejected from a sqlb schema by ` + "`sqlb eject`" + `. This file is yours now:
// edit it, delete parts of it, or keep regenerating it — ` + "`sqlb eject -check`" + `
// reports drift for as long as you want it to and is meant to be dropped
// from CI on the day you stop.
//
// This is the shared half of the exit: the request parsing, the WHERE
// assembly and the JSON writing that every endpoint in handlers.go uses. It
// imports pgx and the standard library, and nothing else.

package %PACKAGE%

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Schema is schema.sql, embedded so that a test or a bootstrap can apply the
// whole schema without knowing where the file ended up.
//
//go:embed schema.sql
var Schema string

// DB is what the statements need: a pgx pool, a connection or a transaction,
// all three of which satisfy it. Narrow on purpose — a handler that takes this
// cannot begin a transaction it was not given, and a test can hand it whatever
// it likes.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// scanner is pgx.Row and pgx.Rows both, which is what lets one generated
// scanner serve a single read and a page.
type scanner interface {
	Scan(dest ...any) error
}

var (
	// ErrNotFound is the id that matched nothing the request was allowed to
	// see. Handlers turn it into a 404 without saying which of the two it was.
	ErrNotFound = errors.New("not found")
	// ErrNoChanges is a PATCH that named no column.
	ErrNoChanges = errors.New("no columns to change")
)

// Column is what a request may name, and for what. The capabilities are the
// ones the schema declared: a column that never opted into filtering is not
// filterable here either, and the rejection says which columns are.
//
// It carries two spellings because a request and a statement do not have to
// agree on one. The schema's WireCase decides how a column is named on the
// wire, and under a declared case those two names differ — so a request says
// ?createdAt=gte.… while the SQL still has to say "created_at". Matching a
// parameter, listing what would have been accepted and naming a rejected one
// are Wire's jobs; everything that reaches Postgres is built from Name.
type Column struct {
	// Name is the column, and is the only spelling that reaches SQL.
	Name string
	// Wire is how a request spells it: the query-string key, a ?sort term, the
	// name in a rejection's allowed list. Empty when the schema left the two
	// the same, which is the default — see wire.
	Wire       string
	Filterable bool
	Sortable   bool
	Searchable bool
	// Parse turns a query-string value into something pgx can bind.
	Parse func(string) (any, error)
}

// wire is the spelling a request uses. Reading it through a method rather than
// the field is what lets the table above omit Wire for a column whose two names
// are the same, which under the default WireCase is every column.
func (c Column) wire() string {
	if c.Wire != "" {
		return c.Wire
	}
	return c.Name
}

// findColumn looks a column up by the name a request used.
func findColumn(cols []Column, wire string) (Column, bool) {
	for _, c := range cols {
		if c.wire() == wire {
			return c, true
		}
	}
	return Column{}, false
}

// findByColumn looks a column up by its database name, which is what the
// schema hands this file for a primary key — a path segment is not a
// query-string key and has no wire spelling of its own.
func findByColumn(cols []Column, name string) (Column, bool) {
	for _, c := range cols {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// wireNames are the spellings a rejection lists, which are the spellings the
// request that was rejected could have used.
func wireNames(cols []Column, pick func(Column) bool) []string {
	var out []string
	for _, c := range cols {
		if pick(c) {
			out = append(out, c.wire())
		}
	}
	return out
}

// columnNames are the database names, and are for building SQL rather than for
// showing anyone.
func columnNames(cols []Column, pick func(Column) bool) []string {
	var out []string
	for _, c := range cols {
		if pick(c) {
			out = append(out, c.Name)
		}
	}
	return out
}

// The operators, as the SQL each one becomes.
const (
	OpEq      = "="
	OpNe      = "<>"
	OpLt      = "<"
	OpLte     = "<="
	OpGt      = ">"
	OpGte     = ">="
	OpIn      = "IN"
	OpNotIn   = "NOT IN"
	OpIsNull  = "IS NULL"
	OpNotNull = "IS NOT NULL"
	OpLike    = "LIKE"
	OpILike   = "ILIKE"
	OpBetween = "BETWEEN"
)

// Condition is one predicate. Or holds a disjunction — ?search fans out over
// the searchable columns and is the only thing that produces one.
type Condition struct {
	Column string
	Op     string
	Value  any
	Value2 any
	Values []any
	Or     []Condition
}

// Order is one ORDER BY term.
type Order struct {
	Column string
	Desc   bool
}

// Query is everything a read varies by.
type Query struct {
	Where  []Condition
	Order  []Order
	Limit  int
	Offset int
}

// Limits are the resource's declared ceilings, emitted from the schema so the
// exit refuses the same oversized requests the API did.
type Limits struct {
	DefaultPageSize int
	MaxPageSize     int
	MaxFilters      int
	MaxSortTerms    int
	// MaxOffset bounds how deep ?page may reach. Offset paging is the one
	// dimension of a request whose cost grows with the number the client sent,
	// and it is the dimension the exit is least able to soften: ?cursor did not
	// come out, so a client walking a large collection has no cheaper spelling
	// to be redirected to.
	MaxOffset int
	// DefaultSort is the ordering a request naming no ?sort gets. Empty means
	// primary-key order. It is not a ceiling like the rest of this struct: it
	// is what the collection *is*, and it is here so the exit answers an
	// unsorted list the way the API did rather than in whatever order the
	// database found convenient.
	DefaultSort []Order
}

// ListRequest is a parsed list query.
type ListRequest struct {
	Query   Query
	Page    int
	PerPage int
	Count   bool
}

// Page is the body of a list response, and is the envelope sqlb served, minus
// next_cursor: keyset paging did not come out with the rest, so offering the
// field would be a promise this code cannot keep.
type Page[T any] struct {
	Items   []T    @@json:"items"@@
	Page    int    @@json:"page"@@
	PerPage int    @@json:"per_page"@@
	HasMore bool   @@json:"has_more"@@
	Total   *int64 @@json:"total,omitempty"@@
}

// args accumulates bind parameters and hands back the placeholder for each, so
// no statement in store.go counts $N by hand.
type args struct{ values []any }

func (a *args) add(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

func quoteIdent(s string) string {
	if ns, rel, ok := strings.Cut(s, "."); ok {
		return quoteIdent(ns) + "." + quoteIdent(rel)
	}
	return @@"@@ + strings.ReplaceAll(s, @@"@@, @@""@@) + @@"@@
}

func writeWhere(sb *strings.Builder, a *args, conds []Condition) {
	if len(conds) == 0 {
		return
	}
	sb.WriteString(" WHERE ")
	for i, c := range conds {
		if i > 0 {
			sb.WriteString(" AND ")
		}
		writeCondition(sb, a, c)
	}
}

func writeCondition(sb *strings.Builder, a *args, c Condition) {
	if len(c.Or) > 0 {
		sb.WriteString("(")
		for i, sub := range c.Or {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			writeCondition(sb, a, sub)
		}
		sb.WriteString(")")
		return
	}

	col := quoteIdent(c.Column)
	switch c.Op {
	case OpIsNull, OpNotNull:
		fmt.Fprintf(sb, "%s %s", col, c.Op)
	case OpIn, OpNotIn:
		if len(c.Values) == 0 {
			// An empty list matches nothing, and its negation matches
			// everything. Rendering "IN ()" instead would not parse.
			if c.Op == OpIn {
				sb.WriteString("false")
			} else {
				sb.WriteString("true")
			}
			return
		}
		holes := make([]string, len(c.Values))
		for i, v := range c.Values {
			holes[i] = a.add(v)
		}
		fmt.Fprintf(sb, "%s %s (%s)", col, c.Op, strings.Join(holes, ", "))
	case OpBetween:
		fmt.Fprintf(sb, "%s BETWEEN %s AND %s", col, a.add(c.Value), a.add(c.Value2))
	default:
		fmt.Fprintf(sb, "%s %s %s", col, c.Op, a.add(c.Value))
	}
}

func writeOrder(sb *strings.Builder, orders []Order) {
	if len(orders) == 0 {
		return
	}
	sb.WriteString(" ORDER BY ")
	for i, o := range orders {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(quoteIdent(o.Column))
		if o.Desc {
			sb.WriteString(" DESC")
		} else {
			sb.WriteString(" ASC")
		}
	}
}

func writeLimit(sb *strings.Builder, limit, offset int) {
	// Literals rather than parameters, so a plan can see them. Both are
	// integers this file produced, so there is nothing to inject.
	if limit > 0 {
		fmt.Fprintf(sb, " LIMIT %d", limit)
	}
	if offset > 0 {
		fmt.Fprintf(sb, " OFFSET %d", offset)
	}
}

// The value parsers. One per column type, chosen when the column table was
// emitted, so a filter on an int column rejects "abc" here rather than at the
// database.
//
// They are exported because the column table above is a table you edit: adding
// a column means naming one of these beside it, and a schema that happens to
// have no float column today should still find ParseFloat here tomorrow.

func ParseText(s string) (any, error) { return s, nil }

func ParseInt(s string) (any, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a whole number", s)
	}
	return n, nil
}

func ParseFloat(s string) (any, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	return f, nil
}

func ParseBool(s string) (any, error) {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not true or false", s)
	}
	return b, nil
}

// ParseTime accepts RFC 3339 and a bare date, which are the two spellings a
// client sends for a timestamp and a date column.
func ParseTime(s string) (any, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return nil, fmt.Errorf("%q is not an RFC 3339 timestamp or a YYYY-MM-DD date", s)
}

// operators maps the wire spelling onto the SQL, and says how many operands
// each takes: 0 for a null test, 2 for between, -1 for a list, 1 otherwise.
var operators = map[string]struct {
	sql      string
	operands int
	pattern  string
}{
	"eq":         {sql: OpEq, operands: 1},
	"ne":         {sql: OpNe, operands: 1},
	"neq":        {sql: OpNe, operands: 1},
	"lt":         {sql: OpLt, operands: 1},
	"lte":        {sql: OpLte, operands: 1},
	"gt":         {sql: OpGt, operands: 1},
	"gte":        {sql: OpGte, operands: 1},
	"in":         {sql: OpIn, operands: -1},
	"nin":        {sql: OpNotIn, operands: -1},
	"isnull":     {sql: OpIsNull, operands: 0},
	"notnull":    {sql: OpNotNull, operands: 0},
	"between":    {sql: OpBetween, operands: 2},
	"like":       {sql: OpLike, operands: 1, pattern: "%s"},
	"ilike":      {sql: OpILike, operands: 1, pattern: "%s"},
	"contains":   {sql: OpILike, operands: 1, pattern: "%%%s%%"},
	"startswith": {sql: OpILike, operands: 1, pattern: "%s%%"},
	"endswith":   {sql: OpILike, operands: 1, pattern: "%%%s"},
}

// reserved are the query parameters that are not column filters.
var reserved = map[string]bool{
	"page": true, "per_page": true, "sort": true, "search": true, "count": true,
}

// notEjected are the parameters sqlb served that this code does not.
//
// They are refused rather than ignored, and the refusal says so. A client that
// keeps sending ?expand=author would otherwise get a 200 and a response with a
// field missing, which is the failure mode an exit must not have.
var notEjected = map[string]string{
	"cursor": "keyset pagination did not come out with the exit; page with ?page and ?per_page",
	"select": "sparse projections did not come out with the exit; the full row is returned",
	"expand": "relation expansion did not come out with the exit; fetch the related row from its own endpoint",
	"filter": "the JSON filter tree did not come out with the exit; use the query-parameter operators",
}

// The ceilings a resource that declared none is held to. They are sqlb's own
// defaults, copied rather than referenced, so the exit refuses the same
// oversized requests the API did.
const (
	defaultPageSize  = 25
	defaultMaxPage   = 200
	defaultFilters   = 24
	defaultSorts     = 4
	defaultMaxOffset = 100000
	// A list is one condition against MaxFilters however long it is, and a
	// value is a lever on how much work a scan does. Without these two, the
	// filter budget above is bypassed by writing ?id=in.1,2,3,… — one
	// parameter, one condition, and a bind parameter per member until pgx's
	// 65535 runs out. Constants rather than Limits fields because they are
	// package constants in sqlb too, and the claim this file makes is that the
	// exit refuses what the API refused (#69).
	maxListValues  = 100
	maxValueLength = 256
)

func (l Limits) resolved() Limits {
	if l.DefaultPageSize <= 0 {
		l.DefaultPageSize = defaultPageSize
	}
	if l.MaxPageSize <= 0 {
		l.MaxPageSize = defaultMaxPage
	}
	if l.MaxFilters <= 0 {
		l.MaxFilters = defaultFilters
	}
	if l.MaxSortTerms <= 0 {
		l.MaxSortTerms = defaultSorts
	}
	if l.MaxOffset <= 0 {
		l.MaxOffset = defaultMaxOffset
	}
	return l
}

// ParseList turns a query string into a Query, refusing what the resource never
// offered and what the exit does not carry.
func ParseList(values url.Values, cols []Column, lim Limits) (ListRequest, error) {
	lim = lim.resolved()
	out := ListRequest{Page: 1}

	for name, raw := range values {
		if reserved[name] {
			continue
		}
		if why, gone := notEjected[name]; gone {
			return out, &Problem{
				Status: http.StatusBadRequest,
				Title:  http.StatusText(http.StatusBadRequest),
				Detail: "?" + name + " is not served here",
				Errors: []*ProblemDetail{{Message: why, Location: "query." + name}},
			}
		}
		col, known := findColumn(cols, name)
		if !known || !col.Filterable {
			return out, badRequest("query."+name, "unknown or unfilterable column",
				wireNames(cols, func(c Column) bool { return c.Filterable }))
		}
		for _, value := range raw {
			cond, err := parseCondition(col, value)
			if err != nil {
				return out, err
			}
			out.Query.Where = append(out.Query.Where, cond)
		}
	}
	if lim.MaxFilters > 0 && len(out.Query.Where) > lim.MaxFilters {
		return out, badRequest("query", fmt.Sprintf("at most %d filters per request", lim.MaxFilters), nil)
	}

	if term := values.Get("search"); term != "" {
		// Column names rather than wire names: this list is fanned out into a
		// disjunction of predicates, and nothing here is shown to the caller.
		searchable := columnNames(cols, func(c Column) bool { return c.Searchable })
		if len(searchable) == 0 {
			return out, badRequest("query.search", "no column here is searchable", nil)
		}
		if len(term) > maxValueLength {
			return out, badRequest("query.search",
				fmt.Sprintf("search term is %d bytes, the limit is %d", len(term), maxValueLength), nil)
		}
		// Escaped, like every other pattern operand. Search was the one path
		// that left % and _ live, so ?search=50%25 produced the operand %50%%
		// — a caller-controlled scan-cost lever, and a silent behaviour change
		// from the API, where a search for a literal "50%" matched literally.
		pattern := "%" + escapeLike(term) + "%"
		or := make([]Condition, 0, len(searchable))
		for _, name := range searchable {
			or = append(or, Condition{Column: name, Op: OpILike, Value: pattern})
		}
		out.Query.Where = append(out.Query.Where, Condition{Or: or})
	}

	if sortParam := values.Get("sort"); sortParam != "" {
		terms := strings.Split(sortParam, ",")
		if lim.MaxSortTerms > 0 && len(terms) > lim.MaxSortTerms {
			return out, badRequest("query.sort", fmt.Sprintf("at most %d sort terms", lim.MaxSortTerms), nil)
		}
		for _, term := range terms {
			name := strings.TrimSpace(term)
			desc := strings.HasPrefix(name, "-")
			name = strings.TrimPrefix(name, "-")
			col, known := findColumn(cols, name)
			if !known || !col.Sortable {
				return out, badRequest("query.sort", "unknown or unsortable column "+name,
					wireNames(cols, func(c Column) bool { return c.Sortable }))
			}
			out.Query.Order = append(out.Query.Order, Order{Column: col.Name, Desc: desc})
		}
	} else {
		// The resource's declared ordering, applied only when the request named
		// none. Appended rather than assigned so the caller's slice is never
		// aliased into a parsed request.
		out.Query.Order = append(out.Query.Order, lim.DefaultSort...)
	}

	perPage := lim.DefaultPageSize
	if raw := values.Get("per_page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return out, badRequest("query.per_page", "per_page must be a positive whole number", nil)
		}
		perPage = n
	}
	if lim.MaxPageSize > 0 && perPage > lim.MaxPageSize {
		perPage = lim.MaxPageSize
	}
	page := 1
	if raw := values.Get("page"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			return out, badRequest("query.page", "page must be 1 or more", nil)
		}
		// The offset budget. Without it ?page=50000000 is a request for ten
		// billion discarded rows, and a page number near the top of int64
		// overflows (n-1)*perPage into a negative offset that fails at the
		// database rather than at validation. Compared in int64, before
		// anything narrows.
		if n-1 > int64(lim.MaxOffset)/int64(perPage) {
			return out, badRequest("query.page",
				fmt.Sprintf("starts past the offset budget of %d rows", lim.MaxOffset), nil)
		}
		page = int(n)
	}

	switch count := values.Get("count"); count {
	case "", "none":
	case "exact":
		out.Count = true
	default:
		return out, badRequest("query.count", "count must be exact or none", []string{"exact", "none"})
	}

	out.Page, out.PerPage = page, perPage
	out.Query.Offset = (page - 1) * perPage
	// One row past the page, so has_more costs a row rather than a count.
	out.Query.Limit = perPage + 1
	return out, nil
}

// parseCondition reads one ` + "`op.value`" + ` — or a bare value, which is equality.
//
// Every Condition it builds is addressed by col.Name and every rejection it
// writes is located at col.wire(), which is the whole of the split: the
// predicate goes to Postgres and the error goes back to whoever sent the
// parameter.
func parseCondition(col Column, raw string) (Condition, error) {
	name, value, hasOp := strings.Cut(raw, ".")
	spec, known := operators[name]
	if !hasOp {
		// A bare "isnull" is the nullary form; anything else is a value.
		if spec, ok := operators[raw]; ok && spec.operands == 0 {
			return Condition{Column: col.Name, Op: spec.sql}, nil
		}
		known = false
	}
	if !known {
		// Not an operator, so the whole string is the value — which is what
		// makes ?status=draft mean equality, and what keeps a value containing
		// a dot working.
		if err := withinLength(col, raw); err != nil {
			return Condition{}, err
		}
		v, err := col.Parse(raw)
		if err != nil {
			return Condition{}, badRequest("query."+col.wire(), err.Error(), nil)
		}
		return Condition{Column: col.Name, Op: OpEq, Value: v}, nil
	}

	switch spec.operands {
	case 0:
		return Condition{Column: col.Name, Op: spec.sql}, nil
	case 2:
		lo, hi, ok := strings.Cut(value, ",")
		if !ok {
			return Condition{}, badRequest("query."+col.wire(), "between takes two values separated by a comma", nil)
		}
		if err := withinLength(col, lo, hi); err != nil {
			return Condition{}, err
		}
		loV, err := col.Parse(lo)
		if err != nil {
			return Condition{}, badRequest("query."+col.wire(), err.Error(), nil)
		}
		hiV, err := col.Parse(hi)
		if err != nil {
			return Condition{}, badRequest("query."+col.wire(), err.Error(), nil)
		}
		return Condition{Column: col.Name, Op: spec.sql, Value: loV, Value2: hiV}, nil
	case -1:
		parts := strings.Split(value, ",")
		if len(parts) > maxListValues {
			return Condition{}, badRequest("query."+col.wire(),
				fmt.Sprintf("operator %q was given %d values, the limit is %d", name, len(parts), maxListValues), nil)
		}
		if err := withinLength(col, parts...); err != nil {
			return Condition{}, err
		}
		vals := make([]any, 0, len(parts))
		for _, part := range parts {
			v, err := col.Parse(part)
			if err != nil {
				return Condition{}, badRequest("query."+col.wire(), err.Error(), nil)
			}
			vals = append(vals, v)
		}
		return Condition{Column: col.Name, Op: spec.sql, Values: vals}, nil
	default:
		if err := withinLength(col, value); err != nil {
			return Condition{}, err
		}
		if spec.pattern != "" {
			// A pattern operator is text, and the value is escaped so that a %
			// or an _ in it matches itself rather than becoming a wildcard.
			return Condition{Column: col.Name, Op: spec.sql,
				Value: fmt.Sprintf(spec.pattern, escapeLike(value))}, nil
		}
		v, err := col.Parse(value)
		if err != nil {
			return Condition{}, badRequest("query."+col.wire(), err.Error(), nil)
		}
		return Condition{Column: col.Name, Op: spec.sql, Value: v}, nil
	}
}

// withinLength bounds each operand, so a filter value cannot be used to make a
// scan arbitrarily expensive. The pattern operators pass their operand through
// to LIKE, which is what makes a long one worth refusing rather than merely
// odd.
func withinLength(col Column, values ...string) error {
	for _, v := range values {
		if len(v) > maxValueLength {
			return badRequest("query."+col.wire(),
				fmt.Sprintf("value is %d bytes, the limit is %d", len(v), maxValueLength), nil)
		}
	}
	return nil
}

// escapeLike neutralises the pattern metacharacters in a user's search term.
func escapeLike(s string) string {
	r := strings.NewReplacer(@@\@@, @@\\@@, "%", @@\%@@, "_", @@\_@@)
	return r.Replace(s)
}

// parseID reads the {id} path segment with the primary key's own parser. The
// key is named by its column, because the handler was emitted from the schema
// rather than parsed out of a query string.
func parseID(raw string, cols []Column, pk string) (any, error) {
	col, ok := findByColumn(cols, pk)
	if !ok {
		return raw, nil
	}
	v, err := col.Parse(raw)
	if err != nil {
		return nil, badRequest("path.id", err.Error(), nil)
	}
	return v, nil
}

// Problem is the error body, RFC 9457 shaped — the same one sqlb served, so a
// client's error handling does not change on the way out.
type Problem struct {
	Type   string           @@json:"type,omitempty"@@
	Title  string           @@json:"title,omitempty"@@
	Status int              @@json:"status,omitempty"@@
	Detail string           @@json:"detail,omitempty"@@
	Errors []*ProblemDetail @@json:"errors,omitempty"@@
}

// ProblemDetail is one rejected parameter or field. Allowed carries what would
// have worked instead, which is the half of an error message that saves a round
// trip.
type ProblemDetail struct {
	Message  string   @@json:"message"@@
	Location string   @@json:"location,omitempty"@@
	Allowed  []string @@json:"allowed,omitempty"@@
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Detail
	}
	return p.Title
}

func badRequest(location, message string, allowed []string) *Problem {
	return &Problem{
		Status: http.StatusBadRequest,
		Title:  http.StatusText(http.StatusBadRequest),
		Detail: message,
		Errors: []*ProblemDetail{{Message: message, Location: location, Allowed: allowed}},
	}
}

// WriteProblem writes an error response. Anything that is not already a
// Problem, and is not an integrity violation the database named, is a 500 whose
// detail is not the caller's business.
func WriteProblem(w http.ResponseWriter, err error) {
	var p *Problem
	switch {
	case errors.As(err, &p):
	default:
		p = constraintProblem(err)
	}
	if p == nil {
		p = &Problem{
			Status: http.StatusInternalServerError,
			Title:  http.StatusText(http.StatusInternalServerError),
			Detail: "internal server error",
		}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// constraintProblem answers a refused write in the terms of the request that
// caused it, or nil when the error is not one.
//
// Before the eject, a duplicate unique value answered 409 and an FK, check or
// not-null violation answered 422, classified off SQLSTATE class 23. Without
// this the same request answered 500 here, so a client-side retry loop keyed on
// 409 broke quietly on the day of the eject (#70). The mapping is small and
// dependency-free, and this file already imports pgx, so carrying it costs less
// than documenting its absence.
//
// A unique or exclusion violation is 409: the request is well formed and would
// be valid against a different state of the database. The others are 422 — the
// entity itself is wrong, and no amount of waiting makes a row referencing a
// product that does not exist into a row that does.
//
// The constraint's name is deliberately not in the body: put in a response it
// becomes a way to enumerate a schema's indexes by provoking them.
func constraintProblem(err error) *Problem {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) != 5 || pgErr.Code[:2] != "23" {
		return nil
	}
	switch pgErr.Code {
	case "23505", "23P01": // unique_violation, exclusion_violation
		return &Problem{
			Status: http.StatusConflict,
			Title:  http.StatusText(http.StatusConflict),
			Detail: "this conflicts with a row that already exists",
		}
	default: // foreign_key, check, not_null
		return &Problem{
			Status: http.StatusUnprocessableEntity,
			Title:  http.StatusText(http.StatusUnprocessableEntity),
			Detail: "this breaks a rule the database enforces",
		}
	}
}

// WriteJSON writes a success response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// notFound is the 404 an id that matched nothing produces. It says nothing
// about whether the row exists outside the caller's scope, because that
// distinction is exactly what a tenant boundary must not leak.
func notFound(resource string) *Problem {
	return &Problem{
		Status: http.StatusNotFound,
		Title:  http.StatusText(http.StatusNotFound),
		Detail: "no " + resource + " matched",
	}
}

// readBody reads a request body, capped so that a handler cannot be made to
// buffer an arbitrary amount of memory. The cap is generous and editable, and
// it is here rather than absent because this is a file somebody now owns.
func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, badRequest("body", "could not read the request body", nil)
	}
	if int64(len(data)) > maxBodyBytes {
		return nil, &Problem{
			Status: http.StatusRequestEntityTooLarge,
			Title:  http.StatusText(http.StatusRequestEntityTooLarge),
			Detail: fmt.Sprintf("request body is larger than %d bytes", maxBodyBytes),
		}
	}
	return data, nil
}

// maxBodyBytes caps a request body at one megabyte.
const maxBodyBytes int64 = 1 << 20

// confineFor runs a resource's Confine hook, if it has one.
func confineFor(r *http.Request, confine func(*http.Request) ([]Condition, error)) ([]Condition, error) {
	if confine == nil {
		return nil, nil
	}
	return confine(r)
}

// assignFor runs a resource's Assign hook, if it has one.
func assignFor(r *http.Request, assign func(*http.Request) (map[string]any, error)) (map[string]any, error) {
	if assign == nil {
		return nil, nil
	}
	return assign(r)
}
`
