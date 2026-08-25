package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// tsDeclPattern matches a top-level declaration in emitted TypeScript. Only the
// exported ones matter: nothing this generator emits is module-local, and a
// pattern that also matched an indented member would read a property named
// `type` inside an interface as a declaration.
var tsDeclPattern = regexp.MustCompile(`(?m)^export (interface|type|const|function|class|enum) ([A-Za-z_$][A-Za-z0-9_$]*)`)

// tsDeclSpace is the declaration space a keyword occupies. TypeScript keeps
// types and values apart, so `type Foo` beside `const Foo` is legal and common;
// two declarations in the *same* space are the redeclaration tsc refuses.
func tsDeclSpace(keyword string) (types, values bool) {
	switch keyword {
	case "interface", "type":
		return true, false
	case "const", "function":
		return false, true
	case "class", "enum":
		return true, true
	}
	return false, false
}

// tsDuplicateDeclaration reports the first name the emitted source declares
// twice in one declaration space, with the two declarations that collide.
//
// It reads the emitted text rather than tracking names as they are written,
// which is what makes it cover conventions nobody thought to register — the
// next `<Entity>Something` type this emitter grows is checked the day it is
// added, without a second list to keep in step.
func tsDuplicateDeclaration(src string) (name, first, second string) {
	type decl struct{ line string }
	seenTypes := map[string]decl{}
	seenValues := map[string]decl{}

	for _, m := range tsDeclPattern.FindAllStringSubmatch(src, -1) {
		keyword, id, line := m[1], m[2], m[0]
		types, values := tsDeclSpace(keyword)
		if types {
			if prev, ok := seenTypes[id]; ok {
				return id, prev.line, line
			}
			seenTypes[id] = decl{line}
		}
		if values {
			if prev, ok := seenValues[id]; ok {
				return id, prev.line, line
			}
			seenValues[id] = decl{line}
		}
	}
	return "", "", ""
}

// generatedSuffixes describes what each generated name after a table's own type
// name is, so that a collision can be reported in the schema's terms rather
// than in the emitted file's. The two client emitters share the conventions,
// which is also why they share the bug.
var generatedSuffixes = map[string]string{
	"":            "the row type",
	"Column":      "the selectable-column type",
	"Sort":        "the sort-term type",
	"Expand":      "the expansion type",
	"Where":       "the filter type",
	"ListParams":  "the list parameters",
	"GetParams":   "the get parameters",
	"Row":         "the narrowed row",
	"WriteResult": "the write result",
	"Create":      "the create body",
	"Patch":       "the patch body",
}

// tsNameOrigins says which tables could have produced a name, and as what.
//
// A collision is always between two tables whose names are unrelated in the
// schema — `boards` and `board_columns` — and the emitted file shows neither,
// so an error naming only the identifier sends the reader to the generated
// source to work out where it came from.
func nameOrigins(reg *schema.Registry, name string) []string {
	var out []string
	for _, t := range reg.Tables() {
		typeName := TypeName(t)
		if !strings.HasPrefix(name, typeName) {
			continue
		}
		what, known := generatedSuffixes[strings.TrimPrefix(name, typeName)]
		if !known {
			// An enum's type is its table's type name plus the column's, and
			// anything else this emitter grows lands here too.
			what = "a generated type"
		}
		out = append(out, fmt.Sprintf("%s of %s", what, t.Name()))
	}
	sort.Strings(out)
	return out
}

// dartDuplicateDeclaration is tsDuplicateDeclaration over Dart's one namespace:
// there is no equivalent of tsDeclSpace, because any two top-level declarations
// sharing a name are an error whatever their kinds.
//
// It reuses dartDeclPattern, which the runtime split already keeps current
// through the modifier soup Dart 3 allows before `class`. Type-shaped
// declarations are the whole of what matters here: every name this generator
// derives from a table is one of those, and the top-level functions it emits
// are verbs (listBoards) that cannot collide with a type name.
func dartDuplicateDeclaration(src string) (name, first, second string) {
	seen := map[string]string{}
	for line := range strings.SplitSeq(src, "\n") {
		m := dartDeclPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := m[1]
		if prev, ok := seen[id]; ok {
			return id, prev, strings.TrimSuffix(strings.TrimSpace(line), " {")
		}
		seen[id] = strings.TrimSuffix(strings.TrimSpace(line), " {")
	}
	return "", "", ""
}

// collision is the error a generated file with two declarations of one name
// returns.
//
// The generator wrote a file its own compiler rejects and said nothing: `sqlb
// generate` reported success, and the failure arrived from `tsc` as two lines
// of "Duplicate identifier" naming neither the schema nor the tables involved
// (#261). Every name here is derived from a table name, so the generator is the
// only thing that can explain it.
//
// Both clients have the bug because both derive names the same way: a table's
// singularised name, and that name plus a suffix. `boards` and `board_columns`
// collide in TypeScript as an interface against a union, and in Dart as a class
// against an enum.
func collision(reg *schema.Registry, file, language, src string, dup func(string) (string, string, string)) error {
	name, first, second := dup(src)
	if name == "" {
		return nil
	}

	where := ""
	if origins := nameOrigins(reg, name); len(origins) > 0 {
		where = " — " + strings.Join(origins, ", and ")
	}
	return fmt.Errorf(
		"codegen: %s declares %s twice%s. %s\n\t%s\n\t%s\n"+
			"A table's generated names are its own singularised name, and that name "+
			"plus a suffix, unless TableDef.TypeName pins a different one: give the "+
			"colliding table a TypeName override, or rename the table itself if the "+
			"SQL name should change too",
		file, name, where, language, first, second)
}

// tsCollision and dartCollision are the two call sites, named so that the
// emitters read as what they are rather than as a shared helper's arguments.
func tsCollision(reg *schema.Registry, file, src string) error {
	return collision(reg, file, "TypeScript reads the second as a redeclaration (TS2300) and the file does not compile:",
		src, tsDuplicateDeclaration)
}

func dartCollision(reg *schema.Registry, file, src string) error {
	return collision(reg, file, "Dart has one top-level namespace, so the second declaration is an error and the file does not compile:",
		src, dartDuplicateDeclaration)
}
