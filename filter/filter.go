package filter

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mind-vm/sqlb"
)

// Defaults applied when Options leaves a limit unset. They are deliberately
// conservative: an unbounded list endpoint is a denial-of-service waiting for a
// client that forgets to paginate.
const (
	DefaultPageSize = 25
	MaxPageSize     = 200
	MaxFilters      = 24
	MaxSortTerms    = 4
	MaxGroupDepth   = 3
	// MaxListValues bounds one `in`/`nin` list. A list is a single condition
	// against MaxFilters however long it is, so without this the budget is
	// bypassed by writing ?id=in.1,2,3,… — one parameter, one predicate, and a
	// bind parameter per member until the driver's 65535 runs out.
	MaxListValues = 100
	// MaxValueLength bounds one filter value or search term. The pattern
	// operators pass their operand through unescaped on purpose, so a value is
	// a lever on how much work a scan does, and a long one is a cheap way to
	// pull that lever.
	MaxValueLength = 256
	// MaxOffset bounds how far into a result set offset paging may reach.
	// Offset paging is the one untrusted-input dimension the grammar left
	// open, and it is the cheapest per-request scan-cost lever it has:
	// `?page=50000000` asks Postgres to produce and discard ten billion rows
	// before returning a page of twenty-five. Cursor paging has no such cost,
	// but it is opt-in per request, so it is not a bound.
	//
	// Generous on purpose — a hundred thousand rows is past where offset paging
	// is a good idea and well past where any human is browsing — because the
	// point is to have a ceiling, not to pick the right depth for a resource.
	// Override it per resource like the others.
	MaxOffset = 100_000
)

// Options configures parsing for one resource.
type Options struct {
	// Model supplies the columns and their capabilities. Required.
	Model *sqlb.Model

	DefaultPageSize int
	// MaxFilters bounds the number of leaf conditions a request may ask for,
	// counting the ones inside `or=`/`and=` groups. Counting top-level
	// parameters instead would leave the budget open to a single group holding
	// as many conditions as the client cared to write.
	MaxFilters   int
	MaxPageSize  int
	MaxSortTerms int
	// MaxListValues bounds one `in`/`nin` list; MaxValueLength bounds one
	// filter value or search term.
	MaxListValues  int
	MaxValueLength int
	// MaxOffset bounds how deep ?page= and ?offset= may reach. A request past
	// it is refused with a message pointing at ?cursor=, which has no such
	// cost.
	MaxOffset int

	// Expandable lists the relation names ?expand may name. Parsing validates
	// against it and Apply performs the join, so a parsed ?expand is never
	// silently dropped: a name that is not here is a 400 listing the ones that
	// are.
	//
	// The rest package validates these against the model at startup, so a
	// relation that cannot be expanded is a mounting error rather than a
	// request-time surprise.
	Expandable []string

	// Computed lists the computed columns this resource is willing to pay for.
	// Empty means none, and none is the default on purpose.
	//
	// A computed column is declared on the model, which is shared, and wanted
	// by one screen. Projecting every declared one attached a correlated
	// subquery per column to every read of the model, and a column carrying a
	// Needs bind made unrelated reads fail outright (#92). So declaring stays
	// global — the expression, its type, its binds — and *selecting* is per
	// resource, beside the other things a mount already decides.
	//
	// A column not listed here is not reachable from this resource at all: not
	// projected, not filterable, not sortable, and not nameable in ?select.
	// Being unreachable rather than merely unprojected is what keeps the cost
	// opt-in, since a filter on a correlated subquery costs what the projection
	// would have.
	Computed []string

	// Columns narrows this resource to the columns it names. Empty means every
	// column the model has, which is the default and what almost every resource
	// wants.
	//
	// It is the same per-resource reachability Computed has, generalised to
	// stored columns, and it is here because a model is shared in the other
	// direction too: one table, two surfaces, and the privileged one is the
	// reason the sensitive column exists (#148). A public catalogue and an
	// admin panel over the same products differ in which columns each may see,
	// and Hidden cannot say that — Hidden is a property of the model, and there
	// is one model.
	//
	// A column not listed is not reachable from this resource at all: not
	// projected, not filterable, not sortable, not nameable in ?select, not
	// searched by ?search, and not named in the list a rejection offers. That
	// last one matters — a narrowed resource that advertised the column it is
	// about to refuse would leak the schema it was narrowed to hide.
	//
	// Names are column names, as Computed's are. The rest package checks them
	// against the model at startup, where a typo is a resource missing a column
	// rather than a request-time surprise.
	Columns []string

	// DefaultSort is the ordering a request that names no ?sort gets: column
	// names, a leading "-" for descending, most significant first.
	//
	//	DefaultSort: []string{"-pinned", "-published_at", "-created_at"}
	//
	// The direction syntax is ?sort's. The names are column names, as
	// Columns' and Computed's are — identical to the wire spelling unless the
	// schema declared a WireCase, and a declaration should not have to be
	// written in the front end's casing.
	//
	// Empty means primary-key order, which is what silence meant before this
	// existed — the difference is that the answer is now declared rather than
	// being an implementation detail nothing could state (#165). For many
	// resources the ordering is part of what the collection *is*: a feed in
	// primary-key order is not the feed, and every caller restating it on every
	// request is a rule one caller can forget and get a well-formed 200 for.
	//
	// It is not a bound and is not charged against MaxSortTerms, which exists to
	// cap what an untrusted request may ask for. A ?sort of any kind replaces it
	// outright rather than being appended to it; the primary-key tiebreak is
	// added afterwards either way, so cursors work unchanged.
	//
	// Every term must name a column this resource can sort by. The rest package
	// checks that where a resource is mounted, so a default naming a column that
	// is not Sortable is a startup failure rather than a 400 blaming whoever sent
	// the first request.
	DefaultSort []string

	// DisableSearch rejects ?search even when columns are searchable.
	DisableSearch bool
}

// reachable reports whether a column may be reached from this resource.
//
// Two independent narrowings, and a column has to pass both: Columns, which is
// the surface this mount serves at all, and Computed, which is the derived
// columns it is willing to pay for. Empty means "no narrowing" in each case,
// so the default is every stored column and no computed one.
func (o Options) reachable(col *sqlb.ColumnInfo) bool {
	if col == nil {
		return true
	}
	if len(o.Columns) > 0 && !contains(o.Columns, col.Name) {
		return false
	}
	if !col.Computed() {
		return true
	}
	return contains(o.Computed, col.Name)
}

func (o Options) defaultPageSize() int {
	if o.DefaultPageSize > 0 {
		return o.DefaultPageSize
	}
	return DefaultPageSize
}

func (o Options) maxPageSize() int {
	if o.MaxPageSize > 0 {
		return o.MaxPageSize
	}
	return MaxPageSize
}

func (o Options) maxFilters() int {
	if o.MaxFilters > 0 {
		return o.MaxFilters
	}
	return MaxFilters
}

func (o Options) maxSortTerms() int {
	if o.MaxSortTerms > 0 {
		return o.MaxSortTerms
	}
	return MaxSortTerms
}

func (o Options) maxListValues() int {
	if o.MaxListValues > 0 {
		return o.MaxListValues
	}
	return MaxListValues
}

func (o Options) maxValueLength() int {
	if o.MaxValueLength > 0 {
		return o.MaxValueLength
	}
	return MaxValueLength
}

func (o Options) maxOffset() int {
	if o.MaxOffset > 0 {
		return o.MaxOffset
	}
	return MaxOffset
}

// Query is a parsed request: predicates, ordering, projection and pagination,
// all already validated against the model.
type Query struct {
	Where  []sqlb.Pred
	Order  []sqlb.Order
	Select []string
	Expand []string
	Search string

	Page     int
	PageSize int
	Limit    int
	Offset   int

	// Computed names the computed columns this resource selects, copied from
	// Options so that Apply projects exactly what parsing validated against.
	Computed []string

	// Columns is the resource's surface, copied from Options for the same
	// reason: the default projection is built in Apply, and a narrowed resource
	// whose parser refused a column while its projection selected it anyway
	// would read the value out of the database on every request and drop it on
	// the way out — which is a narrowing in the response only, and not the one
	// Options.Columns describes.
	Columns []string

	// Cursor is the keyset position `?cursor=` asked to resume from, empty for
	// the first page. It is the alternative to Page and Offset rather than an
	// addition to them: a request carrying both is refused, since the two
	// answer the same question with different answers.
	Cursor sqlb.Cursor
}

// Apply writes the parsed query onto a builder.
//
// Apply owns the projection. Given ?select it uses those columns; otherwise it
// projects every non-hidden column. It does not fall back to the builder's
// default of "all mapped columns", because that would put a Hidden column into
// a REST response any time a handler forgot to project. A caller wanting a
// custom projection should apply Where, Order and the limits from the Query
// fields directly instead.
//
// An expansion is applied as a relation join. Apply does this rather than
// refusing, and the projection below is why it can: ?select names columns of T,
// and an expanded relation is not one — it arrives as its own JSON value in a
// column the scanner recognises, so the row stays exactly as wide as T.
func Apply[T any](b *sqlb.Builder[T], q *Query) *sqlb.Builder[T] {
	b.Where(q.Where...)
	b.Expand(q.Expand...)

	// Ordering is settled before the projection, because Stable may add a term
	// and the projection has to cover whatever the ordering ended up being.
	b.OrderBy(q.Order...)
	b.Stable()

	names := make([]string, 0, len(q.Select))
	if len(q.Select) > 0 {
		names = append(names, q.Select...)
	} else {
		// Every non-hidden column, minus the computed ones this resource did
		// not ask for. Without the second half a correlated subquery declared
		// for one screen is attached to every list of the model, and one
		// carrying a Needs bind fails the request outright (#92).
		selects := make(map[string]bool, len(q.Computed))
		for _, name := range q.Computed {
			selects[name] = true
		}
		for _, col := range b.Model().Selectable() {
			// Both narrowings, in the order Options.reachable applies them.
			// A column outside Columns is not this resource's to read at all
			// (#148); a computed one it did not ask for is a cost it declined.
			if len(q.Columns) > 0 && !contains(q.Columns, col.Name) {
				continue
			}
			if col.Computed() && !selects[col.Name] {
				continue
			}
			names = append(names, col.Name)
		}
	}
	names = append(names, unprojectedOrderColumns(b, names)...)

	items := make([]sqlb.Selectable, len(names))
	for i, name := range names {
		items[i] = sqlb.F(name)
	}
	b.ClearSelect().Select(items...)

	b.After(q.Cursor)
	return b.Limit(q.Limit).Offset(q.Offset)
}

// unprojectedOrderColumns names the ordering columns a projection would leave
// out.
//
// A cursor is built by reading the ordering columns off the last row, so
// `?select=id&sort=created_at` has to fetch created_at even though the response
// will not show it — otherwise the cursor would encode a zero time and the next
// page would start from the beginning. Selecting more than the response shows is
// safe here and nowhere else: rest marshals from the request's ?select, not
// from the columns the statement happened to read.
func unprojectedOrderColumns[T any](b *sqlb.Builder[T], projected []string) []string {
	have := make(map[string]bool, len(projected))
	for _, name := range projected {
		have[name] = true
	}
	var out []string
	for _, name := range b.OrderColumns() {
		if have[name] {
			continue
		}
		have[name] = true
		out = append(out, name)
	}
	return out
}

// TreeParam is the query parameter a JSON filter tree travels in when a request
// carries the URL grammar and a tree at once (see [ParseFilterTree]). Parse does
// not read it — a tree is the REST layer's to compile — but the parameter is
// reserved so the URL grammar never mistakes it for a column, letting the two
// filter formats share one request.
const TreeParam = "filter"

// reserved parameter names, which never name a column.
//
// "count" is reserved but unused here: it asks a list endpoint for a total row
// count, which costs a second query and so is the REST layer's decision rather
// than the parser's. It is listed anyway, because a column named `count` would
// otherwise shadow it and the collision would only surface once someone asked
// for a total. TreeParam is here for the same reason: Parse skips it, but a
// column named `filter` must not shadow the tree a request may also carry.
var reserved = map[string]bool{
	"select": true, "sort": true, "order": true, "search": true,
	"expand": true, "limit": true, "offset": true, "page": true,
	"per_page": true, "or": true, "and": true, "not": true,
	"count": true, "cursor": true, TreeParam: true,
}

// singleValued names the reserved parameters that mean one thing per request.
// Everything reserved is here except the group parameters — "or", "and" and
// "not" — which conjoin by design: several of them is a request with several
// groups, not a request that said the same thing twice. Read through the
// negation that is still the same rule, since several `not` groups are
// NOT A AND NOT B.
//
// It exists because the parser reads these with [firstValue], which is
// url.Values.Get and therefore drops every occurrence after the first. That is
// the one place the package ignored input rather than refusing it: `?sort=a&sort=b`
// sorted by `a` and said nothing, while a repeated *per-column* filter parameter
// conjoins — an asymmetry a caller cannot see from the outside.
var singleValued = map[string]bool{
	"select": true, "sort": true, "order": true, "search": true,
	"expand": true, "limit": true, "offset": true, "page": true,
	"per_page": true, "count": true, "cursor": true, TreeParam: true,
}

// refuseRepeats reports every single-valued reserved parameter a request sent
// more than once, phrased like the cursor/page refusal: name what was sent and
// say what to do about it.
func (p *parser) refuseRepeats(values url.Values) {
	for _, key := range sortedKeys(values) {
		if !singleValued[key] {
			continue
		}
		if n := len(values[key]); n > 1 {
			p.errf(key, values[key][0],
				"sent %d times; %s takes one value per request", n, key)
		}
	}
}

// Parse compiles URL query parameters into a Query.
//
// Every problem found is reported, not just the first, so a caller fixing a
// request sees the whole list at once.
func Parse(values url.Values, opts Options) (*Query, error) {
	if opts.Model == nil {
		return nil, fmt.Errorf("filter: Options.Model is required")
	}
	p := &parser{opts: opts, model: opts.Model}
	q := &Query{PageSize: opts.defaultPageSize(), Computed: opts.Computed, Columns: opts.Columns}

	// Before anything is read, because what follows reads only the first
	// occurrence of each of these and the rest would vanish unremarked.
	p.refuseRepeats(values)

	// Filters, in sorted parameter order so the generated SQL is stable.
	for _, key := range sortedKeys(values) {
		if reserved[key] {
			continue
		}
		col := p.filterableColumn(key)
		if col == nil {
			continue
		}
		for _, raw := range values[key] {
			if pred, ok := p.parseCondition(col, raw, key); ok {
				q.Where = append(q.Where, pred)
			}
		}
	}

	for _, raw := range values["or"] {
		if pred, ok := p.parseGroup("or", raw, 0); ok {
			q.Where = append(q.Where, pred)
		}
	}
	for _, raw := range values["and"] {
		if pred, ok := p.parseGroup("and", raw, 0); ok {
			q.Where = append(q.Where, pred)
		}
	}
	for _, raw := range values["not"] {
		if pred, ok := p.parseGroup("not", raw, 0); ok {
			q.Where = append(q.Where, pred)
		}
	}

	// A JSON filter tree in ?filter= is compiled by this same parser, so its
	// conditions draw on the one MaxFilters budget the parameters above drew on,
	// and its problems join the same error list. Splitting a request's
	// conditions across the two formats therefore buys no extra budget.
	if raw := firstValue(values, TreeParam); raw != "" {
		if pred, ok := p.compileTree([]byte(raw)); ok {
			q.Where = append(q.Where, pred)
		}
	}

	// The budget is charged per leaf condition inside build, so a group full of
	// conditions costs what the same conditions cost written out.

	if s := firstValue(values, "search"); s != "" {
		q.Search = s
		if pred, ok := p.parseSearch(s); ok {
			q.Where = append(q.Where, pred)
		}
	}

	q.Order = p.parseSort(firstValue(values, "sort", "order"))
	q.Select = p.parseSelect(firstValue(values, "select"))
	q.Expand = p.parseExpand(firstValue(values, "expand"))
	p.parsePagination(values, q)

	if len(p.errs) > 0 {
		return nil, p.errs
	}
	return q, nil
}

type parser struct {
	opts  Options
	model *sqlb.Model
	errs  Errors
	// conditions counts every leaf condition the request asked for, wherever
	// it was written. A group is one entry in Query.Where and any number of
	// conditions, so counting entries would bound the wrong thing.
	conditions int
	// overBudget stops the count being reported once per condition after the
	// limit, which would answer a pathological request with a pathological
	// error document.
	overBudget bool
}

// withinLength bounds one operand, recording an error when it is over.
func (p *parser) withinLength(value, param, raw string) bool {
	if len(value) <= p.opts.maxValueLength() {
		return true
	}
	p.errf(param, raw, "value is %d bytes, the limit is %d",
		len(value), p.opts.maxValueLength())
	return false
}

// charge records one leaf condition against the budget, reporting the first
// time it is exceeded and refusing every condition after it.
//
// It charges before the condition is parsed rather than after it succeeds,
// because the work being bounded is the parsing: a request full of malformed
// conditions costs the same as one full of valid ones.
func (p *parser) charge() bool {
	p.conditions++
	if p.conditions <= p.opts.maxFilters() {
		return true
	}
	if !p.overBudget {
		p.overBudget = true
		p.errf("filter", "", "%d filter conditions requested, the limit is %d",
			p.conditions, p.opts.maxFilters())
	}
	return false
}

func (p *parser) errf(param, value, format string, args ...any) {
	p.errs = append(p.errs, &Error{
		Param:  param,
		Value:  value,
		Reason: fmt.Sprintf(format, args...),
	})
}

func (p *parser) errAllowed(param, value, reason string, allowed []string) {
	p.errs = append(p.errs, &Error{Param: param, Value: value, Reason: reason, Allowed: allowed})
}

// filterableColumn resolves a parameter name to a column the request is
// permitted to filter on, recording an error otherwise.
func (p *parser) filterableColumn(name string) *sqlb.ColumnInfo {
	// ColumnByWire, not Column: the name arrived from a request, and a request
	// spells a column the way the wire does. They are the same string unless
	// the schema declared a WireCase (ADR-0036's amendment).
	col := p.model.ColumnByWire(name)
	// A hidden or write-only column is reported as unknown rather than as
	// un-filterable, so that its existence cannot be probed by reading the
	// rejection. A computed column this resource does not select is unknown
	// in the plainer sense: it is declared on the model, and this endpoint
	// does not have it (#92).
	if col == nil || col.Hidden || col.WriteOnly || !p.opts.reachable(col) {
		p.errAllowed(name, "", "unknown parameter", p.capable(capFilter))
		return nil
	}
	if !col.Filterable {
		p.errAllowed(name, "", "column is not filterable", p.capable(capFilter))
		return nil
	}
	return col
}

type capability int

const (
	capFilter capability = iota
	capSort
	capSearch
)

// capable lists the columns carrying a capability, for error messages. Telling
// a caller what it may ask for is what turns a 400 into a usable answer.
func (p *parser) capable(c capability) []string {
	var out []string
	for _, col := range p.model.Columns {
		// A computed column this resource does not select is not part of its
		// surface, so it is absent from the "allowed" lists too — naming it in
		// a rejection would advertise a column every request for it is about
		// to be refused for (#92).
		if col.Hidden || col.WriteOnly || !p.opts.reachable(col) {
			continue
		}
		// Wire, not Name: this list is what a caller is told it may type, and
		// the two differ whenever the schema declared a WireCase.
		switch c {
		case capFilter:
			if col.Filterable {
				out = append(out, col.Wire)
			}
		case capSort:
			if col.Sortable {
				out = append(out, col.Wire)
			}
		case capSearch:
			if col.Searchable {
				out = append(out, col.Wire)
			}
		}
	}
	return out
}

// parseCondition parses one `op.value` (or bare value) against a column.
func (p *parser) parseCondition(col *sqlb.ColumnInfo, raw, param string) (sqlb.Pred, bool) {
	op, value := splitOp(raw)
	return p.build(col, op, value, param, raw)
}

// splitOp separates a leading operator from its operand. A prefix is only
// treated as an operator when it names one, so `email=alice@example.com` and
// `date=2024-01-02` are read as equality rather than as a malformed operator.
func splitOp(raw string) (op, value string) {
	if head, rest, found := strings.Cut(raw, "."); found {
		if _, known := operators[head]; known {
			return head, rest
		}
	}
	if _, known := operators[raw]; known {
		return raw, ""
	}
	return "eq", raw
}

type opKind int

const (
	opBinary opKind = iota
	opList
	opNullary
	opRange
	opPattern
	// opElem takes one element of an array column; opSet takes a list of them.
	opElem
	opSet
	// opDoc takes a JSON document and asks a jsonb column to contain it.
	opDoc
	// opDay takes a calendar date and asks a timestamp column for that whole
	// day, which equality cannot ask (#241).
	opDay
)

var operators = map[string]opKind{
	"eq": opBinary, "ne": opBinary, "neq": opBinary,
	"gt": opBinary, "gte": opBinary, "lt": opBinary, "lte": opBinary,
	"in": opList, "nin": opList,
	"isnull": opNullary, "notnull": opNullary,
	"between": opRange,
	"like":    opPattern, "ilike": opPattern,
	"contains": opPattern, "startswith": opPattern, "endswith": opPattern,

	// Array containment. `contains` is deliberately not reused: it is a text
	// pattern operator above, and one name meaning two things depending on the
	// column it is applied to is exactly the ambiguity the generated clients
	// exist to remove (ADR-0033).
	"has": opElem, "hasany": opSet, "hasall": opSet,

	// Document containment, and `contains` is not reused here either, for the
	// reason ADR-0033 gives about arrays: it already means case-insensitive
	// substring on text, and a third meaning dispatched on column type is the
	// same ambiguity. `hasdoc` joins the `has` family instead, which is what
	// containment is already spelled as here.
	"hasdoc": opDoc,

	// A whole calendar day, for a timestamp column. `eq` cannot express it: a
	// date compares against midnight, and a timestamp is almost never exactly
	// midnight, so the request that reads as "what is on this day" matched
	// nothing and said nothing (#241). Not spelled as a bare date against `eq`
	// for the reason ADR-0033 gives about `contains`: one operator meaning two
	// things depending on the operand's shape is the ambiguity this grammar
	// exists to remove.
	"day": opDay,

	// The negations of the four. The JSON tree can spell these with a `not`
	// group, but the URL grammar conjoins by design and has nowhere to put one,
	// so without these an array or document column is filterable in one
	// direction only — and ADR-0003's claim that the two frontends compile to
	// the same predicate would hold for every column kind but these.
	//
	// The `n` prefix is `nin`'s, chosen over `not`-prefixing so the rule is
	// derivable from one example and any later `has*` operator gets its
	// negation for free. Each is three-valued, not complementary: a NULL column
	// satisfies neither the operator nor its negation, the same way `nin`
	// already behaves.
	"nhas": opElem, "nhasany": opSet, "nhasall": opSet,
	"nhasdoc": opDoc,
}

func (p *parser) build(col *sqlb.ColumnInfo, op, value, param, raw string) (sqlb.Pred, bool) {
	if !p.charge() {
		return sqlb.Pred{}, false
	}
	f := sqlb.F(col.Name)
	kind, known := operators[op]
	if !known {
		p.errAllowed(param, raw, fmt.Sprintf("unknown operator %q", op), operatorNames())
		return sqlb.Pred{}, false
	}
	// Lists and ranges hold several operands in one value, so they are measured
	// per member where they are split rather than in aggregate here.
	if kind != opList && kind != opRange && kind != opSet && !p.withinLength(value, param, raw) {
		return sqlb.Pred{}, false
	}
	// The shorthand form names no operator, so splitOp inferred the "eq" here.
	elem, isArray, ok := p.gateColumnKind(col, op, kind, op == "eq" && value == raw, param, raw)
	if !ok {
		return sqlb.Pred{}, false
	}
	// Before coercion, because coercion is what loses the distinction: a date
	// and a timestamp are one Go value afterwards.
	if !p.refuseBareDate(col, op, splitTopLevel(value, ','), param, raw) {
		return sqlb.Pred{}, false
	}
	operands, ok := p.urlOperands(col, elem, isArray, op, kind, value, param, raw)
	if !ok {
		return sqlb.Pred{}, false
	}
	return p.applyOp(col, f, op, kind, isArray, operands, param, raw)
}

// gateColumnKind rejects an operator that does not fit the kind of column it
// was applied to — array, document or scalar — naming the allowed alternatives,
// and reports whether the column is an array (with its element type) so the
// caller coerces operands to the right type. The refusal is written here rather
// than left to Postgres, which would report a type error from a statement the
// caller cannot see.
//
// The whole-array ordering operators are refused here too: they share opKind
// opBinary with the eq/ne that arrays do accept, so the operator table alone
// cannot separate them, and refusing before extraction keeps the error the
// caller sees the same whether or not the operand also fails to coerce.
//
// A document column is gated the same way and for the same reason, with one
// difference worth naming: it has no shorthand form. `?metadata={"lang":"de"}`
// infers the "eq" that splitOp supplies, so quoting that operator back would
// name a word the request never used — shorthand says the column needs an
// operator instead. Only the URL frontend can produce that spelling; the JSON
// tree always names its operator, and passes false.
func (p *parser) gateColumnKind(col *sqlb.ColumnInfo, op string, kind opKind, shorthand bool, param, raw string) (reflect.Type, bool, bool) {
	elem, isArray := arrayElem(col)
	isDoc := isJSONColumn(col)
	switch {
	case isArray && !arrayOperators[kind],
		isArray && kind == opBinary && op != "eq" && op != "ne" && op != "neq":
		p.errAllowed(param, raw, fmt.Sprintf("operator %q does not apply to the array column %s", op, col.Name), arrayOperatorNames())
		return nil, false, false
	case kind == opDay && !isTimeColumn(col):
		p.errf(param, raw, "operator %q needs a date or timestamp column, but %s is %s", op, col.Name, col.Type)
		return nil, false, false
	case !isArray && (kind == opElem || kind == opSet):
		p.errf(param, raw, "operator %q needs an array column, but %s is %s", op, col.Name, col.Type)
		return nil, false, false
	case isDoc && kind != opDoc && kind != opNullary:
		if shorthand {
			p.errAllowed(param, raw,
				fmt.Sprintf("%s is a JSON document column, which has no shorthand form; name an operator", col.Name),
				documentOperatorNames())
			return nil, false, false
		}
		p.errAllowed(param, raw,
			fmt.Sprintf("operator %q does not apply to the JSON document column %s", op, col.Name),
			documentOperatorNames())
		return nil, false, false
	case !isDoc && kind == opDoc:
		p.errf(param, raw, "operator %q needs a JSON document column, but %s is %s", op, col.Name, col.Type)
		return nil, false, false
	}
	return elem, isArray, true
}

// documentOperatorNames is the set a jsonb column accepts, and the list a
// rejection offers back. It is short on purpose. The ordering operators compare
// documents by a rule almost nobody means, and the pattern operators would
// match against a serialisation whose key order and whitespace are Postgres's
// to choose — both would answer, which is worse than refusing.
func documentOperatorNames() []string {
	return []string{"hasdoc", "isnull", "nhasdoc", "notnull"}
}

// urlOperands turns one URL operand string into the coerced operands applyOp
// expects, honouring the per-member length budget and, for arrays, the same
// element-list rules `hasany` and a whole-array eq are held to. It is the URL
// frontend's half of the split with applyOp: extraction here, mapping there.
func (p *parser) urlOperands(col *sqlb.ColumnInfo, elem reflect.Type, isArray bool,
	op string, kind opKind, value, param, raw string) ([]any, bool) {

	switch kind {
	case opNullary:
		return nil, true

	case opPattern:
		return []any{value}, true

	case opElem:
		v, err := Coerce(unquote(value), elem)
		if err != nil {
			p.errf(param, value, "%v", err)
			return nil, false
		}
		return []any{v}, true

	case opSet:
		return p.arrayOperand(elem, value, param, raw, op)

	case opDay:
		if !isBareDate(value) {
			p.errf(param, raw, "operator %q takes a calendar date, e.g. %s=day.2026-09-01", op, col.Wire)
			return nil, false
		}
		return []any{value}, true

	case opDoc:
		doc := strings.TrimSpace(value)
		if doc == "" {
			p.errf(param, raw, "operator %q needs a JSON document, e.g. %s=hasdoc.{\"lang\":\"de\"}", op, col.Name)
			return nil, false
		}
		if !json.Valid([]byte(doc)) {
			p.errf(param, raw, "%s is a JSON document column and %q is not valid JSON", col.Name, doc)
			return nil, false
		}
		return []any{doc}, true

	case opList:
		parts := splitTopLevel(value, ',')
		if len(parts) == 0 {
			p.errf(param, raw, "operator %q needs at least one value", op)
			return nil, false
		}
		// One list is one condition however long it is, so the filter budget
		// does not bound it and this has to.
		if len(parts) > p.opts.maxListValues() {
			p.errf(param, raw, "operator %q was given %d values, the limit is %d",
				op, len(parts), p.opts.maxListValues())
			return nil, false
		}
		vals := make([]any, 0, len(parts))
		for _, part := range parts {
			if !p.withinLength(part, param, raw) {
				return nil, false
			}
			v, err := Coerce(unquote(part), col.Type)
			if err != nil {
				p.errf(param, part, "%v", err)
				return nil, false
			}
			vals = append(vals, v)
		}
		return vals, true

	case opRange:
		parts := splitTopLevel(value, ',')
		if len(parts) != 2 {
			p.errf(param, raw, "operator \"between\" needs exactly two values, got %d", len(parts))
			return nil, false
		}
		for _, part := range parts {
			if !p.withinLength(part, param, raw) {
				return nil, false
			}
		}
		lo, err := Coerce(unquote(parts[0]), col.Type)
		if err != nil {
			p.errf(param, parts[0], "%v", err)
			return nil, false
		}
		hi, err := Coerce(unquote(parts[1]), col.Type)
		if err != nil {
			p.errf(param, parts[1], "%v", err)
			return nil, false
		}
		return []any{lo, hi}, true

	default: // opBinary
		// A whole-array eq/ne binds an array literal built from an element list;
		// a scalar eq/ne binds one coerced value.
		if isArray {
			return p.arrayOperand(elem, value, param, raw, op)
		}
		v, err := Coerce(unquote(value), col.Type)
		if err != nil {
			p.errf(param, value, "%v", err)
			return nil, false
		}
		return []any{v}, true
	}
}

// applyOp maps a column, operator and already-coerced operands to a predicate.
// It is the single compiler both frontends terminate in: the URL parser
// (urlOperands) and the JSON tree (jsonOperands) each turn their own wire
// format into typed operands and then call this, so the two spell exactly the
// same predicate and cannot drift (ADR-0003). Operand shape is the extractor's
// contract: nullary takes none, binary/pattern one, range two, a list one or
// more; an array operator's operands are already coerced to the element type.
func (p *parser) applyOp(col *sqlb.ColumnInfo, f sqlb.Field, op string, kind opKind,
	isArray bool, operands []any, param, raw string) (sqlb.Pred, bool) {

	switch kind {
	case opNullary:
		if op == "isnull" {
			return f.IsNull(), true
		}
		return f.NotNull(), true

	case opElem:
		// `has` binds the element — `$1 = ANY(col)` — not an array constant.
		if op == "nhas" {
			return f.NotHas(operands[0]), true
		}
		return f.Has(operands[0]), true

	case opDay:
		// The operand is the date as written. OnDay casts it in Postgres rather
		// than parsing it here, because a Go time.Time is an instant and its
		// date depends on a time zone that is not the session's.
		return f.OnDay(operands[0]), true

	case opDoc:
		// Both frontends deliver the document as JSON text, which is what
		// ContainsJSON binds; the `::jsonb` cast is added there. The assertion
		// is checked rather than assumed: a third frontend that got the operand
		// shape wrong should be a 400 from here, not a panic in a request.
		doc, isText := operands[0].(string)
		if !isText {
			p.errf(param, raw, "operator %q needs a JSON document", op)
			return sqlb.Pred{}, false
		}
		if op == "nhasdoc" {
			return f.NotContainsJSON(doc), true
		}
		return f.ContainsJSON(doc), true

	case opSet:
		switch op {
		case "hasany":
			return f.HasAny(operands...), true
		case "nhasany":
			return f.NotHasAny(operands...), true
		case "nhasall":
			return f.NotHasAll(operands...), true
		}
		return f.HasAll(operands...), true

	case opPattern:
		if !isTextColumn(col) {
			p.errf(param, raw, "operator %q needs a text column, but %s is %s", op, col.Name, col.Type)
			return sqlb.Pred{}, false
		}
		s, _ := operands[0].(string)
		switch op {
		case "contains":
			return f.Contains(s), true
		case "startswith":
			return f.StartsWith(s), true
		case "endswith":
			return f.EndsWith(s), true
		case "like":
			return f.Like(s), true
		default:
			return f.ILike(s), true
		}

	case opList:
		if op == "in" {
			return f.OneOf(operands...), true
		}
		return f.NotOneOf(operands...), true

	case opRange:
		return f.Between(operands[0], operands[1]), true

	default: // opBinary
		if isArray {
			// gateArrayScalar has already refused everything but eq/ne, so the
			// operands are an element list that binds as a whole-array literal.
			if op == "eq" {
				return f.Eq(sqlb.Array(operands...)), true
			}
			return f.Neq(sqlb.Array(operands...)), true
		}
		v := operands[0]
		switch op {
		case "eq":
			return f.Eq(v), true
		case "ne", "neq":
			return f.Neq(v), true
		case "gt":
			return f.Gt(v), true
		case "gte":
			return f.Gte(v), true
		case "lt":
			return f.Lt(v), true
		default:
			return f.Lte(v), true
		}
	}
}

// arrayOperators is the set an array column accepts.
//
// Ordering and BETWEEN are absent because Postgres's array ordering is not a
// thing an API should offer; `in` is absent because a list of arrays has no
// spelling in this grammar; the pattern operators are absent because search is
// a text operation. Each of those is additive to allow later and breaking to
// withdraw, so the refusal is the starting position (ADR-0033).
var arrayOperators = map[opKind]bool{
	opElem:    true,
	opSet:     true,
	opNullary: true,
	opBinary:  true, // narrowed to eq/ne below; the ordering four are refused there
}

// arrayOperand parses the comma-separated element list an array-valued operator
// takes, under the same per-member limits a value list is held to.
func (p *parser) arrayOperand(elem reflect.Type, value, param, raw, op string) ([]any, bool) {
	parts := splitTopLevel(value, ',')
	if len(parts) > p.opts.maxListValues() {
		p.errf(param, raw, "operator %q was given %d values, the limit is %d",
			op, len(parts), p.opts.maxListValues())
		return nil, false
	}
	// Unlike `in`, an empty list is meaningful: it is the empty array, which
	// every array contains and none overlaps.
	if len(parts) == 1 && strings.TrimSpace(parts[0]) == "" {
		return nil, true
	}
	vals := make([]any, 0, len(parts))
	for _, part := range parts {
		if !p.withinLength(part, param, raw) {
			return nil, false
		}
		v, err := Coerce(unquote(part), elem)
		if err != nil {
			p.errf(param, part, "%v", err)
			return nil, false
		}
		vals = append(vals, v)
	}
	return vals, true
}

// arrayElem reports whether the column is a Postgres array, and its element
// type. bytea and json.RawMessage are []byte and are not arrays.
func arrayElem(col *sqlb.ColumnInfo) (reflect.Type, bool) {
	t := col.Type
	if t == nil || t.Kind() != reflect.Slice || t.Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	return t.Elem(), true
}

func arrayOperatorNames() []string {
	return []string{
		"eq", "has", "hasall", "hasany", "isnull", "ne", "neq",
		"nhas", "nhasall", "nhasany", "notnull",
	}
}

// parseGroup parses `(cond,cond,...)` where each condition is
// `column.op.value` or a nested `or(...)` / `and(...)`.
func (p *parser) parseGroup(param, raw string, depth int) (sqlb.Pred, bool) {
	if depth > MaxGroupDepth {
		p.errf(param, raw, "filter groups nested deeper than %d levels", MaxGroupDepth)
		return sqlb.Pred{}, false
	}
	body, ok := strings.CutPrefix(strings.TrimSpace(raw), "(")
	if !ok || !strings.HasSuffix(body, ")") {
		p.errf(param, raw, "expected a parenthesised group such as (status.eq.active,age.gt.18)")
		return sqlb.Pred{}, false
	}
	body = strings.TrimSuffix(body, ")")

	var preds []sqlb.Pred
	for _, item := range splitTopLevel(body, ',') {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if inner, isNested := strings.CutPrefix(item, "or"); isNested && strings.HasPrefix(inner, "(") {
			if sub, ok := p.parseGroup("or", inner, depth+1); ok {
				preds = append(preds, sub)
			}
			continue
		}
		if inner, isNested := strings.CutPrefix(item, "and"); isNested && strings.HasPrefix(inner, "(") {
			if sub, ok := p.parseGroup("and", inner, depth+1); ok {
				preds = append(preds, sub)
			}
			continue
		}
		if inner, isNested := strings.CutPrefix(item, "not"); isNested && strings.HasPrefix(inner, "(") {
			if sub, ok := p.parseGroup("not", inner, depth+1); ok {
				preds = append(preds, sub)
			}
			continue
		}

		name, rest, found := strings.Cut(item, ".")
		if !found {
			p.errf(param, item, "expected column.operator.value")
			continue
		}
		col := p.filterableColumn(name)
		if col == nil {
			continue
		}
		op, value, found := strings.Cut(rest, ".")
		if !found {
			// Allows the nullary forms, e.g. deleted_at.isnull.
			op, value = rest, ""
		}
		if pred, ok := p.build(col, op, value, param, item); ok {
			preds = append(preds, pred)
		}
	}

	if len(preds) == 0 {
		return sqlb.Pred{}, false
	}
	switch param {
	case "or":
		return sqlb.Or(preds...), true
	case "not":
		// A parenthesised group is variadic by syntax, so `not(a,b)` has to
		// mean something. NOT (a AND b) is the reading that keeps the default
		// conjunction a group already carries, and it makes `?not=(…)` the
		// exact inverse of `?and=(…)`.
		//
		// The JSON tree refuses a second child here rather than choosing, and
		// the difference is deliberate: there the explicit wrapper costs one
		// node, while here it would cost the terseness this grammar exists for.
		// Both spell the same set — `?not=(a,b)` is the tree's `not` over an
		// `and` — so no filter is expressible in one and not the other.
		return sqlb.Not(sqlb.And(preds...)), true
	}
	return sqlb.And(preds...), true
}

// parseSearch fans a term out across every searchable column.
func (p *parser) parseSearch(term string) (sqlb.Pred, bool) {
	if p.opts.DisableSearch {
		p.errf("search", term, "search is not enabled for this resource")
		return sqlb.Pred{}, false
	}
	// A search term is substituted into one LIKE per searchable column, so its
	// length is multiplied by the width of the fan-out before it reaches the
	// database.
	if !p.withinLength(term, "search", term) {
		return sqlb.Pred{}, false
	}
	var preds []sqlb.Pred
	for _, col := range p.model.Columns {
		if col.Searchable && !col.Hidden && !col.WriteOnly && p.opts.reachable(col) {
			preds = append(preds, sqlb.F(col.Name).Contains(term))
		}
	}
	if len(preds) == 0 {
		p.errf("search", term, "no column of this resource is searchable")
		return sqlb.Pred{}, false
	}
	return sqlb.Or(preds...), true
}

// parseSort resolves ?sort, falling back to the resource's declared ordering.
//
// The fallback goes through the same term parser rather than being applied
// later, so the declared default and a request that spells the same thing
// produce the same ordering — including the declared null placement, which is
// the half a hand-written default in an SDK facade tends to lose.
func (p *parser) parseSort(raw string) []sqlb.Order {
	if raw == "" {
		if len(p.opts.DefaultSort) == 0 {
			return nil
		}
		return p.sortTerms(p.opts.DefaultSort, true)
	}
	terms := strings.Split(raw, ",")
	if len(terms) > p.opts.maxSortTerms() {
		p.errf("sort", raw, "%d sort terms requested, the limit is %d", len(terms), p.opts.maxSortTerms())
		return nil
	}
	return p.sortTerms(terms, false)
}

// sortTerms turns sort terms into ordering, reporting each one it cannot.
//
// declared says the terms came from the resource rather than from the request,
// which changes only what a rejection says: the terms have been checked at mount
// since #165, so reaching a rejection here means a caller assembled
// [Options] by hand and a message blaming the request would send them looking in
// the wrong place.
func (p *parser) sortTerms(terms []string, declared bool) []sqlb.Order {
	var out []sqlb.Order
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		name, desc, err := SortTerm(term)
		if err != nil {
			p.errf("sort", term, "%s%s", blame(declared), err)
			continue
		}
		term = name

		// A request names a column the way the wire spells it; a declaration
		// names it the way the schema does, as Options.Columns and
		// Options.Computed do. The two are the same string unless the schema
		// declared a WireCase, and keeping them apart is what stops a
		// declaration having to be written in the front end's casing.
		col := p.model.ColumnByWire(term)
		if declared {
			col = p.model.Column(term)
		}
		switch {
		case col == nil || col.Hidden || col.WriteOnly || !p.opts.reachable(col):
			p.errAllowed("sort", term, blame(declared)+"unknown column", p.capable(capSort))
			continue
		case !col.Sortable:
			p.errAllowed("sort", term, blame(declared)+"column is not sortable", p.capable(capSort))
			continue
		}

		f := sqlb.F(col.Name)
		o := f.Asc()
		if desc {
			o = f.Desc()
		}
		out = append(out, withDeclaredNulls(o, col))
	}
	return out
}

// SortTerm splits one sort term into the column it names and its direction.
//
// Two spellings, both accepted: `-created_at` and `created_at.desc`. The second
// is there for PostgREST familiarity, and having both here rather than in each
// caller is what stops a declared default and a `?sort` disagreeing about what
// the same text means.
//
// Exported because the term is written in two places — a request, and the
// resource's own DefaultSort — and the second is checked where a resource is
// mounted, which is outside this package.
func SortTerm(term string) (name string, desc bool, err error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return "", false, errors.New("a sort term cannot be empty")
	}
	if name, found := strings.CutPrefix(term, "-"); found {
		return name, true, nil
	}
	if name, dir, found := strings.Cut(term, "."); found {
		switch strings.ToLower(dir) {
		case "asc":
			return name, false, nil
		case "desc":
			return name, true, nil
		default:
			return "", false, fmt.Errorf("unknown sort direction %q, expected asc or desc", dir)
		}
	}
	return term, false, nil
}

// blame prefixes a sort rejection when the term came from the resource's
// declared default rather than from the request, so the reader looks at the
// mount instead of at the query string.
func blame(declared bool) string {
	if declared {
		return "the resource's declared default ordering names a column it cannot sort by: "
	}
	return ""
}

// withDeclaredNulls applies the column's declared null placement to one term.
//
// The grammar has no spelling for it, and that is the design rather than a gap:
// where NULLs belong is a property of what the column means — a NULL
// `published_at` means "not published", which belongs last however the feed is
// sorted — so it is declared once on the column and every request gets it
// right, including the ones a generated client builds (#88).
//
// Left alone, the placement is Postgres's own, which is NULLS LAST ascending
// and NULLS FIRST descending. That is the default the declaration exists to
// escape: it flips underneath a column the moment `?sort=published_at` becomes
// `?sort=-published_at`, so a column that means something by being NULL cannot
// use it.
func withDeclaredNulls(o sqlb.Order, col *sqlb.ColumnInfo) sqlb.Order {
	switch col.SortNulls {
	case sqlb.NullsFirst:
		return o.NullsFirst()
	case sqlb.NullsLast:
		return o.NullsLast()
	default:
		return o
	}
}

func (p *parser) parseSelect(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		col := p.model.ColumnByWire(name)
		if col == nil || col.Hidden || col.WriteOnly || !p.opts.reachable(col) {
			p.errAllowed("select", name, "unknown column", p.selectableNames())
			continue
		}
		out = append(out, col.Name)
	}
	// A projection that dropped the primary key cannot address its own rows,
	// so it is added back rather than surprising the client later — unless the
	// resource narrowed itself out of the key, in which case adding it back
	// would put the one column Options.Columns excluded into every response
	// that named any other (#148).
	if len(out) > 0 && p.model.PK != nil && !contains(out, p.model.PK.Name) &&
		p.opts.reachable(p.model.PK) {
		out = append([]string{p.model.PK.Name}, out...)
	}
	return out
}

func (p *parser) parseExpand(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !contains(p.opts.Expandable, name) {
			p.errAllowed("expand", name, "relation is not expandable", p.opts.Expandable)
			continue
		}
		out = append(out, name)
	}
	return out
}

func (p *parser) parsePagination(values url.Values, q *Query) {
	size := q.PageSize
	if raw := firstValue(values, "per_page"); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			p.errf("per_page", raw, "not a number")
		case n < 1:
			p.errf("per_page", raw, "must be at least 1")
		default:
			size = n
		}
	}
	if raw := firstValue(values, "limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			p.errf("limit", raw, "not a number")
		case n < 1:
			p.errf("limit", raw, "must be at least 1")
		default:
			size = n
		}
	}
	// The cap is enforced rather than reported, so that a client asking for
	// more simply gets the maximum instead of an error.
	if max := p.opts.maxPageSize(); size > max {
		size = max
	}
	q.PageSize = size
	q.Limit = size

	// A cursor and an offset are two answers to "where does this page start",
	// and honouring one silently would make the other's presence a no-op the
	// client could not see. Naming both and saying which to drop is the only
	// answer that lets a caller fix it in one step.
	if raw := firstValue(values, "cursor"); raw != "" {
		q.Cursor = sqlb.Cursor(raw)
		for _, conflict := range []string{"page", "offset"} {
			if firstValue(values, conflict) != "" {
				p.errf("cursor", raw,
					"a cursor and %s both say where the page starts; send one or the other", conflict)
			}
		}
		// Page numbers are meaningless under keyset paging: the client's
		// position is the cursor, and there is no count of pages behind it.
		q.Page = 1
		return
	}

	// The offset budget, applied to both spellings of the same thing. Without
	// it `?page=50000000` is a request for ten billion discarded rows, and
	// `?page=9223372036854775807` overflows (n-1)*size into a negative offset
	// that fails at the database rather than at validation. Both are computed in
	// int64 and compared before anything narrows.
	budget := int64(p.opts.maxOffset())

	if raw := firstValue(values, "page"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			p.errf("page", raw, "not a number")
		case n < 1:
			p.errf("page", raw, "must be at least 1")
		case (n - 1) > budget/int64(size):
			p.errf("page", raw, "starts past the offset budget of %d rows; use ?cursor= to read deeper", budget)
		default:
			q.Page = int(n)
			q.Offset = int(n-1) * size
		}
		return
	}
	if raw := firstValue(values, "offset"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			p.errf("offset", raw, "not a number")
		case n < 0:
			p.errf("offset", raw, "must not be negative")
		case n > budget:
			p.errf("offset", raw, "is past the offset budget of %d rows; use ?cursor= to read deeper", budget)
		default:
			q.Offset = int(n)
		}
	}
	if q.Page == 0 {
		q.Page = q.Offset/size + 1
	}
}

// Coerce converts a URL token into the Go type of its column, so that the
// driver binds an int as an int rather than as text.
//
// It is exported because a path segment needs the same treatment as a query
// parameter: `GET /posts/{id}` has to bind a uuid as a uuid, since Postgres
// will not compare one to text. Parse uses it for every filter value.
func Coerce(s string, t reflect.Type) (any, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	// time.Time is checked before the TextUnmarshaler branch: its own
	// UnmarshalText accepts RFC 3339 only, which would reject the plain dates
	// that a date-range filter is usually written with.
	if t == timeType {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if v, err := time.Parse(layout, s); err == nil {
				return v, nil
			}
		}
		return nil, fmt.Errorf("expected an RFC 3339 timestamp or a date, got %q", s)
	}

	// Types that know how to parse themselves take precedence, which covers
	// uuid.UUID and similar wrappers used by generated models.
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		v := reflect.New(t)
		// Guaranteed by the Implements check above, but asserted with the
		// comma-ok form so a future change to that condition fails loudly.
		u, ok := v.Interface().(encoding.TextUnmarshaler)
		if !ok {
			return nil, fmt.Errorf("filter: %s does not implement encoding.TextUnmarshaler", t)
		}
		if err := u.UnmarshalText([]byte(s)); err != nil {
			return nil, fmt.Errorf("invalid %s value %q: %w", t, s, err)
		}
		return v.Elem().Interface(), nil
	}

	switch t.Kind() {
	case reflect.String:
		return s, nil
	case reflect.Bool:
		v, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("expected a boolean, got %q", s)
		}
		return v, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected an integer, got %q", s)
		}
		return v, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected a non-negative integer, got %q", s)
		}
		return v, nil
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("expected a number, got %q", s)
		}
		return v, nil
	}

	return nil, fmt.Errorf("values of type %s cannot be used in a filter", t)
}

var (
	timeType            = reflect.TypeOf(time.Time{})
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

func isTextColumn(col *sqlb.ColumnInfo) bool {
	t := col.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.String
}

// isTimeColumn reports whether a column holds a date or a timestamp. They are
// one Go type and three things to Postgres, which is exactly why PGType exists
// and why the refusal below reads it rather than this.
func isTimeColumn(col *sqlb.ColumnInfo) bool {
	t := col.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == timeType
}

// bareDate matches a date with no time part, which is the spelling that reads
// as a day and compares as a midnight.
var bareDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func isBareDate(s string) bool { return bareDate.MatchString(strings.TrimSpace(unquote(s))) }

// refuseBareDate turns the silent case into a 400.
//
// `?starts_at=eq.2026-09-01` against a timestamptz column compiles to an
// equality against midnight in the session's time zone, and a stored timestamp
// is almost never exactly that — so the request a caller writes for "what is on
// this day" matches nothing, returns 200, and gives nothing to notice (#241).
// `ne` is the same trap pointed the other way, matching every row; `in` and
// `nin` are the same comparison in a list.
//
// Only where the column's declared type says it is a timestamp. A date column
// compares against a date correctly, and PGType is empty for a hand-written
// model that has not said what its columns are — where the type is unknown this
// says nothing rather than refusing a request that may be exactly right
// (ColumnInfo.PGType: "every reader of it must treat unknown as a real answer").
//
// The ordering operators are deliberately untouched: `gte.2026-09-01` against a
// timestamp means "from midnight onwards", which is what it says and what the
// caller wants.
func (p *parser) refuseBareDate(col *sqlb.ColumnInfo, op string, values []string, param, raw string) bool {
	switch col.PGType {
	case "timestamptz", "timestamp":
	default:
		return true
	}
	switch op {
	case "eq", "ne", "neq", "in", "nin":
	default:
		return true
	}
	for _, v := range values {
		if !isBareDate(v) {
			continue
		}
		day := strings.TrimSpace(unquote(v))
		p.errf(param, raw,
			"%s is a %s, so %q compares against midnight on that date and matches almost nothing; "+
				"ask for the whole day with %s=day.%s, or give a full timestamp such as %s=%s.%sT09:00:00Z",
			col.Wire, col.PGType, day, col.Wire, day, col.Wire, op, day)
		return false
	}
	return true
}

var jsonRawMessageType = reflect.TypeOf(json.RawMessage(nil))

// isJSONColumn reports whether a column holds a jsonb document.
//
// The test is type identity rather than kind, because json.RawMessage and the
// []byte that a bytea column maps to are both slices of bytes and only one of
// them is a document. Getting that backwards would offer containment over a
// blob and refuse it over metadata.
func isJSONColumn(col *sqlb.ColumnInfo) bool {
	t := col.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == jsonRawMessageType
}

// splitTopLevel splits on sep, ignoring separators inside brackets or double
// quotes so that grouped, quoted and JSON values survive.
//
// Braces and square brackets count towards the same depth as parentheses, so
// `or=(metadata.contains.{"a":1,"b":2},status.eq.draft)` splits into two
// conditions rather than three. One counter rather than a stack means `{)`
// balances, which is the existing tolerance for an unmatched `)` rather than a
// new one: this splits, it does not validate, and a malformed value is
// rejected by whatever parses the piece it lands in.
func splitTopLevel(s string, sep byte) []string {
	var (
		out   []string
		depth int
		quote bool
		start int
	)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote:
			if c == '\\' && i+1 < len(s) {
				i++
			} else if c == '"' {
				quote = false
			}
		case c == '"':
			quote = true
		case c == '(', c == '{', c == '[':
			depth++
		case c == ')', c == '}', c == ']':
			if depth > 0 {
				depth--
			}
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	// Drop a single trailing empty field from a trailing separator.
	if n := len(out); n > 1 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

// unquote removes surrounding double quotes, which let a value contain commas.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

func operatorNames() []string {
	out := make([]string, 0, len(operators))
	for name := range operators {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func firstValue(values url.Values, keys ...string) string {
	for _, k := range keys {
		if v := values.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func sortedKeys(values url.Values) []string {
	out := make([]string, 0, len(values))
	for k := range values {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// selectableNames is the projection surface of *this resource*: every non-hidden
// column, minus the computed ones the resource does not select.
//
// Model.Selectable is model-wide and cannot answer this — the same model may be
// mounted twice with different computed sets (#92).
func (p *parser) selectableNames() []string {
	out := make([]string, 0, len(p.model.Columns))
	for _, col := range p.model.Selectable() {
		if !p.opts.reachable(col) {
			continue
		}
		// The wire spelling, because a 400 that lists names the caller cannot
		// type is worse than one that lists nothing.
		out = append(out, col.Wire)
	}
	return out
}
