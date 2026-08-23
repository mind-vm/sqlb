package codegen

import (
	"regexp"
	"sort"
	"strings"
)

// Emitting the client runtime once, beside the per-schema client, rather than
// inside it.
//
// The runtime — the response envelopes, the problem document, the transport
// signature and the filter encoder — is derived from nothing schema-specific.
// One project never noticed: the file was self-contained and that was a
// feature. A second module in the same application is where it breaks, and it
// breaks differently per language.
//
// TypeScript is structurally typed, so two copies of Page interoperate and the
// cost is only that both ship. This is that fix (issue #110).
//
// Go reached this shape first and for a different reason — the transport-only
// client became its own package in #97, so a sync job could take the encoder
// without the command tree. TypeScript now agrees with it.
//
// # Dart is not done here, and the reason is not effort
//
// Dart is nominally typed, so its duplication is a defect rather than a cost:
// two Page<T> are two unrelated classes. But its runtime's contract with the
// generated client is largely *private* — Row's _str/_int/_one/_many protocol
// that every row view inherits, Cond._encode, the top-level _get/_row/_page
// helpers — and Dart privacy is per library, so none of it survives the file
// boundary. Splitting it needs that protocol made public and documented, which
// changes what a generated client exposes and is a decision rather than a
// refactor. Attempted and reverted; the finding is on #110.
//
// # Why the import list is computed rather than written down
//
// The generated client must import exactly what it uses. TypeScript is checked
// under `noUnusedLocals` and Dart under `--fatal-infos`, so an import the body
// does not reference is a build failure, and the set genuinely varies: a schema
// with no array column never mentions ArrayCond, and one with no list operation
// never mentions ListQuery. A hand-maintained list would be wrong for some
// schema and right for the fixture.
//
// So the runtime's exports are parsed out of the runtime source itself, and the
// emitted body is scanned for them. Nothing to keep in step, and a name added
// to the runtime is picked up by the next build rather than by whoever
// remembers.

// tsExportPattern matches an exported declaration in the TypeScript runtime,
// capturing whether it is a type — which `verbatimModuleSyntax` requires be
// imported with `import type` — and its name.
var tsExportPattern = regexp.MustCompile(`(?m)^export (type|interface|const|function|class) ([A-Za-z_][A-Za-z0-9_]*)`)

// runtimeSymbol is one name the runtime offers.
type runtimeSymbol struct {
	name   string
	isType bool
}

func tsRuntimeSymbols() []runtimeSymbol {
	var out []runtimeSymbol
	for _, m := range tsExportPattern.FindAllStringSubmatch(tsRuntime, -1) {
		kind, name := m[1], m[2]
		out = append(out, runtimeSymbol{
			name: name,
			// A class is a value and a type at once; imported as a value, it
			// serves both.
			isType: kind == "type" || kind == "interface",
		})
	}
	return out
}

// usesSymbol reports whether body references name as a whole identifier.
//
// Word-boundary rather than substring, so Page does not match PageParams and
// Cond does not match ArrayCond — both of which are real names in this runtime,
// and either false positive would emit an import the body does not use and fail
// the build it was meant to pass.
func usesSymbol(body, name string) bool {
	for i := 0; i+len(name) <= len(body); i++ {
		if body[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isIdentRune(body[i-1]) {
			continue
		}
		if j := i + len(name); j < len(body) && isIdentRune(body[j]) {
			continue
		}
		return true
	}
	return false
}

func isIdentRune(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// tsPrivatePattern matches a top-level declaration in the runtime that is *not*
// exported, which is what a client body must not reach for.
var tsPrivatePattern = regexp.MustCompile(`(?m)^(type|interface|const|function|class) ([A-Za-z_][A-Za-z0-9_]*)`)

// tsUnexportedUse reports a runtime helper the client body calls but the
// runtime keeps to itself.
//
// Before the split this could not happen — one file, so every helper was in
// scope — and the first thing the split broke was exactly this: itemPath was
// module-local, the client called it, and the failure arrived as ten identical
// "Cannot find name" errors from tsc rather than from the generator that caused
// them. Checked here so the next one is a sentence instead.
func tsUnexportedUse(body string) string {
	for _, m := range tsPrivatePattern.FindAllStringSubmatch(tsRuntime, -1) {
		if usesSymbol(body, m[2]) {
			return m[2]
		}
	}
	return ""
}

// tsRuntimeImports renders the import statements a client body needs, plus the
// re-export that keeps existing call sites working.
//
// The re-export is what makes this change invisible to a project that has one
// module: code doing `import type { Page } from './client.gen'` keeps
// compiling, because client.gen still offers Page — it just no longer declares
// it. Without that, splitting the file would break every consumer to fix a
// problem only multi-module consumers have.
func tsRuntimeImports(body, from string) string {
	var types, values []string
	for _, s := range tsRuntimeSymbols() {
		if !usesSymbol(body, s.name) {
			continue
		}
		if s.isType {
			types = append(types, s.name)
		} else {
			values = append(values, s.name)
		}
	}
	sort.Strings(types)
	sort.Strings(values)

	var b strings.Builder
	if len(types) > 0 {
		b.WriteString("import type { " + strings.Join(types, ", ") + " } from '" + from + "';\n")
	}
	if len(values) > 0 {
		b.WriteString("import { " + strings.Join(values, ", ") + " } from '" + from + "';\n")
	}
	// Unconditional, because it is the compatibility promise rather than a
	// consequence of what this schema happens to use.
	b.WriteString("export * from '" + from + "';\n")
	return b.String()
}

// dartDeclPattern matches a top-level Dart declaration's name, through the
// modifier soup Dart 3 allows before `class`.
var dartDeclPattern = regexp.MustCompile(
	`^(?:(?:abstract|base|interface|final|sealed|mixin) )*(?:class|enum|mixin|extension|typedef) ([A-Za-z_][A-Za-z0-9_]*)`)

var dartIdentTail = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*$`)

// dartSharedNames is what goes in the shared library, and it is a short list on
// purpose.
//
// These are the types an application names when it writes one pager widget,
// wires one transport, or catches one error across two modules — which is
// exactly what #110 reports it cannot do. Sharing them makes those one
// declaration instead of one per module.
//
// Row and the Cond family are deliberately absent, and not for want of trying.
// Dart privacy is per *library*, and both keep their contract with the
// generated client private: Row declares the _str/_int/_one/_many protocol
// every row view inherits, and the client emits `cond?._encode(out, 'id')` over
// a private _Query. Sharing either would need that protocol made public and
// documented — a change to what every generated client exposes, and a decision
// rather than a refactor.
//
// Leaving them per client costs duplication of code nothing can observe,
// because all of it is private. What it does not cost is the thing the issue is
// about: two modules now agree about Page, Transport and Problem.
var dartSharedNames = map[string]bool{
	"Collection": true, "Page": true,
	"Problem": true, "ProblemDetail": true,
	"ApiRequest": true, "Transport": true,
	"CursorPager": true, "SortTerm": true, "WireValue": true,
	"MissingColumn": true, "UnknownEnumValue": true,

	// The change feed, which is here for a reason the others are not: FeedEvent
	// is sealed, and a sealed hierarchy has to be one library. Splitting
	// ChangeEvent from FeedEvent would not merely duplicate a class, it would
	// not compile. Everything on this path is schema-independent anyway — an
	// event names its table as a string, and narrowing that string to a table
	// the client serves is the client's half.
	"ChangeOp": true, "FeedEvent": true, "ChangeEvent": true,
	"ResetEvent": true, "SseFrame": true, "sseFrames": true, "ChangeFeed": true,
}

// splitDartRuntime partitions the Dart runtime into the shared library and the
// half that stays with each client.
//
// clientBody is the schema's own generated types, and it is an input because
// the per-client half's contents depend on it: the resource sections call
// _get, _page, _row and _itemPath, and a reachability walk that only looked at
// the runtime would leave every one of them behind. The shared half does not
// depend on it, so renderDartRuntime passes nothing.
func splitDartRuntime(clientBody string) (shared, perClient string) {
	decls := dartDeclarations(dartRuntime)

	var pub, priv []dartDecl
	for _, d := range decls {
		switch {
		case d.name != "" && strings.HasPrefix(d.name, "_"):
			// Placed below, by which half reaches it.
		case dartSharedNames[d.name]:
			pub = append(pub, d)
		default:
			// Including the unnamed runs — leading comments and section
			// banners — which belong to the file they were written for.
			priv = append(priv, d)
		}
	}

	// A private helper goes wherever it is referenced, and into both halves if
	// both reference it. Duplicating one is free: a private name is scoped to
	// its library, so two copies cannot collide, and each is used where it
	// lands — which matters, because Dart is analysed with --fatal-infos and an
	// unreferenced private is an error.
	//
	// To a fixpoint, since a private helper may call another.
	return renderDecls(withPrivates(decls, pub, "")),
		renderDecls(withPrivates(decls, priv, clientBody))
}

// withPrivates adds every private declaration this half reaches, repeating
// until nothing new is pulled in. seed is source that will sit beside the half
// but is not part of it — the generated client body, whose calls count.
func withPrivates(all, half []dartDecl, seed string) []dartDecl {
	taken := map[string]bool{}
	for {
		body := seed + renderDecls(half)
		var added bool
		for _, d := range all {
			if d.name == "" || !strings.HasPrefix(d.name, "_") || taken[d.name] {
				continue
			}
			if !usesSymbol(body, d.name) {
				continue
			}
			taken[d.name] = true
			half = append(half, d)
			added = true
		}
		if !added {
			return half
		}
	}
}

func renderDecls(decls []dartDecl) string {
	var b strings.Builder
	for _, d := range decls {
		b.WriteString(d.src)
	}
	return b.String()
}

type dartDecl struct {
	src  string
	name string
}

// dartDeclarations cuts the runtime into top-level declarations, each carrying
// the doc comment and blank lines that precede it.
//
// Column zero is the boundary, with one correction that cost an afternoon: a
// line beginning at column zero is only a *new* declaration if it begins with a
// letter, an underscore or an annotation. A multi-line signature closes on a
// `)` in column zero —
//
//	List<T> _rows<T>(
//	  Map<String, dynamic> json,
//	) {
//
// — and reading that as a new declaration cuts the function in half, which the
// analyser then reports as a syntax error three declarations later.
func dartDeclarations(src string) []dartDecl {
	var out []dartDecl
	var pending, current strings.Builder
	depth, open := 0, false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		out = append(out, dartDecl{
			src:  pending.String() + current.String(),
			name: dartDeclName(current.String()),
		})
		pending.Reset()
		current.Reset()
	}

	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		starts := startsDeclaration(trimmed)

		if current.Len() == 0 && !starts {
			pending.WriteString(line)
			continue
		}
		if starts && !open && current.Len() > 0 {
			flush()
		}

		current.WriteString(line)
		depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
		switch {
		case depth > 0:
			open = true
		case open && depth == 0:
			open = false
			flush()
		case !open && strings.HasSuffix(trimmed, ";"):
			flush()
		}
	}
	flush()
	if pending.Len() > 0 {
		out = append(out, dartDecl{src: pending.String()})
	}
	return out
}

// startsDeclaration reports whether a line begins a new top-level declaration:
// column zero, and an identifier or an annotation rather than punctuation
// closing the previous one.
func startsDeclaration(line string) bool {
	if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}
	if strings.HasPrefix(line, "//") {
		return false
	}
	c := line[0]
	return c == '@' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// dartDeclName reads the declared name out of a declaration's first line.
//
// Two shapes. A keyword declaration names itself after the keyword. Everything
// else is a function or a variable, whose name is the identifier immediately
// before the parameter list or the assignment — so this cannot take the first
// identifier, which would answer "List" for `List<T> _rows<T>(…)`.
func dartDeclName(src string) string {
	line := src
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if m := dartDeclPattern.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	cut := len(line)
	for _, c := range []byte{'(', '='} {
		if i := strings.IndexByte(line, c); i >= 0 && i < cut {
			cut = i
		}
	}
	// A trailing type-parameter list is stripped. Only the trailing one:
	// cutting at the first `<` would answer the return type.
	head := strings.TrimRight(line[:cut], " ")
	if strings.HasSuffix(head, ">") {
		depth := 0
		for i := len(head) - 1; i >= 0; i-- {
			if head[i] == '>' {
				depth++
			} else if head[i] == '<' {
				depth--
				if depth == 0 {
					head = head[:i]
					break
				}
			}
		}
	}
	if m := dartIdentTail.FindStringSubmatch(head); m != nil {
		return m[1]
	}
	return ""
}

// dartRuntimeImports renders the directives a client needs.
//
// Two, and they do different jobs: `export` re-exports the shared library to
// whoever imports the client — which is what keeps `import 'client.gen.dart'`
// offering Page and Transport, and what makes two clients offer the *same*
// ones — while `import` is what brings them into this library's own scope.
func dartRuntimeImports(body, from string) string {
	var b strings.Builder
	for name := range dartSharedNames {
		if usesSymbol(body, name) {
			b.WriteString("import '" + from + "';\n")
			break
		}
	}
	b.WriteString("export '" + from + "';\n")
	return b.String()
}
