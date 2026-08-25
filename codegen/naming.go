package codegen

import (
	"strings"
	"unicode"

	"github.com/jryannel/sqlb/schema"
)

// initialisms are rendered upper-case in Go identifiers, following the
// convention the standard library and every Go linter expect: org_id becomes
// OrgID rather than OrgId.
var initialisms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI", "api": "API", "db": "DB",
	"http": "HTTP", "https": "HTTPS", "json": "JSON", "xml": "XML",
	"sql": "SQL", "uuid": "UUID", "ip": "IP", "cpu": "CPU", "ttl": "TTL",
	"acl": "ACL", "ssh": "SSH", "tls": "TLS", "ui": "UI", "eof": "EOF",
}

// GoName converts a snake_case SQL identifier to an exported Go name.
//
//	org_id        → OrgID
//	created_at    → CreatedAt
//	password_hash → PasswordHash
func GoName(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part == "" {
			continue
		}
		if up, ok := initialisms[strings.ToLower(part)]; ok {
			b.WriteString(up)
			continue
		}
		r := []rune(part)
		b.WriteRune(unicode.ToUpper(r[0]))
		b.WriteString(string(r[1:]))
	}
	return b.String()
}

// EnumConst is the constant-name suffix for one enum value: the value with
// every run of characters that cannot appear in a Go identifier treated as a
// word boundary.
//
//	draft          → Draft
//	task.assigned  → TaskAssigned
//	image/png      → ImagePng
//
// The value itself is never touched — it stays verbatim on the right of the
// `=`, because it is data and the constant name is not. Deriving the name by
// title-casing the value whole emitted `NotificationTypeTask.assigned`, which
// does not parse, so a value set spelled with the ordinary dotted namespacing
// convention could not be declared at all (issue #138).
//
// `_` was already the word boundary, so this is GoName over a value normalised
// to underscores: the initialism table still reaches api.key and the rule stays
// one rule rather than two.
//
// A leading digit needs no escape here. The name is always emitted with the
// enum's type name in front of it, so `2fa.enabled` becomes
// NotificationType2faEnabled and starts with an N. An empty value has no word
// in it at all, and takes a name rather than colliding with the bare type name.
func EnumConst(value string) string {
	var norm strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			norm.WriteRune(r)
			continue
		}
		norm.WriteRune('_')
	}
	if name := GoName(norm.String()); name != "" {
		return name
	}
	return "Empty"
}

// unexportedGoName is GoName with a lower-case first letter, for the private
// column-set types. It avoids producing a Go keyword.
func unexportedGoName(s string) string {
	name := GoName(s)
	if name == "" {
		return name
	}
	r := []rune(name)
	// A leading initialism must be lowered whole, or ID becomes iD.
	lead := 0
	for lead < len(r) && unicode.IsUpper(r[lead]) {
		lead++
	}
	if lead > 1 && lead < len(r) {
		lead-- // the last upper-case rune starts the next word
	}
	for i := 0; i < lead; i++ {
		r[i] = unicode.ToLower(r[i])
	}
	out := string(r)
	if isGoKeyword(out) {
		return out + "_"
	}
	return out
}

func isGoKeyword(s string) bool {
	switch s {
	case "break", "case", "chan", "const", "continue", "default", "defer", "else",
		"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
		"map", "package", "range", "return", "select", "struct", "switch", "type", "var":
		return true
	}
	return false
}

// Singular is a deliberately small English singulariser, the inverse of the
// pluraliser in the runtime.
//
// It only has to produce a readable Go type name: correctness does not depend
// on it, because every generated model carries an explicit TableName method, so
// a wrong guess is cosmetic rather than a mapping bug.
func Singular(s string) string {
	lower := strings.ToLower(s)
	switch {
	case strings.HasSuffix(lower, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(lower, "ches"), strings.HasSuffix(lower, "shes"),
		strings.HasSuffix(lower, "xes"), strings.HasSuffix(lower, "zes"),
		strings.HasSuffix(lower, "sses"):
		return s[:len(s)-2]
	case strings.HasSuffix(lower, "ss"):
		return s // "address" is already singular
	case strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "us"):
		return s[:len(s)-1]
	}
	return s
}

// TypeName is the Go/TypeScript type name for a table: [TableDef.TypeNameOverride]
// if the schema pinned one, otherwise the singular of its local name, exported.
// A module prefix is deliberately not included — billing_invoices yields
// Invoice, because the prefix is a storage concern and the package already
// provides the namespace in Go.
func TypeName(t *schema.TableDef) string {
	if ov := t.TypeNameOverride(); ov != "" {
		return ov
	}
	return GoName(Singular(t.LocalName()))
}

// plural picks between two spellings for n, which is the whole of what a
// generated sentence needs and less than a dependency. schema has its own for
// the same reason: the two packages do not share a helper file, and this is
// four lines.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
