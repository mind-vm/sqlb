package studio

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// writableColumns is every column a create or update body may carry. Neither
// operation is asked to distinguish further — the manifest doesn't say a
// column is create-only or update-only, so this form doesn't guess one.
func writableColumns(t *schema.TableManifest) []schema.ColumnManifest {
	var out []schema.ColumnManifest
	for _, c := range t.Columns {
		if c.ReadOnly || c.Computed || c.Name == t.PrimaryKey {
			continue
		}
		out = append(out, c)
	}
	return out
}

// createInput is the create body's declared properties that are not columns —
// what a create request carries beyond the row (#309). Empty for almost every
// resource, because a create body is normally its columns and nothing else.
//
// There is no update counterpart, and the nil an edit passes in its place is
// not an omission: the schema declares CreateInput and nothing like it for a
// PATCH, so a body property is a thing a create can carry and an update cannot.
func createInput(t *schema.TableManifest) []schema.BodyProperty {
	if t.REST == nil {
		return nil
	}
	return t.REST.CreateInput
}

// formField is one input on the create/edit form. The widget choice is
// deliberately narrow: a checkbox for bool and a select for a declared enum
// are the only two cases the manifest answers unambiguously (bool has
// exactly two legal values; Enum lists the exact set). Everything else is
// plain text, with the declared type shown as a hint — the same "carries
// nothing an author can decide" line ADR-0053 draws for the rest of this
// tool, applied to a form widget instead of a row label.
type formField struct {
	Name     string
	Kind     string // "checkbox" | "select" | "text"
	Value    string
	Checked  bool
	Options  []string
	Nullable bool
	Hint     string
}

func hintFor(c schema.ColumnManifest) string {
	h := c.Type
	if c.Array {
		h += "[], comma-separated"
	}
	if c.Nullable {
		h += ", optional"
	}
	return h
}

// editValue is dispValue with one exception: an array is joined with commas
// rather than rendered as a JSON array, because that's the format
// encodeFieldValue's submit path expects back — the two have to agree on one
// spelling or a value round-trips through an edit form corrupted.
func editValue(c schema.ColumnManifest, val any) string {
	if val == nil {
		return ""
	}
	if !c.Array {
		return dispValue(val)
	}
	arr, ok := val.([]any)
	if !ok {
		return dispValue(val)
	}
	parts := make([]string, 0, len(arr))
	for _, e := range arr {
		parts = append(parts, dispValue(e))
	}
	return strings.Join(parts, ", ")
}

// buildFormFields renders writableColumns against row's current values, or
// empty values when row is nil (the create form), followed by props — the
// declared properties that are not columns, which the create form has and the
// edit form does not.
//
// They go last rather than interleaved because there is nowhere to interleave
// them to: a body property has no position in the table, and the columns come
// out in the order the schema declares them.
func buildFormFields(t *schema.TableManifest, props []schema.BodyProperty, row map[string]any) []formField {
	var fields []formField
	for _, c := range writableColumns(t) {
		var val any
		if row != nil {
			val = row[wireOf(c)]
		}
		f := formField{Name: c.Name, Nullable: c.Nullable, Hint: hintFor(c)}
		switch {
		case c.Type == "bool" && !c.Array:
			f.Kind = "checkbox"
			if b, ok := val.(bool); ok {
				f.Checked = b
			}
		case len(c.Enum) > 0:
			f.Kind = "select"
			f.Options = c.Enum
			if val != nil {
				f.Value = dispValue(val)
			}
		default:
			f.Kind = "text"
			f.Value = editValue(c, val)
		}
		fields = append(fields, f)
	}
	return append(fields, buildBodyFields(props)...)
}

// formFieldsFromForm rebuilds the field list from a rejected submission, so
// an error redisplay shows what the operator typed rather than reverting to
// the row's last-fetched values.
//
// props is what buildFormFields was given, threaded through the redisplay by
// whichever handler is submitting. A create form that offered a declared
// property has to offer it again: without it the operator's second attempt is
// refused for the same missing property, and the form still has nowhere to
// type it.
func formFieldsFromForm(t *schema.TableManifest, props []schema.BodyProperty, form url.Values) []formField {
	var fields []formField
	for _, c := range writableColumns(t) {
		f := formField{Name: c.Name, Nullable: c.Nullable, Hint: hintFor(c)}
		switch {
		case c.Type == "bool" && !c.Array:
			f.Kind = "checkbox"
			f.Checked = form.Has(c.Name)
		case len(c.Enum) > 0:
			f.Kind = "select"
			f.Options = c.Enum
			f.Value = form.Get(c.Name)
		default:
			f.Kind = "text"
			f.Value = form.Get(c.Name)
		}
		fields = append(fields, f)
	}
	return append(fields, bodyFieldsFromForm(props, form)...)
}

// scalarValue converts one submitted text value into the JSON type the
// column's declared type implies. This is encoding, not a widget guess: the
// manifest already states the type, so "5" becomes the number 5 rather than
// the string "5" mechanically, the same way json.Marshal would for a typed
// field this tool has none of.
func scalarValue(colType, raw string) (any, error) {
	switch colType {
	case "smallint", "int", "bigint", "real", "float", "numeric":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", raw)
		}
		return f, nil
	default:
		return raw, nil
	}
}

func encodeFieldValue(c schema.ColumnManifest, raw string) (any, error) {
	if !c.Array {
		return scalarValue(c.Type, raw)
	}
	parts := strings.Split(raw, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := scalarValue(c.Type, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// bodyHint mirrors hintFor for a declared body property. BodyProperty has no
// Array field (ADR-0043's body vocabulary doesn't carry one), so there is no
// array case to add here.
func bodyHint(p schema.BodyProperty) string {
	h := p.Type
	if p.Nullable {
		h += ", optional"
	}
	return h
}

// buildBodyFields renders declared properties as empty inputs. Both bodies
// that declare them arrive here — an action's, and the non-column half of a
// create's — because a property is the same thing wherever it was declared,
// which is what schema.BodyProperty says by having one name for both (#309).
func buildBodyFields(body []schema.BodyProperty) []formField {
	var fields []formField
	for _, p := range body {
		f := formField{Name: p.Name, Nullable: p.Nullable, Hint: bodyHint(p)}
		switch {
		case p.Type == "bool":
			f.Kind = "checkbox"
		case len(p.Enum) > 0:
			f.Kind = "select"
			f.Options = p.Enum
		default:
			f.Kind = "text"
		}
		fields = append(fields, f)
	}
	return fields
}

func bodyFieldsFromForm(body []schema.BodyProperty, form url.Values) []formField {
	var fields []formField
	for _, p := range body {
		f := formField{Name: p.Name, Nullable: p.Nullable, Hint: bodyHint(p)}
		switch {
		case p.Type == "bool":
			f.Kind = "checkbox"
			f.Checked = form.Has(p.Name)
		case len(p.Enum) > 0:
			f.Kind = "select"
			f.Options = p.Enum
			f.Value = form.Get(p.Name)
		default:
			f.Kind = "text"
			f.Value = form.Get(p.Name)
		}
		fields = append(fields, f)
	}
	return fields
}

// bodyPropertyValue encodes one submitted property, and reports whether the
// body should carry it at all: a blank text or select is left out, so that the
// property's own default applies rather than an empty string overriding it.
//
// The key it is stored under is the property's name exactly as declared, not
// its WireCase spelling. WireCase is a function of a *column* name and a
// property is not a column. Every emitter writes the declared name verbatim
// (codegen's renderActionInput and renderBodyProps both tag with d.Name), so a
// camelCase schema would otherwise have studio sending completedAt to a
// handler that only knows completed_at.
func bodyPropertyValue(p schema.BodyProperty, form url.Values) (any, bool, error) {
	if p.Type == "bool" {
		return form.Has(p.Name), true, nil
	}
	if p.Nullable && form.Has(p.Name+"__clear") {
		return nil, true, nil
	}
	raw := form.Get(p.Name)
	if raw == "" {
		return nil, false, nil
	}
	val, err := scalarValue(p.Type, raw)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", p.Name, err)
	}
	return val, true, nil
}

// parseActionBody encodes a submitted form into the JSON body a declared
// action expects.
func parseActionBody(body []schema.BodyProperty, form url.Values) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range body {
		val, send, err := bodyPropertyValue(p, form)
		if err != nil {
			return nil, err
		}
		if send {
			out[p.Name] = val
		}
	}
	return out, nil
}

// parseFormBody turns a submitted form into a JSON body keyed by wire name.
// A bool is always present (a checkbox has no "unchanged" state). A blank
// text/select value is omitted — "no change" on edit, "use the column's own
// default" on create — unless its "<name>__clear" companion is present, which
// forces an explicit null so a nullable field can be cleared rather than only
// ever grown.
//
// props are the declared properties that are not columns, empty on the edit
// path. They cannot collide with a column's key: the schema refuses a property
// named for a column in either spelling, precisely because one JSON object
// cannot carry both under one name (schema's validateCreateInput).
func parseFormBody(t *schema.TableManifest, props []schema.BodyProperty, form url.Values) (map[string]any, error) {
	body := map[string]any{}
	for _, c := range writableColumns(t) {
		wire := wireOf(c)
		if c.Type == "bool" && !c.Array {
			body[wire] = form.Has(c.Name)
			continue
		}
		if c.Nullable && form.Has(c.Name+"__clear") {
			body[wire] = nil
			continue
		}
		raw := form.Get(c.Name)
		if raw == "" {
			continue
		}
		val, err := encodeFieldValue(c, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		body[wire] = val
	}
	for _, p := range props {
		val, send, err := bodyPropertyValue(p, form)
		if err != nil {
			return nil, err
		}
		if send {
			body[p.Name] = val
		}
	}
	return body, nil
}
