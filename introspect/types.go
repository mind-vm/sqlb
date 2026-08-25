package introspect

import (
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// columnType maps what format_type reports onto a logical type.
//
// The spellings are Postgres's canonical ones rather than the ones the DDL
// layer emits: a column declared varchar(200) reports as "character
// varying(200)", timestamptz as "timestamp with time zone", and time as "time
// without time zone". Writing this against the emitted spellings would have
// matched almost nothing.
//
// ok is false for a type the DSL has no equivalent for, which is reported
// rather than guessed at — a column silently imported as text would produce a
// migration proposing to change the real column's type to text.
// splitArrayType strips the trailing [] format_type puts on an array column,
// returning the element spelling and whether there was one.
//
// Only one dimension is accepted. `text[][]` is a two-dimensional column, which
// the DSL cannot declare — and dropping a dimension silently would produce a
// registry whose next Diff proposes rewriting the real column.
func splitArrayType(formatted string) (elem string, array bool) {
	rest, found := strings.CutSuffix(strings.TrimSpace(formatted), "[]")
	if !found {
		return formatted, false
	}
	return rest, true
}

// columnType maps a formatted type onto a logical one, and returns the type's
// parenthesised arguments where it has them — a varchar's length, a vector's
// dimension, or a numeric's precision and scale. They are the same thing to
// Postgres and to this function; what each means is decided by the constructor
// that receives it.
func columnType(formatted string) (t schema.Type, typeArg, scale int, ok bool) {
	base, arg := splitTypeArg(formatted)
	switch base {
	case "text":
		return schema.TypeText, 0, 0, true
	case "character varying":
		// A varchar with no length is indistinguishable from text in
		// Postgres, and the DDL layer renders both as text, so importing it
		// as text keeps the round trip closed.
		if arg == "" {
			return schema.TypeText, 0, 0, true
		}
		n, err := strconv.Atoi(arg)
		if err != nil {
			return "", 0, 0, false
		}
		return schema.TypeVarchar, n, 0, true
	case "smallint":
		return schema.TypeSmallInt, 0, 0, true
	case "integer":
		return schema.TypeInt, 0, 0, true
	case "bigint":
		return schema.TypeBigInt, 0, 0, true
	case "real":
		return schema.TypeReal, 0, 0, true
	case "double precision":
		return schema.TypeFloat, 0, 0, true
	case "numeric":
		// A numeric with a precision is a different type from an unbounded
		// one, and since #81 the DSL can say so: Numeric(name, p, s). Postgres
		// formats it as "numeric(5,2)", and as "numeric(5,0)" for a precision
		// declared alone.
		if arg == "" {
			return schema.TypeNumeric, 0, 0, true
		}
		precText, scaleText, hasScale := strings.Cut(arg, ",")
		prec, err := strconv.Atoi(strings.TrimSpace(precText))
		if err != nil || prec <= 0 {
			return "", 0, 0, false
		}
		if !hasScale {
			return schema.TypeNumeric, prec, 0, true
		}
		sc, err := strconv.Atoi(strings.TrimSpace(scaleText))
		if err != nil {
			return "", 0, 0, false
		}
		return schema.TypeNumeric, prec, sc, true
	case "boolean":
		return schema.TypeBool, 0, 0, true
	case "uuid":
		return schema.TypeUUID, 0, 0, true
	case "timestamp with time zone":
		return schema.TypeTimestamp, 0, 0, true
	case "date":
		return schema.TypeDate, 0, 0, true
	case "time without time zone":
		return schema.TypeTime, 0, 0, true
	case "jsonb":
		return schema.TypeJSON, 0, 0, true
	case "bytea":
		return schema.TypeBytes, 0, 0, true
	case "vector":
		// A vector with no dimension is a legal Postgres column and not one the
		// DSL can declare: the dimension is part of the type there, so
		// importing it as any particular width would propose a migration that
		// narrows the real column to a size nobody chose.
		if arg == "" {
			return "", 0, 0, false
		}
		n, err := strconv.Atoi(arg)
		if err != nil || n <= 0 {
			return "", 0, 0, false
		}
		return schema.TypeVector, n, 0, true
	}
	return "", 0, 0, false
}

// splitTypeArg splits "character varying(200)" into its name and its argument.
func splitTypeArg(s string) (base, arg string) {
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return s, ""
	}
	return s[:open], s[open+1 : len(s)-1]
}

// The generators the schema package ships, recognised by the exact text they
// produce. Anything else becomes a raw expression, which is faithful whether or
// not this package understands it.
var knownDefaults = map[string]func() *schema.Default{
	"now()":              schema.Now,
	"uuid_generate_v7()": schema.GenUUIDv7,
	"gen_random_uuid()":  schema.GenUUIDv4,
	"CURRENT_TIMESTAMP":  schema.Now,

	// Postgres 18's built-in, which migrate.MinPostgres(18) emits in place of
	// the extension's uuid_generate_v7(). Both spellings map to the same
	// generator on the way back in, which is what stops a schema generated for
	// 18 from diffing against itself forever: the registry records the
	// generator, and which spelling reaches the database is decided when the
	// DDL is rendered.
	"uuidv7()": schema.GenUUIDv7,
}

// columnAuto reads how the database supplies a column's value: as an identity,
// as a serial, or not at all.
//
// Postgres records the two spellings in unrelated places. An identity column
// sets attidentity to 'a' or 'd'; a serial sets nothing at all — it is a plain
// integer column, a sequence, and a nextval default, and only the default says
// so. Both arrive here, so that the difference between them stays where it
// belongs, in what the DDL renders.
//
// The second return is the reason the column cannot be declared, empty when it
// can. Both refusals are arrangements Postgres permits and the DSL has no
// reading for, and they are reported here rather than left to Validate — which
// fails the whole import, where this loses one column and names it.
func columnAuto(col columnRow) (schema.Auto, string) {
	var auto schema.Auto
	switch {
	case col.Identity == "a":
		auto = schema.AutoIdentityAlways
	case col.Identity == "d":
		auto = schema.AutoIdentity
	case isSequenceDefault(col.Default):
		// A serial is a counter, and a nextval default under a text column is
		// somebody's deliberate arrangement rather than an auto-incrementing
		// key. Postgres enforces the same rule for an identity itself —
		// "identity column type must be smallint, integer, or bigint" — so
		// only the serial spelling can arrive wrong.
		if !isIntegerType(col.Type) {
			return schema.NotAuto, "column draws its default from a sequence but is a " + col.Type +
				", which the DSL has no reading for: a serial counts, and only smallint, int and bigint do"
		}
		auto = schema.AutoSerial
	default:
		return schema.NotAuto, ""
	}
	// Postgres creates both spellings NOT NULL and lets the constraint be
	// dropped afterwards, so a nullable one is rare but reachable. The DSL
	// cannot say it: a sequence always has a next value, so "nullable" is a
	// statement about a column nothing writes NULL to.
	if !col.NotNull {
		return schema.NotAuto, "column is supplied by a sequence and is also nullable, which the DSL " +
			"cannot declare: it renders the column NOT NULL, and a declaration that quietly " +
			"required a column the database allows NULL in would be a migration nobody asked for"
	}
	return auto, ""
}

// isIntegerType reports whether a Postgres type name is one of the three the
// DSL counts with. Spelled against the catalog's names rather than against
// schema.Type, because it is asked before the type has been mapped.
func isIntegerType(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "smallint", "int2", "integer", "int", "int4", "bigint", "int8":
		return true
	}
	return false
}

// isSequenceDefault reports whether a stored default draws from a sequence,
// which is what a serial column is made of. Postgres expands serial into three
// separate things — a plain integer column, a sequence it owns, and a nextval
// default — and records none of them as an identity, so attidentity is empty
// and the identity check in buildColumn does not see it.
//
// The cast here is regclass, not the column's own type, so stripCast leaves it
// alone and the expression would otherwise survive as an ordinary raw default:
// one naming a sequence that Diff never renders a CREATE for.
func isSequenceDefault(expr string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(expr)), "nextval(")
}

// columnDefault maps a stored default expression back onto a schema.Default.
//
// Postgres attaches a cast to every literal it stores — 'draft' comes back as
// 'draft'::text — so a redundant cast, one naming the column's own type, is
// stripped. That is not cosmetic: it makes the default render as the DDL layer
// would have written it, so a schema imported and then diffed against itself
// produces nothing.
//
// A cast to anything else is left alone inside a raw expression, because it is
// doing something, and this package has no business deciding what.
func columnDefault(expr, formatted string, t schema.Type) *schema.Default {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	if gen, known := knownDefaults[expr]; known {
		return gen()
	}
	if lit, ok := stripCast(expr, formatted); ok {
		if s, quoted := unquoteLiteral(lit); quoted {
			return schema.Value(s)
		}
		return schema.Expr(lit)
	}
	// Bare numbers and booleans carry no cast and render identically either
	// way, so they pass through as expressions rather than being parsed into
	// Go values whose type would then have to be guessed.
	return schema.Expr(expr)
}

// stripCast removes a trailing ::type when it names the column's own type, and
// reports whether it did.
//
// The cast is compared against the type name with its modifier removed as well
// as with it, because Postgres does not agree with itself about which one to
// use. format_type reports a length-bounded column as "character varying(20)",
// and the default it stores on that same column is 'junior'::character varying
// — no length. Comparing only the formatted name therefore never matched for a
// varchar, so the cast survived, the default came back as a raw expression
// where the declaration had a literal, and migrate.Diff proposed the same
// SET DEFAULT on every run. A gate that is red for a reason that is not real is
// worse than no gate, because it teaches people to skip reading it.
func stripCast(expr, formatted string) (string, bool) {
	cut := strings.LastIndex(expr, "::")
	if cut < 0 {
		return expr, false
	}
	cast := strings.TrimSpace(expr[cut+2:])
	if cast != formatted && cast != unmodified(formatted) {
		return expr, false
	}
	return strings.TrimSpace(expr[:cut]), true
}

// unmodified removes a type modifier from a formatted type name, keeping any
// array suffix: "character varying(20)" becomes "character varying", and
// "numeric(10,2)[]" becomes "numeric[]".
//
// Only the modifier goes. A cast naming a different type is left for
// columnDefault to keep as an expression, because it is doing something.
func unmodified(formatted string) string {
	open := strings.IndexByte(formatted, '(')
	if open < 0 {
		return formatted
	}
	shut := strings.LastIndexByte(formatted, ')')
	if shut < open {
		return formatted
	}
	return strings.TrimSpace(formatted[:open]) + strings.TrimSpace(formatted[shut+1:])
}

// unquoteLiteral turns a SQL string literal into its value.
func unquoteLiteral(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), true
}

// enumValues recovers the permitted values of an enum column from the CHECK
// that enforces it.
//
// The DDL layer writes `"status" IN ('draft', 'live')`, and Postgres stores the
// normalised form `status = ANY (ARRAY['draft'::text, 'live'::text])`. Matching
// the form that was written would have recovered nothing; this matches the form
// that comes back.
//
// Enums are text with a CHECK rather than a native Postgres enum (ADR-0017),
// which is what makes recovering them a matter of reading an expression rather
// than reading a type.
// An array enum is constrained by containment instead — `labels <@
// ARRAY['red'::text, 'green'::text]` — because the check has to hold for every
// element. Both spellings are read here, so an enum array round-trips as an
// enum array rather than falling back to plain text and diffing forever.
func enumValues(column, expr string) ([]string, bool) {
	expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(expr), "("), ")"))
	body, ok := enumCheckBody(column, expr)
	if !ok || strings.TrimSpace(body) == "" {
		return nil, false
	}
	var out []string
	for _, part := range splitTopLevel(body) {
		lit, _ := stripCastAny(strings.TrimSpace(part))
		v, quoted := unquoteLiteral(lit)
		if !quoted {
			return nil, false
		}
		out = append(out, v)
	}
	return out, len(out) > 0
}

// enumCheckBody returns the ARRAY[...] contents of whichever enum check form
// the expression is, or false if it is neither.
func enumCheckBody(column, expr string) (string, bool) {
	if rest, found := strings.CutPrefix(expr, column+" = ANY (ARRAY["); found {
		if body, closed := strings.CutSuffix(rest, "])"); closed {
			return body, true
		}
		return "", false
	}
	rest, found := strings.CutPrefix(expr, column+" <@ ARRAY[")
	if !found {
		return "", false
	}
	// The closing bracket is found by scanning rather than by trimming the
	// last one, because the stored form may carry the cast the DDL layer wrote
	// — and `::text[]` ends in a bracket of its own.
	body, tail, closed := untilCloseBracket(rest)
	if !closed {
		return "", false
	}
	if tail != "" && !strings.HasPrefix(tail, "::") {
		return "", false
	}
	return body, true
}

// untilCloseBracket splits at the bracket closing the one already open,
// ignoring brackets inside string literals.
func untilCloseBracket(s string) (body, tail string, ok bool) {
	depth := 1
	inString := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inString:
			if c == '\'' {
				// A doubled quote is an escaped one and does not end the
				// literal.
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return s[:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// stripCastAny removes a trailing cast whatever it names.
func stripCastAny(s string) (string, bool) {
	cut := strings.LastIndex(s, "::")
	if cut < 0 {
		return s, false
	}
	return strings.TrimSpace(s[:cut]), true
}

// splitTopLevel splits on commas that are not inside a string literal, so that
// a permitted value containing a comma survives.
func splitTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'' && inString:
			cur.WriteString("''")
			i++
		case s[i] == '\'':
			inString = !inString
			cur.WriteByte(s[i])
		case s[i] == ',' && !inString:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// splitList splits a comma-joined column list from the catalog queries.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// referentialAction maps confdeltype and confupdtype onto the DSL's actions.
// "a" is NO ACTION, which the DDL layer omits because it is the default.
func referentialAction(code string) (schema.RefAction, bool) {
	switch code {
	case "a", "":
		return schema.NoAction, true
	case "c":
		return schema.Cascade, true
	case "n":
		return schema.SetNull, true
	case "d":
		return schema.SetDefault, true
	case "r":
		return schema.Restrict, true
	}
	return schema.NoAction, false
}
