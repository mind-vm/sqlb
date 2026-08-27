package codegen

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// Options configures a generation run.
type Options struct {
	// Registry supplies the tables. Required.
	Registry *schema.Registry
	// Dir is the output directory. Required.
	Dir string
	// Package is the package clause for generated Go. Required.
	Package string

	// ModelsFile, ColumnsFile, ManifestFile and RestFile override the default
	// names. Set one to "-" to skip that artefact.
	//
	// RestFile is written only when the schema exposes at least one table, so a
	// package with no REST surface does not acquire a dependency on huma.
	ModelsFile   string
	ColumnsFile  string
	ManifestFile string
	RestFile     string

	// TSDir emits the TypeScript client, into a directory relative to Dir —
	// "web/src/api" in a repository whose frontend lives beside its server.
	// Empty means no client is emitted at all, which is the right default for a
	// project that has no TypeScript consumer.
	//
	// Relative to Dir, not to the module root, and the difference is worth a
	// sentence because getting it wrong is silent. With Dir "sqlbdata" in a
	// module that is itself a subdirectory, reaching a frontend beside the
	// module takes two levels — "../../web/src/api" — and one level writes a
	// complete, correct client into sqlbdata/../web/src/api, which nothing
	// imports while the real frontend goes on building against the client it
	// already had. generate warns when this directory and its parent both had
	// to be created, which is what that mistake looks like from here (#290).
	//
	// Two escapes remove the arithmetic instead of warning about getting it
	// wrong ([Options.resolveClientDir]): an absolute path is used verbatim,
	// and a path prefixed "//" resolves against the module root — the
	// directory holding the nearest go.mod — rather than against Dir. A
	// repository laid out server/ + web/ + mobile/ writes "//web/src/api"
	// and means exactly that, at any Dir depth.
	//
	// Three files land there. The runtime and the client are dependency-free;
	// the queries file holds the queryOptions, infiniteQueryOptions and
	// mutationOptions factories and takes @tanstack/react-query as a peer
	// dependency, so a project that does not use it sets TSQueriesFile to "-"
	// and keeps the rest.
	TSDir         string
	TSClientFile  string
	TSQueriesFile string

	// TSRuntimeFile names the file holding the part of the client that does
	// not depend on the schema — the envelopes, the problem document, the
	// transport signature and the filter encoder. Defaults to runtime.gen.ts.
	//
	// It is a separate file because a second module in the same application
	// otherwise ships a second copy of all of it, and asks the application to
	// wire one Transport per module (#110). Point two projects at one path and
	// they share it: the content is derived from nothing schema-specific, so
	// the second writer produces the same bytes and `check` stays meaningful.
	TSRuntimeFile string

	// CLIDir emits a cobra command-line client, into a directory relative to
	// Dir — "cli" in a repository whose binary lives beside its server. Empty
	// means no CLI is emitted, which is the right default for a project that
	// has no use for one.
	//
	// The emitted package depends on github.com/spf13/cobra and nothing else
	// beyond the standard library. It does not import sqlb or the generated
	// models: it speaks to the API over HTTP, so it holds no database
	// credential and needs no build tag to keep one out.
	//
	// CLIName is the binary's name, which is what appears in usage lines and,
	// upper-cased, as the prefix of the environment variables the root command
	// reads: "taskctl" gives TASKCTL_BASE_URL and TASKCTL_TOKEN. It defaults to
	// Package.
	CLIDir     string
	CLIPackage string
	CLIName    string
	CLIFile    string

	// ClientDir emits the transport-only Go client — Request, Transport,
	// Client, Do, Run and the typed problem document — into a directory
	// relative to Dir. The emitted package imports the standard library and
	// nothing else.
	//
	// It is a separate package from the CLI because it is a separate artefact.
	// A sync job, a server-to-server caller, or an admin tool that already has
	// a command tree of its own wants the typed encoder and not a command-line
	// framework, and while the two shared a package it could not have one
	// without the other (#97).
	//
	// Setting CLIDir and leaving this empty emits the client into a "client"
	// subdirectory of CLIDir, because the command tree has to import it from
	// somewhere. Setting this and leaving CLIDir empty emits the client alone,
	// which is the server-to-server case.
	ClientDir     string
	ClientPackage string
	ClientFile    string

	// ClientImportPath is the path the generated CLI imports the generated
	// client under. Empty derives it: the module path out of the nearest
	// go.mod, joined with Dir and the client's directory.
	//
	// Deriving it is right for a repository generating into itself, which is
	// every project using sqlb generate. It cannot be right for a caller whose
	// Dir is an absolute path, or who generates into a module it is not inside
	// — so the derivation is a default rather than the mechanism.
	ClientImportPath string

	// DartDir emits a typed Dart client, into a directory relative to Dir —
	// "mobile/lib/api" in a repository whose Flutter app lives beside its
	// server. Empty means no client is emitted, which is the right default for
	// a project that has no Dart consumer.
	//
	// Relative to Dir rather than the module root, with the same two-level
	// arithmetic and the same silent failure as [Options.TSDir]; see there.
	//
	// Two files land there — the client and the runtime library it exports —
	// and neither imports anything: not a pub package, not even dart:io. There
	// is no framework layer to make optional, because the mobile ecosystem has
	// no equivalent of TanStack Query to bind to — the cursor pager it emits
	// instead is plain Dart (ADR-0031).
	DartDir  string
	DartFile string

	// DartRuntimeFile names the shared Dart library, defaulting to
	// runtime.gen.dart. It holds the response envelopes, the problem document
	// and the transport signature — the types an application names when it
	// writes one pager or wires one transport across two modules (#110).
	DartRuntimeFile string

	// SkillDir emits the project-specific agent skill, into a directory
	// relative to Dir — ".claude/skills" in a repository whose agents read from
	// there. Empty means no skill is emitted, and that is the default on
	// purpose: this is the one emitter that writes into a directory sqlb does
	// not own, beside files a project wrote itself, so it is opted into rather
	// than arrived at (ADR-0049).
	//
	// One file lands there, at <SkillDir>/<SkillName>/SKILL.md — so
	// ".claude/skills/sqlb-schema/SKILL.md" unless SkillName says otherwise. It
	// describes what this schema exposes and what each resource accepts, which is
	// the answer no static document can carry, because capabilities are opt-in and
	// therefore per-project. Being covered by `sqlb check` is the load-bearing
	// half: a skill that has drifted from the schema is worse than no skill,
	// since it is confidently wrong about the one thing it exists to know.
	//
	// It carries structure — names, types, capability flags, paths — and not
	// comments. See skill.go for why that is a trust boundary and not a style
	// choice.
	//
	// # Where to point it, and what the agent tooling does with it
	//
	// ".claude/skills" relative to the module root is the answer for an ordinary
	// single-module project, because that is a *project* skill and the tooling
	// reads those when the session starts.
	//
	// **A repository with more than one registry wants one SkillDir per
	// registry**, and module-local placement is how to get it: point each
	// module's SkillDir at a `.claude/skills` beside that module. The skill
	// answers per-registry questions — which columns are filterable *here* — so a
	// module-local one is the right scope as well as the safe one, and a nested
	// `.claude/skills` is directory-scoped by the tooling, meaning sixteen skills
	// all named `sqlb-schema` are sixteen correctly-scoped skills rather than
	// sixteen collisions.
	//
	// Pointing two registries at one SkillDir under one SkillName is
	// last-writer-wins, and it is order-dependent: whichever module generated
	// second is current and the other's `sqlb check` is red, with "run: sqlb
	// generate" as advice that cannot work, because running it reddens the first
	// (#142). SkillName is the way to share a directory deliberately.
	//
	// One consequence worth knowing before wiring this up, and it is not sqlb's
	// to fix: a skills directory that did not exist when the session started may
	// not be watched, so the first `sqlb generate` that creates one can emit a
	// skill that is not offered until the session restarts. Observed once to be
	// picked up immediately, so treat it as a possibility rather than a rule.
	// After the directory exists, edits to it are picked up live — which is what
	// makes the `sqlb check` gate worth having.
	//
	// A nested module is discovered later than the root: a `.claude/skills` below
	// the repository root is read once a file in that subtree has been, rather
	// than at startup. It works; it arrives late. A **single-registry** project
	// that wants the skill offered from the first turn can point SkillDir at the
	// repository root's `.claude/skills` even when the schema lives in a nested
	// module. With more than one registry that placement is the clobber above,
	// and it needs a distinct SkillName per registry.
	SkillDir string

	// SkillName is the skill's directory and the name in its frontmatter,
	// defaulting to "sqlb-schema". It is the second half of
	// <SkillDir>/<SkillName>/SKILL.md.
	//
	// It exists so that a repository with several registries can share one
	// SkillDir — "sqlb-schema-waitlist" and "sqlb-schema-tenants" under the
	// repository root, which is what makes root placement available to a
	// multi-module repository at all (#142). A project whose skills are
	// module-local wants the default: the directory already distinguishes them,
	// and one name across sixteen modules is a name a reader learns once.
	//
	// Keep the `sqlb-` prefix on anything you set. This file lands in a directory
	// sqlb does not own, beside skills the project wrote itself and skills it
	// installed, and a collision there is a silently shadowed instruction.
	// Refused if it is not a single lowercase kebab-case path segment, because
	// the agent tooling names a skill by its directory and an unloadable skill
	// fails by being quietly absent rather than by erroring.
	SkillName string

	// SkillSchemaPackage is how the emitted skill spells this project in the
	// commands it tells an agent to run: "./taskschema", the same argument
	// `sqlb generate` takes.
	//
	// Empty falls back to `go generate ./...`, which is correct for any project
	// whose schema package carries the directive and is the reason this is not
	// required. It cannot be derived here: the package pattern is an argument to
	// cmd/sqlb, and the emitters are given a registry rather than the pattern
	// that produced one.
	SkillSchemaPackage string

	// WiringDir emits this schema's fx wiring — an `fx.Option` value named
	// FxModule, joining the migration history and the resource mount to the
	// value groups WiringMigrations and WiringOperations name — into a
	// directory relative to Dir. Empty means the wiring lands directly in Dir
	// itself, alongside models_gen.go and rest_gen.go, which is the right
	// default: "output module-local, into Options.Dir, never a shared
	// directory" is the property that keeps two modules' wiring from
	// clobbering each other the way #142 did for SkillDir.
	//
	// Setting WiringDir alone emits nothing: what is actually generated is
	// gated on WiringMigrations and WiringOperations, each independently, so
	// a project with only a resource mount to contribute leaves
	// WiringMigrations unset and gets no migrations-shaped provider (ADR-0059).
	WiringDir     string
	WiringPackage string
	WiringFile    string

	// WiringMigrations and WiringOperations each configure one fx
	// value-group contribution: the migration history and the resource
	// mount. See WiringSet's fields for what each carries and ADR-0059 for
	// why there are exactly these two shapes and not a general one.
	//
	// WiringOperations is silently skipped, the same way RestFile is, when
	// the schema exposes no REST resource — there is no Register function
	// generated to call. It is also refused outright when the schema
	// declares actions or queries: Register then needs hand-written
	// Actions/Queries funcs this emitter cannot supply, so that module wires
	// its resource mount by hand instead.
	WiringMigrations WiringSet
	WiringOperations WiringSet

	// Types replaces the Go type emitted for the columns each override
	// matches — the sqlc `overrides:` equivalent, and the reason a codebase
	// whose ids are uuid.UUID rather than string can generate its models
	// rather than describing hand-written ones.
	//
	// An override reaches the models, the typed column facade, the REST bodies
	// and the manifest, and reaches nothing else. It does not change the SQL
	// type, and it does not change the wire: the TypeScript and Dart clients,
	// the CLI and the OpenAPI document all map from the schema type, so an
	// override is invisible to them. ADR-0035 records why that split is the
	// load-bearing part.
	Types []TypeOverride
}

func (o Options) modelsFile() string   { return orDefault(o.ModelsFile, "models_gen.go") }
func (o Options) columnsFile() string  { return orDefault(o.ColumnsFile, "columns_gen.go") }
func (o Options) manifestFile() string { return orDefault(o.ManifestFile, "sqlb.json") }
func (o Options) restFile() string     { return orDefault(o.RestFile, "rest_gen.go") }

func (o Options) skillName() string { return orDefault(o.SkillName, defaultSkillName) }

func (o Options) tsClientFile() string  { return orDefault(o.TSClientFile, "client.gen.ts") }
func (o Options) tsQueriesFile() string { return orDefault(o.TSQueriesFile, "queries.gen.ts") }
func (o Options) tsRuntimeFile() string { return orDefault(o.TSRuntimeFile, "runtime.gen.ts") }

// tsRuntimeImport is how the client names the runtime in an import: the file,
// relative to itself, by its real name.
//
// Relative and inside the directory the generator already owns, which is what
// makes this need no configuration in the ordinary case — unlike Go, where the
// same split needed ClientImportPath because a Go import is a module path
// rather than a file path.
//
// # Why the extension is written
//
// The other generated import — queries.gen.ts naming client.gen — omits it,
// and gets away with it because that file is only ever typechecked. This one is
// in a file that *runs*: the client is imported directly by tests under
// `node --test` with type stripping, and Node's resolver needs a real path.
//
// Omitting it would be a bundler assumption, and "no bundler assumption" is a
// property this client claims in its own header. It needs
// `allowImportingTsExtensions` in tsconfig, which a project consuming the
// client as source rather than compiling it already has.
func (o Options) tsRuntimeImport() string {
	return "./" + o.tsRuntimeFile()
}

func (o Options) dartFile() string { return orDefault(o.DartFile, "client.gen.dart") }

func (o Options) dartRuntimeFile() string {
	return orDefault(o.DartRuntimeFile, "runtime.gen.dart")
}

func (o Options) cliFile() string    { return orDefault(o.CLIFile, "cli_gen.go") }
func (o Options) clientFile() string { return orDefault(o.ClientFile, "client_gen.go") }

// clientDir is where the client package lands: ClientDir when set, and a
// "client" subdirectory of the CLI otherwise, so that setting CLIDir alone
// still produces something the command tree can import.
func (o Options) clientDir() string {
	if o.ClientDir != "" {
		return o.ClientDir
	}
	if o.CLIDir != "" {
		return filepath.Join(o.CLIDir, "client")
	}
	return ""
}

// clientPackage is the package clause of the emitted client, defaulting to the
// last element of its directory, as cliPackage does.
func (o Options) clientPackage() string {
	if o.ClientPackage != "" {
		return o.ClientPackage
	}
	return filepath.Base(o.clientDir())
}

// clientImportPath is the path the CLI imports the client package under.
//
// It needs the module path, which is the one thing about a generated import
// that cannot be derived from the schema. Reading go.mod is what keeps it off
// the Options struct for the common case; Module is the override for a caller
// generating outside a module root.
func (o Options) clientImportPath() (string, error) {
	if o.ClientImportPath != "" {
		return o.ClientImportPath, nil
	}
	if filepath.IsAbs(o.Dir) {
		return "", fmt.Errorf(
			"codegen: Options.Dir %q is absolute, so the generated CLI's import of the generated client "+
				"cannot be derived from the module path; set Options.ClientImportPath", o.Dir)
	}
	mod, err := moduleFromGoMod()
	if err != nil {
		return "", fmt.Errorf(
			"codegen: the generated CLI imports the generated client, which needs the module path: %w; "+
				"set Options.ClientImportPath", err)
	}
	return path.Join(mod, filepath.ToSlash(o.Dir), filepath.ToSlash(o.clientDir())), nil
}

// clientDirResolution names which rule resolved a TSDir/DartDir value, so a
// warning about the result can explain the rule that actually applied instead
// of always naming the default one.
type clientDirResolution int

const (
	resolvedAgainstDir clientDirResolution = iota
	resolvedAbsolute
	resolvedAgainstModuleRoot
)

// resolveClientDir resolves a TSDir/DartDir value to what render puts in the
// files map: dir itself, unresolved, for the default case that joins against
// Dir the way every other emitter's output does — generate and Check do that
// join once, later, for every file alike. The two escapes below need no later
// join, so they resolve to an absolute path here and generate/Check pass an
// absolute name through unchanged; see the filepath.IsAbs checks there.
//
// Two escapes exist because #290's second report found the default's
// join-against-Dir arithmetic anxious even done correctly: a repository laid
// out server/ + web/ + mobile/ has to simulate filepath.Join in its head to
// know how many "../" reach a directory beside the Go module rather than
// under it.
//
//   - An absolute path is used verbatim.
//   - A path prefixed "//" resolves against the module root — the directory
//     holding the nearest go.mod, walking up from the working directory —
//     instead of against Dir. "//web/src/api" means that, regardless of what
//     Dir is or how deep it is nested.
//
// Neither collides with an existing configuration: filepath.Join(Dir, dir)
// already cleans a leading "/" out of dir, so nothing valid before today meant
// something different starting now.
func (o Options) resolveClientDir(field, dir string) (string, clientDirResolution, error) {
	switch {
	// Checked first: "//web/api" also satisfies filepath.IsAbs (it starts with
	// "/"), and is the more specific of the two — filepath.Clean would collapse
	// it to a single leading slash and silently treat it as the literal
	// absolute path "/web/api" otherwise.
	case strings.HasPrefix(dir, "//"):
		root, err := moduleRootDir()
		if err != nil {
			return "", 0, fmt.Errorf(
				"codegen: Options.%s %q is module-root-relative, which needs a go.mod above the "+
					"working directory: %w", field, dir, err)
		}
		return filepath.Join(root, strings.TrimPrefix(dir, "//")), resolvedAgainstModuleRoot, nil
	case filepath.IsAbs(dir):
		return dir, resolvedAbsolute, nil
	default:
		return dir, resolvedAgainstDir, nil
	}
}

// moduleFromGoMod reads the module path out of the nearest go.mod, walking up
// from the working directory.
func moduleFromGoMod() (string, error) {
	dir, err := moduleRootDir()
	if err != nil {
		return "", err
	}
	return readModulePath(filepath.Join(dir, "go.mod"))
}

// moduleRootDir finds the directory holding the nearest go.mod, walking up
// from the working directory.
//
// Walking rather than reading "./go.mod" because a caller writing its own
// generator runs it from wherever it likes, and the module root is the one
// place the answer is. No dependency on golang.org/x/mod for one line of it.
func moduleRootDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", mustGetwd())
		}
		dir = parent
	}
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "the working directory"
	}
	return dir
}

func readModulePath(name string) (string, error) {
	src, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(src), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "module"); found {
			if mod := strings.TrimSpace(rest); mod != "" {
				return mod, nil
			}
		}
	}
	return "", fmt.Errorf("%s declares no module path", name)
}
func (o Options) cliName() string { return orDefault(o.CLIName, o.Package) }

// cliPackage is the package clause of the emitted CLI. It defaults to the last
// element of CLIDir, which is what a reader would guess from the import path.
//
// A directory name that is not an identifier is refused rather than repaired.
// Quietly turning "api-client" into "apiclient" would emit a package under a
// name nothing in the project mentions, and the import that failed to compile
// would be the first anyone heard of it.
func (o Options) cliPackage() string {
	if o.CLIPackage != "" {
		return o.CLIPackage
	}
	return filepath.Base(o.CLIDir)
}

// tsClientImport is the specifier the queries file imports the client by: a
// sibling module, named without its extension, which is what a bundler
// resolver expects.
func (o Options) tsClientImport() string {
	return "./" + strings.TrimSuffix(o.tsClientFile(), ".ts")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (o Options) validate() error {
	switch {
	case o.Registry == nil:
		return fmt.Errorf("codegen: Options.Registry is required")
	case o.Dir == "":
		return fmt.Errorf("codegen: Options.Dir is required")
	case o.Package == "":
		return fmt.Errorf("codegen: Options.Package is required")
	}
	// The CLI lands in a package of its own, so its clause is derived from a
	// directory name and can fail to be an identifier — "web/api-client" reads
	// fine as a path and does not compile as a package. Caught here, naming the
	// option to set, rather than by go/format, which parses without checking
	// that a package name is one.
	if dir := o.clientDir(); dir != "" && !isGoIdent(o.clientPackage()) {
		return fmt.Errorf(
			"codegen: the generated client lands in %q, giving the package name %q, which is not a Go identifier; set Options.ClientPackage",
			dir, o.clientPackage())
	}
	if o.CLIDir != "" && !isGoIdent(o.cliPackage()) {
		return fmt.Errorf(
			"codegen: Options.CLIDir %q gives the package name %q, which is not a Go identifier; set Options.CLIPackage",
			o.CLIDir, o.cliPackage())
	}
	// Checked whenever it is set, rather than only when a skill is emitted: a
	// SkillName with no SkillDir is a project that meant to opt in, and telling
	// it the name is unusable is more useful than silently writing nothing.
	if o.SkillName != "" && !isSkillName(o.SkillName) {
		return fmt.Errorf(
			"codegen: Options.SkillName is %q, which is not a usable skill directory; it must be "+
				"one path segment of lowercase letters, digits and hyphens, starting with a letter "+
				"or digit — the agent tooling names a skill by its directory, and one it cannot "+
				"load is quietly absent rather than an error. Try %q",
			o.SkillName, defaultSkillName+"-something")
	}
	if err := o.wiringValidate(); err != nil {
		return err
	}
	return nil
}

// defaultSkillName is where the skill lands when a project does not say. The
// sqlb- prefix is deliberate: this file is written into a directory sqlb does
// not own, beside skills a project wrote itself and skills it installed, and a
// collision there is a silently shadowed instruction.
const defaultSkillName = "sqlb-schema"

// isSkillName reports whether s can be a skill directory.
//
// One segment, so nothing here can climb out of SkillDir or nest a skill where
// the tooling would not look for one. Lowercase kebab-case because that is the
// convention every skill in the ecosystem follows, and a name that only *mostly*
// follows it fails by not being offered — which looks exactly like a skill the
// model chose not to load.
func isSkillName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			// Doubled hyphens read as a typo and would survive round-tripping
			// through a directory name, so they are refused with the rest.
			if s[i-1] == '-' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Generate writes the generated files and returns their paths.
//
// The schema is validated first. Generating from a schema with a known
// authoring error would produce plausible-looking Go that encodes the mistake,
// which is harder to debug than refusing.
func Generate(opts Options) ([]string, error) {
	written, _, err := generate(opts)
	return written, err
}

// generate is Generate's implementation, with one extra return Generate
// throws away: how many files it actually rewrote, a missing file counting as
// a rewrite.
//
// A file whose rendered bytes match what is already on disk is not written at
// all. That is not an optimisation — writing 9,000 lines is nothing — it is
// what keeps a language server usable. gopls invalidates a package on the
// filesystem event, not on the content: golang.org/x/tools/gopls@v0.21.1's
// snapshot.clone marks the containing package and every package that imports
// it as needing a re-typecheck for any watched write, and only skips the
// heavier `go list` reload when the file hash is unchanged. Generated code is
// what the rest of a project imports — in a typical layout models_gen.go sits
// in the package everything depends on — so a no-op `go generate` used to
// throw away the type information for the whole module. Not touching the file
// means no event, and no event means nothing to re-index (#269).
//
// The driver's "generate" verb (Run, in project.go) uses the count to warn
// that dependencies may have changed — the case in point being a schema that
// newly reaches for outbox/events, which pulls huma's SSE adapter package
// into rest_gen.go only once this call has produced code that imports it.
// `go mod tidy` run before generate had no way to see that; nothing after
// `go generate ./...` prompts a second one (#204). The check is "did any
// generated file's bytes change", not "did an import specifically appear":
// parsing import diffs across Go, TypeScript, Dart and CLI output for one bit
// of signal is a lot of surface for a warning that is cheap to over-fire — a
// generate that only reformats a comment nudges once for nothing, which costs
// far less than staying silent the time it matters.
func generate(opts Options) (written []string, rewrote int, err error) {
	files, err := render(opts)
	if err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, 0, err
	}
	for _, name := range sortedKeys(files) {
		// Every other name is relative to Dir and joined here. A TSDir/DartDir
		// escaped to an absolute path or the module root (resolveClientDir) is
		// already the real path — joining it against Dir a second time would
		// mangle it, since filepath.Join does not treat a later absolute
		// element specially.
		path := name
		if !filepath.IsAbs(name) {
			path = filepath.Join(opts.Dir, name)
		}
		// A name may carry a subdirectory — the TypeScript client is emitted
		// into one — so the parent is created per file rather than once above.
		if dir := filepath.Dir(path); dir != opts.Dir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, 0, err
			}
		}
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, files[name]) {
			// Byte-identical: leave the file, and its mtime, alone.
			written = append(written, path)
			continue
		}
		rewrote++
		if err := replaceFile(path, files[name]); err != nil {
			return nil, 0, err
		}
		written = append(written, path)
	}
	return written, rewrote, nil
}

// strandedClientDir is a TypeScript or Dart client directory that generate had
// to create along with its parent.
//
// The mistake it exists for is one "../" too few (#290). Left at the default,
// TSDir and DartDir resolve against Dir rather than against the module root,
// so a repository whose frontend sits beside its Go module needs two levels
// to reach it, and one level lands the client *inside* the module — where it
// is created, written correctly and imported by nothing. Neither tsc nor the
// Flutter build has anything to say about that: the real application goes on
// compiling against the client it already had, both stay green, and the first
// symptom is someone opening the generated client months later and finding it
// describes a schema that has moved. [Options.resolveClientDir]'s absolute and
// module-root escapes remove the arithmetic that causes this, but not the
// possibility of a fresh directory being the wrong one, so this still checks
// after either.
//
// The reported path did not help either, and could not: filepath.Join cleans
// "sqlbdata/../web/src/api" down to "web/src/api", which is a correct
// module-root-relative path that reads exactly like the repository's real web/
// directory. So the signal here is not the path — it is that the *tree* was
// new, which is the one thing that differs between the two cases.
//
// A warning rather than a refusal, because nothing here can know which was
// meant: emitting into a directory that does not exist yet is legitimate the
// first time, and the second run is silent because by then it does not.
//
// Scoped to the two clients whose consumer is not a Go compiler. A CLIDir or
// ClientDir that resolved somewhere unintended is still a Go package, and the
// import that stops resolving says so on the next build. SkillDir's usual value
// is ".claude/skills", a two-level tree that legitimately does not exist in a
// repository that has not had an agent in it yet, so it would warn on the very
// case it is meant to be quiet about.
type strandedClientDir struct {
	field      string // the Options field, so the message names what to edit
	configured string // the value as the project wrote it
	resolved   string // where it actually landed, absolute where that is knowable
	via        clientDirResolution
}

// strandedClientDirs reports the client directories generate is about to create
// from nothing. It has to be called before generate, which creates them.
func strandedClientDirs(opts Options) ([]strandedClientDir, error) {
	var out []strandedClientDir
	for _, c := range []struct{ field, dir string }{
		{"TSDir", opts.TSDir},
		{"DartDir", opts.DartDir},
	} {
		if c.dir == "" {
			continue
		}
		resolved, via, err := opts.resolveClientDir(c.field, c.dir)
		if err != nil {
			return nil, err
		}
		path := resolved
		if via == resolvedAgainstDir {
			path = filepath.Join(opts.Dir, resolved)
		}
		if isDir(path) || isDir(filepath.Dir(path)) {
			continue
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		out = append(out, strandedClientDir{field: c.field, configured: c.dir, resolved: path, via: via})
	}
	return out, nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// warning is what the driver prints. It carries the absolute path because the
// relative one is the half of this that already looked right.
//
// The explanation names whichever rule actually resolved the path: the
// default's "../" arithmetic against Options.Dir for the ordinary case, or —
// for a path that already opted into an absolute or module-root escape — that
// the directory is simply new, since the escape means there was no arithmetic
// to get wrong.
func (s strandedClientDir) warning(dir string) string {
	explanation := fmt.Sprintf(
		"sqlb:   %s resolves against Options.Dir (%q), not against the module root, so a "+
			"directory beside the Go module takes one \"../\" more than it reads like.\n",
		s.field, dir)
	if s.via != resolvedAgainstDir {
		explanation = fmt.Sprintf(
			"sqlb:   %s %q resolved to a directory that does not exist yet, which is ordinary "+
				"for a client's first run and worth a second look otherwise.\n", s.field, s.configured)
	}
	return fmt.Sprintf(
		"sqlb: %s %q named a directory that did not exist, nor did its parent — created %s\n"+
			"%s"+
			"sqlb:   A client is emitted beside the code that imports it. If nothing there imports "+
			"it, the application is still building against the client it already had, and will keep "+
			"doing so without complaining.\n",
		s.field, s.configured, s.resolved, explanation)
}

// Check reports which generated files are missing or out of date, without
// writing anything.
//
// Generated code is committed, so it drifts: someone edits the schema, forgets
// to regenerate, and the committed models silently describe a table that no
// longer exists. Run it as a CI gate — an empty result means the tree is
// current.
func Check(opts Options) ([]string, error) {
	files, err := render(opts)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, name := range sortedKeys(files) {
		path := name
		if !filepath.IsAbs(name) {
			path = filepath.Join(opts.Dir, name)
		}
		existing, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			stale = append(stale, path+" (missing)")
		case err != nil:
			return nil, err
		case !bytes.Equal(existing, files[name]):
			stale = append(stale, path+" (out of date)")
		}
	}
	return stale, nil
}

// render produces the generated files in memory.
func render(opts Options) (map[string][]byte, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := opts.Registry.Validate(); err != nil {
		return nil, fmt.Errorf("codegen: schema does not validate, refusing to generate:\n%w", err)
	}
	if len(opts.Registry.Tables()) == 0 {
		return nil, fmt.Errorf("codegen: registry has no tables (is the schema package imported for its side effects?)")
	}

	files := map[string][]byte{}

	if name := opts.modelsFile(); name != "-" {
		src, err := renderModels(opts)
		if err != nil {
			return nil, err
		}
		files[name] = src
	}
	if name := opts.columnsFile(); name != "-" {
		src, err := renderColumns(opts)
		if err != nil {
			return nil, err
		}
		files[name] = src
	}
	if name := opts.restFile(); name != "-" {
		src, err := renderREST(opts)
		if err != nil {
			return nil, err
		}
		// A nil result means nothing is exposed, which is not an error and
		// should not leave an empty file behind.
		if src != nil {
			files[name] = src
		}
	}
	if name := opts.manifestFile(); name != "-" {
		m := opts.Registry.BuildManifest()
		dropForeignFromManifest(m, opts)
		// The manifest reports what was generated rather than what the default
		// mapping would have produced: its goType field exists for a reader
		// deciding how to call the generated code, and an override changed
		// that answer (ADR-0035).
		if err := applyOverridesToManifest(m, opts); err != nil {
			return nil, err
		}
		src, err := m.JSON()
		if err != nil {
			return nil, err
		}
		files[name] = src
	}
	if opts.TSDir != "" {
		tsDir, _, err := opts.resolveClientDir("TSDir", opts.TSDir)
		if err != nil {
			return nil, err
		}
		// The runtime first, for the reason the Go client is emitted before the
		// CLI: a reader of the file list should meet the thing being imported
		// before the thing importing it.
		if name := opts.tsRuntimeFile(); name != "-" {
			files[filepath.Join(tsDir, name)] = renderTSRuntime()
		}
		if name := opts.tsClientFile(); name != "-" {
			src, err := renderTSClient(opts)
			if err != nil {
				return nil, err
			}
			files[filepath.Join(tsDir, name)] = src
		}
		if name := opts.tsQueriesFile(); name != "-" {
			src, err := renderTSQueries(opts)
			if err != nil {
				return nil, err
			}
			// A schema that exposes nothing has no queries to emit, which is
			// not an error and should not leave an empty file behind.
			if src != nil {
				files[filepath.Join(tsDir, name)] = src
			}
		}
	}
	if opts.DartDir != "" {
		dartDir, _, err := opts.resolveClientDir("DartDir", opts.DartDir)
		if err != nil {
			return nil, err
		}
		if name := opts.dartRuntimeFile(); name != "-" {
			files[filepath.Join(dartDir, name)] = renderDartRuntime()
		}
		if name := opts.dartFile(); name != "-" {
			src, err := renderDartClient(opts)
			if err != nil {
				return nil, err
			}
			files[filepath.Join(dartDir, name)] = src
		}
	}
	// The client first, because the CLI imports it and a reader of the file
	// list should see the thing being imported before the thing importing it.
	if dir := opts.clientDir(); dir != "" {
		if name := opts.clientFile(); name != "-" {
			src, err := renderGoClient(opts)
			if err != nil {
				return nil, err
			}
			if src != nil {
				files[filepath.Join(dir, name)] = src
			}
		}
	}
	if opts.SkillDir != "" {
		src, err := renderSkill(opts)
		if err != nil {
			return nil, err
		}
		files[filepath.Join(opts.SkillDir, opts.skillName(), "SKILL.md")] = src
	}
	if opts.CLIDir != "" {
		if name := opts.cliFile(); name != "-" {
			src, err := renderGoCLI(opts)
			if err != nil {
				return nil, err
			}
			// A schema that exposes nothing has no commands to offer, which is
			// not an error and should not leave behind a file that imports
			// cobra for the sake of an empty tree.
			if src != nil {
				files[filepath.Join(opts.CLIDir, name)] = src
			}
		}
	}
	if opts.WiringMigrations.Type != "" || opts.WiringOperations.Type != "" {
		src, err := renderWiring(opts)
		if err != nil {
			return nil, err
		}
		// nil means there was nothing to contribute — see renderWiring — and
		// should not leave an empty file behind, the same as RestFile and CLIFile.
		if src != nil {
			files[filepath.Join(opts.WiringDir, opts.wiringFile())] = src
		}
	}
	return files, nil
}

// Must panics if generation failed, for use in a generator main where there is
// nothing useful to do with the error.
func Must(files []string, err error) []string {
	if err != nil {
		panic(err)
	}
	for _, f := range files {
		fmt.Fprintln(os.Stderr, "generated", f)
	}
	return files
}

// header is emitted at the top of every generated Go file. The exact first line
// is what `go test`, build tooling and code review conventions look for to
// recognise generated code.
func header(pkg string, imports []string) *bytes.Buffer {
	var b bytes.Buffer
	fmt.Fprintln(&b, "// Code generated by github.com/mind-vm/sqlb. DO NOT EDIT.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "package %s\n", pkg)
	if len(imports) > 0 {
		fmt.Fprintln(&b)
		if len(imports) == 1 {
			fmt.Fprintf(&b, "import %q\n", imports[0])
		} else {
			// Standard library first, then everything else, separated by a
			// blank line. gofmt sorts within a group but will not split one,
			// so the grouping has to be written here or the output looks
			// unlike hand-written Go.
			fmt.Fprintln(&b, "import (")
			var external []string
			for _, imp := range imports {
				if strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
					external = append(external, imp)
					continue
				}
				fmt.Fprintf(&b, "\t%q\n", imp)
			}
			if len(external) > 0 && len(external) < len(imports) {
				fmt.Fprintln(&b)
			}
			for _, imp := range external {
				fmt.Fprintf(&b, "\t%q\n", imp)
			}
			fmt.Fprintln(&b, ")")
		}
	}
	return &b
}

// gofmt formats generated source. A generator bug that produces invalid Go
// fails here, naming the file, rather than at the consumer's next build.
func gofmt(name string, src []byte) ([]byte, error) {
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("codegen: generated %s is not valid Go: %w\n%s", name, err, numbered(src))
	}
	return out, nil
}

// numbered renders source with line numbers, so the parse error above points
// at something a reader can find.
func numbered(src []byte) string {
	var b strings.Builder
	for i, line := range strings.Split(string(src), "\n") {
		fmt.Fprintf(&b, "%4d | %s\n", i+1, line)
	}
	return b.String()
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// replaceFile writes data to path by way of a temporary file in the same
// directory and a rename, so a reader never observes the file half-written.
//
// os.WriteFile truncates and then fills, which leaves a window in which the
// file on disk is a prefix of valid Go — or empty. Nothing in a build notices,
// because a build reads the tree once and after generate has returned. A
// language server does not: gopls re-reads on the filesystem event, and a read
// that lands in that window parses as a syntax error it then reports against
// code the author did not write. Rename is atomic on every filesystem sqlb
// targets, so the observable states are the old content and the new one.
//
// The temporary file is created alongside the target rather than in TMPDIR
// because rename across filesystems fails, and /tmp is a different filesystem
// often enough to matter. Its name begins with a dot for the same reason the
// driver's scratch directory does: the go tool skips dot-prefixed files, so
// one left behind by a kill between create and rename is invisible to a build
// rather than a second `package` clause in the directory.
func replaceFile(path string, data []byte) error {
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename has succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; generated files are readable.
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}
