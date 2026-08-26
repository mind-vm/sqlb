package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// binding is everything about one resource that can be computed once, at
// registration, rather than per request: the model, the JSON name of each
// column, and the columns a request body may write.
type binding[T any] struct {
	opts  Options
	model *sqlb.Model

	// jsonKey maps a column name onto the bytes that open its member in the
	// response — `"org_id":`, quoted and with the colon.
	//
	// The property is usually the column name, because codegen writes
	// `json:"org_id"` beside `db:"org_id"`, but a hand-written model may
	// disagree and the response must follow the struct, not the column. It is
	// held rendered rather than as the name because that is what serialising a
	// row needs and it cannot change between requests; see jsonKeyOf. A column
	// with no JSON name is absent rather than present holding nil, so a lookup
	// miss is the skip condition.
	jsonKey map[string][]byte

	// relKey is the same rendering for the relations this resource declares
	// expandable, keyed by the name `?expand` uses. Only declared relations can
	// be reached, so this covers every key a response can carry.
	relKey map[string][]byte

	// selectable is the default projection: every non-hidden column, minus the
	// computed ones this resource did not opt into. It drives the response
	// body's keys as well as the SELECT list, so a computed column the mount
	// does not select is absent from the JSON rather than present holding its
	// zero value — which for a bool would have been indistinguishable from a
	// real false (#92).
	selectable []*sqlb.ColumnInfo

	// writeSelectable is selectable as a create or update response can answer it:
	// the same projection, minus the computed columns whose expression takes a
	// bind.
	//
	// A write has nowhere to take a bind from, so ADR-0041 leaves such a column
	// out of RETURNING — and the response used to serialise the scanned struct's
	// zero value anyway, which for a bool is indistinguishable from a real false.
	// That is the failure the projection machinery was built for on the read side
	// (#92) arriving on the write side: `PATCH /posts/{id}` answered
	// `"my_acknowledged": false` to the viewer who had just acknowledged it, and
	// the consumer's tests passed because they asserted on the GET (#163). The key
	// is absent instead, which is what the ADR promised and what a client reads as
	// "not computed here".
	writeSelectable []*sqlb.ColumnInfo

	// writeComputed is the computed columns of writeSelectable, by name, for the
	// statement's own opt-in. Writes take computed columns the way reads do —
	// only the ones the mount asked for — so a resource with no Computed sends an
	// INSERT whose RETURNING is stored columns and nothing else (#164).
	writeComputed []string

	// served is selectable as a set, for the places that ask about one column
	// rather than walking the projection — the OpenAPI parameter list, mostly.
	//
	// Derived from selectable rather than recomputed from Options, because the
	// document and the parser disagreeing about what a request may name is the
	// failure both #92 and #148 are shaped like: a filter parameter published
	// for a column every request naming it is about to be refused for.
	served map[string]bool

	// writable is what a request body may set. Read-only columns are excluded
	// because the database or a hook owns them, and hidden ones because a
	// column that never leaves the process should not be settable from
	// outside it either.
	writable []*sqlb.ColumnInfo

	// readOnly is the complement of writable, as reflect field paths.
	//
	// Paths rather than column names because of how the guarantee is enforced:
	// the fields are cleared on the row a request produced, rather than the
	// columns being omitted from the INSERT. See clearReadOnly.
	readOnly [][]int
}

// clearReadOnly resets every read-only field of value to its zero value.
//
// This is what stops a request writing a column the schema says it may not.
// The generated create body has no field for one, so ordinarily there is
// nothing to clear — but a hand-written CreateBody's Row() can set anything on
// the struct it builds, and the capability would then be advisory.
//
// It runs before the insert and therefore before BeforeCreate, which is the
// whole point: a hook may still supply the value. That ordering is what makes
// `ReadOnly` mean "the database or a hook owns this" rather than "nothing can
// ever put a value here".
func (b *binding[T]) clearReadOnly(value *T) {
	if len(b.readOnly) == 0 {
		return
	}
	rv := reflect.ValueOf(value).Elem()
	for _, index := range b.readOnly {
		field, ok := fieldAt(rv, index)
		if !ok {
			continue
		}
		field.SetZero()
	}
}

// writableOnly keeps the names of columns this resource lets a request write.
//
// [CreateExplicit] is implemented by the body, and a hand-written one can name
// anything — including a read-only column, which clearReadOnly has just zeroed.
// Passing that name to Insert.Explicit would write the zero it just cleared,
// turning the read-only guarantee inside out. So the mount's own writable set
// decides, the same way it decides what may be written at all.
func (b *binding[T]) writableOnly(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	writable := make(map[string]bool, len(b.writable))
	for _, col := range b.writable {
		writable[col.Name] = true
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if writable[name] {
			out = append(out, name)
		}
	}
	return out
}

// fieldAt walks a reflect index path, which may traverse embedded structs.
//
// It stops at a nil embedded pointer rather than allocating one: there is no
// value behind it to clear, and allocating would add a struct the caller never
// asked for.
func fieldAt(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v, true
}

func bind[T any](opts Options) (*binding[T], error) {
	m := sqlb.ModelOf[T]()
	b := &binding[T]{
		opts:       opts,
		model:      m,
		jsonKey:    make(map[string][]byte, len(m.Columns)),
		selectable: selectableFor(m, opts.Computed, opts.Columns),
	}
	b.writeSelectable, b.writeComputed = writeProjection(b.selectable)

	// Checked before the loop below, because that loop decides what a request
	// body may write and an unchecked name there is a resource quietly serving
	// the wrong surface.
	if err := checkColumns(m, opts); err != nil {
		return nil, err
	}
	reachable := reachableSet(m, opts.Columns)
	b.served = make(map[string]bool, len(b.selectable))
	for _, col := range b.selectable {
		b.served[col.Name] = true
	}

	// The unrendered names, for the diagnostic below. Serialising wants the
	// keys and nothing else, so this stays local.
	jsonName := make(map[string]string, len(m.Columns))

	rt := reflect.TypeFor[T]()
	for _, col := range m.Columns {
		name, err := jsonNameOf(rt, col)
		if err != nil {
			return nil, err
		}
		jsonName[col.Name] = name
		if name != "" {
			key, err := jsonKeyOf(name)
			if err != nil {
				return nil, fmt.Errorf("rest: %s.%s has an unencodable json name %q: %w", m.Type, col.Field, name, err)
			}
			b.jsonKey[col.Name] = key
		}
		// A column outside Options.Columns is treated exactly as a read-only one
		// on the write path: cleared off the row a request produced, rather than
		// merely absent from the generated body. The generated body is shared
		// with the wide resource, so "absent" is not something the narrowed
		// mount can arrange — and a CreateBody.Row that sets the column would
		// otherwise write it through a resource that cannot even read it (#148).
		if col.ReadOnly || !reachable(col) {
			b.readOnly = append(b.readOnly, col.Index)
			continue
		}
		if !col.Hidden {
			b.writable = append(b.writable, col)
		}
	}

	// The body's spelling and the request's spelling must be the same string.
	//
	// The response body is json.Marshal over the struct, so its keys come from
	// the `json` tag; a filter parameter is resolved through ColumnInfo.Wire,
	// which comes from the `sqlb` tag. Codegen writes both from one WireCase
	// and they cannot disagree — but a hand-written model, or a generated one
	// edited by hand, can produce a resource whose response says `createdAt`
	// and whose filter only answers to `created_at`. That is precisely the two
	// spellings ADR-0036 exists to prevent, so it refuses to mount rather than
	// serving an API whose own document is wrong about it.
	for _, col := range m.Columns {
		if col.Hidden || col.WriteOnly {
			continue
		}
		if name := jsonName[col.Name]; name != "" && name != col.Wire {
			return nil, fmt.Errorf(
				"rest: %s.%s is spelled %q in the body and %q on the wire; one column has one "+
					"spelling, so a filter or a sort naming it would not match its own response "+
					"(ADR-0036) — regenerate the model, or make the json and sqlb tags agree",
				m.Type, col.Field, name, col.Wire)
		}
	}

	// A hidden or write-only column carrying a JSON name on the row struct
	// would be serialised by any code that marshalled the struct directly —
	// a debug log, a hand-written handler — so the mismatch is worth
	// reporting where it is introduced. This is about the row struct only:
	// a write-only column's separate create/update body struct carries a
	// real json tag, since that is how the value gets in.
	for _, col := range m.Columns {
		if (col.Hidden || col.WriteOnly) && jsonName[col.Name] != "" {
			return nil, fmt.Errorf(
				"rest: %s.%s is hidden or write-only but has json tag %q on the row struct; "+
					"both need `json:\"-\"` there so they cannot be marshalled by accident",
				m.Type, col.Field, jsonName[col.Name])
		}
	}
	// Computed names a column the model computes. An unknown one, or a stored
	// one, is a mounting error for the reason Expandable's is: at request time
	// it would parse cleanly and quietly serve a resource missing the value
	// somebody declared it should carry.
	for _, name := range opts.Computed {
		col := m.Column(name)
		switch {
		case col == nil:
			return nil, fmt.Errorf(
				"rest: %s declares Computed %q, but %s has no such column (have: %s)",
				opts.name(), name, m.Type, strings.Join(m.ColumnNames(), ", "))
		case !col.Computed():
			return nil, fmt.Errorf(
				"rest: %s declares Computed %q, but %s stores that column rather than computing it; "+
					"a stored column is already in the response and does not need declaring",
				opts.name(), name, m.Type)
		}
	}

	// DefaultSort decides what a request that names no ordering gets, so a term
	// it cannot serve has to fail here: at request time the client sent nothing
	// wrong and would be answered 400 for it.
	if err := checkDefaultSort(b, opts); err != nil {
		return nil, err
	}

	// Expandable names a relation, not a column, and an unknown one has to fail
	// here: at request time the parameter parses cleanly against Options and the
	// response would come back 200 with the relation missing.
	for _, name := range opts.Expandable {
		rel := m.Relation(name)
		if rel == nil {
			return nil, fmt.Errorf(
				"rest: %s declares Expandable %q, but %s has no such relation (declared: %s); "+
					"a relation is a field tagged `sqlb:\"expands=<column>\"` beside a column tagged `expand`",
				opts.Path, name, m.Type.Name(), strings.Join(m.RelationNames(), ", "))
		}
		key, err := jsonKeyOf(rel.Name)
		if err != nil {
			return nil, fmt.Errorf("rest: relation %s of %s has an unencodable name: %w", rel.Name, m.Type, err)
		}
		if b.relKey == nil {
			b.relKey = make(map[string][]byte, len(opts.Expandable))
		}
		b.relKey[name] = key
	}

	return b, nil
}

// jsonNameOf resolves the JSON property a column serialises as, following the
// same rules encoding/json does.
func jsonNameOf(rt reflect.Type, col *sqlb.ColumnInfo) (string, error) {
	sf, err := fieldByIndex(rt, col.Index)
	if err != nil {
		return "", err
	}
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name, nil
	}
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "-":
		return "", nil
	case "":
		return sf.Name, nil
	}
	return name, nil
}

// jsonKeyOf renders a property name as the bytes that open its member: the
// quoted name, then the colon.
//
// It is quoted through encoding/json rather than by wrapping the name in quote
// characters, because a JSON name comes from a struct tag and may contain
// anything — the escaping has to be the same escaping the rest of the document
// gets, and only the encoder knows what that is.
//
// Precomputing it is worth a function because of where the alternative ran:
// MarshalJSON serialises one column of one row, so quoting the *same* constant
// name happened once per column per row — 400 times for a 50-row page of eight
// columns, each an allocation. It was the largest single source of garbage on
// the response path.
func jsonKeyOf(name string) ([]byte, error) {
	quoted, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	return append(quoted, ':'), nil
}

// fieldByIndex walks an index path over a type, tolerating embedded pointers.
// reflect.Type.FieldByIndex panics on a path it cannot walk; this reports.
func fieldByIndex(t reflect.Type, index []int) (reflect.StructField, error) {
	var sf reflect.StructField
	for i, x := range index {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || x >= t.NumField() {
			return sf, fmt.Errorf("rest: cannot resolve field at index %v of %s", index[:i+1], t)
		}
		sf = t.Field(x)
		t = sf.Type
	}
	return sf, nil
}

// columnsFor resolves a projection — the ?select list, or every selectable
// column when the request named none — to the columns to serialise.
func (b *binding[T]) columnsFor(selected []string) []*sqlb.ColumnInfo {
	if len(selected) == 0 {
		return b.selectable
	}
	out := make([]*sqlb.ColumnInfo, 0, len(selected))
	for _, name := range selected {
		if col := b.model.Column(name); col != nil && !col.Hidden && !col.WriteOnly {
			out = append(out, col)
		}
	}
	return out
}

// row is one serialised model row, restricted to a projection.
//
// It exists because a projected scan leaves the unselected fields at their zero
// value, and marshalling the struct would report `"title": ""` for a column the
// query never read — indistinguishable from a genuinely empty title. Adding
// omitempty everywhere would hide real empty values instead, which is the same
// lie in the other direction. So the projection decides which keys exist.
type row[T any] struct {
	value T
	cols  []*sqlb.ColumnInfo
	// keys is the binding's precomputed `"name":` per column. A column absent
	// from it has no JSON name and is not serialised.
	keys map[string][]byte
	// expand are the relations this request asked to resolve. They are not
	// columns, so they are serialised after them, under the relation's own
	// name — which is the JSON name of the field the expansion landed in. Each
	// carries its key for the same reason a column's is precomputed.
	expand []expansion
}

// expansion is one relation to serialise, with its member key already rendered.
type expansion struct {
	rel *sqlb.RelationInfo
	key []byte
}

// memberBytes is a rough guess at what one serialised column costs, used to
// size a buffer in one allocation rather than letting it double its way there.
// Guessing low only costs the growth it was avoiding, so it is set near the
// small end: an id, a status, a timestamp.
const memberBytes = 48

// rowWriter is a buffer and the encoder bound to it.
//
// It is a type rather than two locals because a page shares one: encoding/json
// asks each value for its own []byte, so a row that answered on its own would
// allocate a buffer, fill it, and have the page copy it out and drop it — once
// per row. rows.MarshalJSON hands every row the same writer instead, which is
// what makes a page of fifty cost one buffer rather than fifty.
//
// The encoder points at the buffer, so a rowWriter must not be copied once
// built. newRowWriter is the only constructor for that reason.
type rowWriter struct {
	buf bytes.Buffer
	enc *json.Encoder
	// written counts members of the object currently being built, so that the
	// separator goes before every member but the first. A counter rather than
	// the loop index because a column can be skipped, and keying the comma off
	// the index would emit a leading one the moment the first column is the
	// skipped one.
	written int
}

func newRowWriter(hint int) *rowWriter {
	w := &rowWriter{}
	w.buf.Grow(hint)
	w.enc = json.NewEncoder(&w.buf)
	return w
}

// openMember writes the separator, if one is due, and the member's key.
func (w *rowWriter) openMember(key []byte) {
	if w.written > 0 {
		w.buf.WriteByte(',')
	}
	w.written++
	w.buf.Write(key)
}

// timestamp writes t as encoding/json would, and reports whether it did.
//
// It is worth a special case because time.Time is the one type a generated
// resource carries that allocates on every value: MarshalJSON opens with a
// make of its own, which the encoder then copies out and drops. AppendFormat
// writes the same bytes into the buffer the page is already filling. On the
// benchmark page — fifty rows, one timestamp each — that was a fifth of the
// allocations left on the response path.
//
// The two formats agree wherever MarshalJSON succeeds; where it fails it is
// the error that matters, so the cases it rejects return false and go back
// through the encoder to raise it. Those are a year outside [0,9999] and a
// zone a whole day or more from UTC, neither of which survives a round trip
// through timestamptz — the guard is here because AppendFormat would render
// them as malformed JSON rather than refuse.
func (w *rowWriter) timestamp(t time.Time) bool {
	if y := t.Year(); y < 0 || y > 9999 {
		return false
	}
	if _, off := t.Zone(); off <= -24*3600 || off >= 24*3600 {
		return false
	}
	b := w.buf.AvailableBuffer()
	b = append(b, '"')
	b = t.AppendFormat(b, time.RFC3339Nano)
	w.buf.Write(append(b, '"'))
	return true
}

// member writes one key and its value.
//
// The value goes through the bound encoder rather than json.Marshal, which
// would allocate a copy of every value for us to append and then discard — one
// allocation per column per row.
func (w *rowWriter) member(key []byte, v any) error {
	w.openMember(key)
	switch t := v.(type) {
	case time.Time:
		if w.timestamp(t) {
			return nil
		}
	case *time.Time:
		// A nil one is null, which the encoder writes for free; only a value
		// worth formatting takes the fast path.
		if t != nil && w.timestamp(*t) {
			return nil
		}
	}
	if err := w.enc.Encode(v); err != nil {
		return err
	}
	// Encode terminates a value with a newline, which is legal between tokens
	// but is not what a response should carry. It is always exactly one byte,
	// and Encode writes nothing at all when it fails, so this only ever removes
	// the terminator it just wrote.
	w.buf.Truncate(w.buf.Len() - 1)
	return nil
}

// sizeHint is roughly what this row will occupy, for sizing a buffer.
func (r row[T]) sizeHint() int {
	return (len(r.cols)+len(r.expand))*memberBytes + 2
}

// MarshalJSON writes only the projected columns, in projection order, followed
// by whatever relations the request expanded.
//
// A page does not reach this method — rows.MarshalJSON calls writeTo directly,
// with a buffer the whole page shares. This is the single-row path: the item,
// create and update responses.
func (r row[T]) MarshalJSON() ([]byte, error) {
	w := newRowWriter(r.sizeHint())
	if err := r.writeTo(w); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// writeTo appends the row to w as a JSON object.
func (r row[T]) writeTo(w *rowWriter) error {
	w.written = 0
	w.buf.WriteByte('{')
	rv := reflect.ValueOf(r.value)

	for _, col := range r.cols {
		key := r.keys[col.Name]
		if key == nil {
			continue
		}
		fv, ok := valueByIndex(rv, col.Index)
		if !ok {
			w.openMember(key)
			w.buf.WriteString("null")
			continue
		}
		if err := w.member(key, fv.Interface()); err != nil {
			return fmt.Errorf("rest: encoding %s: %w", col.Name, err)
		}
	}

	for _, exp := range r.expand {
		fv, ok := valueByIndex(rv, exp.rel.Index)
		if !ok {
			continue
		}
		if err := w.member(exp.key, fv.Interface()); err != nil {
			return fmt.Errorf("rest: encoding expanded %s: %w", exp.rel.Name, err)
		}
	}

	w.buf.WriteByte('}')
	return nil
}

// rows is a page of them, and the reason it is a named type rather than a
// slice: encoding/json asks a value for its bytes, so the array is the smallest
// unit that can be built in one buffer.
type rows[T any] []row[T]

// MarshalJSON writes the whole page into one buffer.
//
// An empty page marshals as `[]` rather than `null`, so a client iterating the
// result does not have to test for it — which is why no handler has to replace
// a nil slice before returning one.
func (rs rows[T]) MarshalJSON() ([]byte, error) {
	hint := 2
	if len(rs) > 0 {
		hint += len(rs) * (rs[0].sizeHint() + 1)
	}
	w := newRowWriter(hint)

	w.buf.WriteByte('[')
	for i := range rs {
		if i > 0 {
			w.buf.WriteByte(',')
		}
		if err := rs[i].writeTo(w); err != nil {
			return nil, err
		}
	}
	w.buf.WriteByte(']')
	return w.buf.Bytes(), nil
}

// Schema reports the row's OpenAPI schema as T's, so the document describes the
// model rather than this wrapper. Every property is optional, because ?select
// may leave any of them out.
func (r row[T]) Schema(reg huma.Registry) *huma.Schema {
	s := reg.Schema(reflect.TypeFor[T](), true, "")
	if resolved := deref(reg, s); resolved != nil {
		resolved.Required = nil
		nullifyOneToOne(resolved, reflect.TypeFor[T]())
	}
	return s
}

// nullifyOneToOne widens a one-to-one reverse relation's property to admit
// null, matching what the server actually sends when the relation is absent
// (compileExpansions' LEFT JOIN, not the capped-collection envelope).
//
// It cannot use the `nullable` struct tag every other nullable field in this
// codebase uses: huma refuses that combination outright for a $ref field
// (schema.go panics with "nullable is not supported for field ... which is
// type '$ref'", because OpenAPI has no clean way to say "nullable" and "ref"
// in the same breath — setting Schema.Nullable on a $ref schema serialises as
// the nonsensical `"type": ["", "null"]`, since a $ref schema carries no Type
// of its own to pair "null" with). The 3.1-native way to say "this ref, or
// null" is the anyOf form used below, the same shape this document's own
// components/schemas would use for a nullable $ref if huma emitted one at
// all.
//
// A field qualifies by the same marker relation.go's tag parser recognises: a
// bare `reverse` token in its `sqlb` tag. The forward and capped-collection
// cases are untouched — a capped collection already reports absence as an
// empty items list, never null, so it has nothing to widen.
func nullifyOneToOne(s *huma.Schema, t reflect.Type) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !isOneToOneField(f) {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		orig, ok := s.Properties[name]
		if !ok || orig == nil || orig.Ref == "" {
			continue
		}
		s.Properties[name] = &huma.Schema{AnyOf: []*huma.Schema{orig, {Type: "null"}}}
	}
}

// isOneToOneField reports whether f is the bare-pointer field the Go codegen
// emits for a one-to-one reverse relation: the `reverse` token, split on
// commas the way relation.go's own relationTag parser splits it, rather than
// a substring match that could mismatch a future tag value containing
// "reverse" as part of a longer word.
func isOneToOneField(f reflect.StructField) bool {
	tag, ok := f.Tag.Lookup("sqlb")
	if !ok {
		return false
	}
	for part := range strings.SplitSeq(tag, ",") {
		if part == "reverse" {
			return true
		}
	}
	return false
}

// deref follows a $ref back to the schema it names, so that a registered
// component can be amended.
func deref(reg huma.Registry, s *huma.Schema) *huma.Schema {
	if s == nil {
		return nil
	}
	if s.Ref == "" {
		return s
	}
	return reg.SchemaFromRef(s.Ref)
}

// valueByIndex walks an index path over a value, reporting rather than
// panicking when it meets a nil embedded pointer.
func valueByIndex(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, x := range index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return reflect.Value{}, false
				}
				v = v.Elem()
			}
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		v = v.Field(x)
	}
	return v, true
}

// rowsOf wraps scanned rows for serialisation under a projection.
func (b *binding[T]) rowsOf(values []T, cols []*sqlb.ColumnInfo, expand []string) rows[T] {
	// Resolved once for the page rather than per row: the relations are the
	// same for every row in it, and the lookup is a scan of a short slice.
	rels := b.relationsFor(expand)
	out := make(rows[T], len(values))
	for i, v := range values {
		out[i] = row[T]{value: v, cols: cols, keys: b.jsonKey, expand: rels}
	}
	return out
}

// relationsFor resolves the request's expand names to the relations to
// serialise. Names are already validated — by the parser against
// Options.Expandable, and by bind against the model — so an unknown one here
// would be a bug rather than bad input, and is skipped rather than guessed at.
// Each carries the member key bind rendered for it, so serialising a page of
// rows re-renders nothing.
func (b *binding[T]) relationsFor(names []string) []expansion {
	if len(names) == 0 {
		return nil
	}
	out := make([]expansion, 0, len(names))
	for _, name := range names {
		rel := b.model.Relation(name)
		key := b.relKey[name]
		if rel == nil || key == nil {
			continue
		}
		out = append(out, expansion{rel: rel, key: key})
	}
	return out
}

// checkColumns validates Options.Columns against the model.
//
// Both refusals are startup-only on purpose. An unknown name would leave the
// resource serving one column fewer than somebody meant, with no request able
// to report it; a missing primary key would leave a resource that cannot
// address, order or page its own rows, and the symptoms of that arrive one at
// a time and in the wrong place.
func checkColumns(m *sqlb.Model, opts Options) error {
	if len(opts.Columns) == 0 {
		return nil
	}
	for _, name := range opts.Columns {
		if m.Column(name) == nil {
			return fmt.Errorf(
				"rest: %s declares Columns %q, but %s has no such column (have: %s)",
				opts.name(), name, m.Type, strings.Join(m.ColumnNames(), ", "))
		}
	}
	if m.PK != nil && !containsName(opts.Columns, m.PK.Name) {
		return fmt.Errorf(
			"rest: %s narrows to Columns that leave out the primary key %q; "+
				"the key addresses a row, settles the ordering and is what a cursor is built from, "+
				"so a resource without it cannot page — add %q, or drop Columns and use Hidden if the key must never be served",
			opts.name(), m.PK.Name, m.PK.Name)
	}
	return nil
}

// checkDefaultSort validates Options.DefaultSort against what this resource can
// actually sort by.
//
// Startup-only, and for the reason Expandable's check is: the terms are the
// resource's, not the request's, so a bad one at request time is a 400 answering
// a client that sent nothing at all. The check is against the resource's surface
// rather than the model's, because Columns and Computed both narrow what is
// sortable and a default naming a column this mount does not serve is the same
// mistake as a default naming one that does not exist (#165).
func checkDefaultSort[T any](b *binding[T], opts Options) error {
	for _, term := range opts.DefaultSort {
		name, _, err := filter.SortTerm(term)
		if err != nil {
			return fmt.Errorf("rest: %s declares DefaultSort %q, but %w", opts.name(), term, err)
		}
		col := b.model.Column(name)
		switch {
		case col == nil || col.Hidden || !b.served[col.Name]:
			return fmt.Errorf(
				"rest: %s declares DefaultSort %q, but this resource has no such column (sortable: %s)",
				opts.name(), term, strings.Join(b.sortableNames(), ", "))
		case !col.Sortable:
			return fmt.Errorf(
				"rest: %s declares DefaultSort %q, but %s does not declare that column Sortable; "+
					"a capability is opt-in, and an ordering nothing declared is one no ?sort could ask for either "+
					"(sortable: %s)",
				opts.name(), term, b.model.Type, strings.Join(b.sortableNames(), ", "))
		}
	}
	return nil
}

// sortableNames lists the columns this resource will sort by, for a rejection.
func (b *binding[T]) sortableNames() []string {
	var out []string
	for _, col := range b.selectable {
		if col.Sortable && !col.Hidden {
			out = append(out, col.Name)
		}
	}
	return out
}

// reachableSet answers "is this column part of this resource" once, so the
// binding loop does not walk the allowlist per column.
func reachableSet(m *sqlb.Model, columns []string) func(*sqlb.ColumnInfo) bool {
	if len(columns) == 0 {
		return func(*sqlb.ColumnInfo) bool { return true }
	}
	in := make(map[string]bool, len(columns))
	for _, name := range columns {
		in[name] = true
	}
	return func(col *sqlb.ColumnInfo) bool { return col != nil && in[col.Name] }
}

func containsName(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// selectableFor is the resource's projection: every non-hidden column, minus
// the computed ones it did not ask for and the ones outside its surface.
//
// Model.Selectable cannot answer this on its own — it is model-wide, and the
// same model may be mounted twice with different computed sets, which is the
// case that made a shared model expensive to read (#92), and twice with
// different column sets, which is the public-and-privileged pair (#148).
// writeProjection splits a read projection into what a write can answer: the
// columns, minus the ones a mutation cannot evaluate, and the computed names
// among them.
//
// The one thing a write cannot evaluate is a column carrying Needs. Its
// expression takes a bind, the bind is a property of who is asking, and the
// hooks a write runs receive the row rather than the statement — so ADR-0041
// leaves it out of RETURNING and it is read back by the next query. Dropping it
// from the response too is what makes the two agree (#163).
func writeProjection(selectable []*sqlb.ColumnInfo) ([]*sqlb.ColumnInfo, []string) {
	cols := make([]*sqlb.ColumnInfo, 0, len(selectable))
	var computed []string
	for _, col := range selectable {
		if col.Computed() {
			if len(col.Needs) > 0 {
				continue
			}
			computed = append(computed, col.Name)
		}
		cols = append(cols, col)
	}
	return cols, computed
}

func selectableFor(m *sqlb.Model, computed, columns []string) []*sqlb.ColumnInfo {
	wanted := make(map[string]bool, len(computed))
	for _, name := range computed {
		wanted[name] = true
	}
	reachable := reachableSet(m, columns)
	out := make([]*sqlb.ColumnInfo, 0, len(m.Columns))
	for _, col := range m.Selectable() {
		if !reachable(col) {
			continue
		}
		if col.Computed() && !wanted[col.Name] {
			continue
		}
		out = append(out, col)
	}
	return out
}
