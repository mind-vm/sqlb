package codegen

// The fx wiring: the value-group providers a schema-owning module contributes
// to its host's uber-go/fx graph — the migration history and the resource
// mount — emitted as one `fx.Option` value the hand-written module composes
// itself (ADR-0059).
//
// # Why this imports nothing sqlb does not already import
//
// `sqlbfx`, an earlier attempt at the same problem, was a runtime library:
// importing it pulled chi, goose and huma into a consumer's dependency graph
// to duplicate contracts the host already had, and it was rejected for
// exactly that. This emitter's answer is to generate source that references
// the *host's own* types by a fully-qualified string — WiringSet.Type — so the
// generated file imports the host's package and `go.uber.org/fx`, both of
// which the consuming module already depends on, and sqlb itself gains no new
// dependency (`mise run deps-check` is the guard). A wrong Type is a compile
// error in the emitted file, not a runtime surprise.
//
// # Two contributions, not a generalised one
//
// This is deliberately not a mechanism for an arbitrary value group: it knows
// exactly two shapes, because those are the two things a schema declaration
// determines on its own. WiringMigrations contributes the migration history —
// gated on EmbedDir naming a directory of .sql files to embed — and
// WiringOperations contributes the resource mount, gated on the schema
// exposing at least one REST resource. Nothing is emitted for hooks: a
// `Scoped` column already refuses to mount without the hook that confines it,
// so the absence is a boot failure rather than sqlb generating unreviewed
// authorization policy.
import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// WiringSet configures one contributed fx value-group provider.
//
// The same struct shape serves both Options.WiringMigrations and
// Options.WiringOperations; which one a value configures is decided by which
// field it is assigned to, not by anything in the struct itself.
type WiringSet struct {
	// Type is the fully-qualified Go type this contribution's provider
	// returns — "import/path.TypeName", matching the host's own value-group
	// element by name: "github.com/you/app/fxkit.MigrationSet". sqlb imports
	// nothing of the host's beyond this string, so a name that does not exist
	// is a compile error in the generated file rather than a provider that
	// silently joins no group.
	//
	// The package name is assumed to be the last path segment — the ordinary
	// case, and the one every import in this codebase follows. A host package
	// whose declared name differs from its directory is not supported; name
	// the directory to match, or wire this contribution by hand.
	//
	// Empty means this contribution is not emitted at all.
	Type string

	// Group is the fx value-group tag this contribution's provider joins —
	// the same string the host passes to `group:"..."` where it declares the
	// group, such as "fxkit.migrations" or "http-operations". Which group a
	// resource joins is an access-surface decision a schema cannot make on
	// its own, so there is no default.
	Group string

	// Name is written into the contributed value's Module field: the
	// contributor name the host's boot log and its errors report, and — for
	// WiringMigrations — the prefix of the migration-tracking table.
	//
	// Empty defaults to the registry's module name
	// (schema.Registry.Module, ADR-0015). A schema.NewRegistry() registry has
	// none, on purpose — a table like "notifications" should not be renamed
	// just because a module wrapping it needs an fx name — so Name must be
	// set explicitly there; generation refuses rather than emitting an
	// unnamed contributor.
	Name string

	// EmbedDir is the directory of .sql migration files to embed, relative to
	// WiringDir — ordinarily the same path Project.MigrationsDir names,
	// resolved against WiringDir rather than the module root. A `go:embed`
	// directive cannot reach outside the package it is written in, so
	// MigrationsDir has to resolve to a subdirectory of WiringDir for the
	// generated file to compile; get that wrong and the failure is `go
	// build`, not a boot-time surprise.
	//
	// Only meaningful on WiringMigrations: it is what marks that contribution
	// as the migrations one rather than the resource mount, and it must be
	// set whenever WiringMigrations.Type is. Set on WiringOperations, it is
	// refused.
	EmbedDir string
}

func (o Options) wiringPackage() string { return orDefault(o.WiringPackage, o.Package) }
func (o Options) wiringFile() string    { return orDefault(o.WiringFile, "wiring_gen.go") }

// wiringValidate checks the two WiringSets structurally — everything that can
// be caught before a registry is walked. Called from Options.validate.
func (o Options) wiringValidate() error {
	if o.WiringOperations.EmbedDir != "" {
		return fmt.Errorf(
			"codegen: Options.WiringOperations.EmbedDir is %q, but EmbedDir only applies to "+
				"WiringMigrations — it is what a go:embed directive names a directory for, and the "+
				"resource mount embeds nothing; leave it empty here", o.WiringOperations.EmbedDir)
	}
	if err := validateWiringSet(o.WiringMigrations, "WiringMigrations", o.Registry); err != nil {
		return err
	}
	if err := validateWiringSet(o.WiringOperations, "WiringOperations", o.Registry); err != nil {
		return err
	}
	if (o.WiringMigrations.Type != "" || o.WiringOperations.Type != "") && !isGoIdent(o.wiringPackage()) {
		return fmt.Errorf(
			"codegen: the generated fx wiring lands in package %q, which is not a Go identifier; "+
				"set Options.WiringPackage", o.wiringPackage())
	}
	return nil
}

func validateWiringSet(ws WiringSet, field string, reg *schema.Registry) error {
	if ws.Type == "" {
		return nil
	}
	if _, _, _, err := parseQualifiedType(ws.Type); err != nil {
		return fmt.Errorf("codegen: Options.%s.Type: %w", field, err)
	}
	if ws.Group == "" {
		return fmt.Errorf(
			"codegen: Options.%s.Type is set but Group is empty; set it to the fx value-group tag "+
				"this contribution joins", field)
	}
	if field == "WiringMigrations" && ws.EmbedDir == "" {
		return fmt.Errorf(
			"codegen: Options.WiringMigrations.Type is set but EmbedDir is empty; EmbedDir names the " +
				"directory of .sql files to embed, relative to WiringDir")
	}
	if ws.Name == "" && (reg == nil || reg.Module() == "") {
		suffix := ""
		if field == "WiringMigrations" {
			suffix = " and the migration-tracking table's prefix"
		}
		return fmt.Errorf(
			"codegen: Options.%s.Name is empty and the registry has no module name "+
				"(schema.NewRegistry() is unnamed on purpose); set Name explicitly — it becomes the "+
				"contributor name in the host's boot log%s", field, suffix)
	}
	return nil
}

// wiringContribution is a WiringSet resolved against a registry: the parsed
// type, and Name defaulted.
type wiringContribution struct {
	importPath, pkgName, typeName string
	group, name, embedDir         string
}

func planWiring(ws WiringSet, reg *schema.Registry) (wiringContribution, error) {
	importPath, pkgName, typeName, err := parseQualifiedType(ws.Type)
	if err != nil {
		return wiringContribution{}, err
	}
	name := ws.Name
	if name == "" {
		name = reg.Module()
	}
	return wiringContribution{
		importPath: importPath, pkgName: pkgName, typeName: typeName,
		group: ws.Group, name: name, embedDir: ws.EmbedDir,
	}, nil
}

// parseQualifiedType splits "import/path.TypeName" into the import path, the
// package name and the type name, assuming — as every import in this
// repository does — that the package's declared name is its directory's last
// segment.
func parseQualifiedType(s string) (importPath, pkgName, typeName string, err error) {
	fail := fmt.Errorf(
		"codegen: %q is not a fully-qualified type (want \"import/path.TypeName\", package name "+
			"assumed to be the last path segment)", s)

	slash := strings.LastIndex(s, "/")
	last := s
	if slash >= 0 {
		last = s[slash+1:]
	}
	dot := strings.LastIndex(last, ".")
	if dot < 0 {
		return "", "", "", fail
	}
	pkgName, typeName = last[:dot], last[dot+1:]
	if pkgName == "" || typeName == "" || !isGoIdent(pkgName) || !isGoIdent(typeName) {
		return "", "", "", fail
	}
	if slash < 0 {
		importPath = pkgName
	} else {
		importPath = s[:slash+1] + pkgName
	}
	return importPath, pkgName, typeName, nil
}

// renderWiring emits the generated fx wiring, or nil when there is nothing to
// contribute — no WiringSet is configured, or the ones that are have nothing
// to point at (an operations set with no exposed resource, the same way
// renderREST itself emits nothing then).
func renderWiring(opts Options) ([]byte, error) {
	hasMigrations := opts.WiringMigrations.Type != ""

	var exposed []*schema.TableDef
	for _, t := range ownTables(opts) {
		if t.Rest() != nil {
			exposed = append(exposed, t)
		}
	}
	hasOperations := opts.WiringOperations.Type != "" && len(exposed) > 0 && opts.restFile() != "-"

	if !hasMigrations && !hasOperations {
		return nil, nil
	}

	if hasOperations {
		acts := actionsOf(exposed)
		qs := queriesOf(exposed)
		if len(acts) > 0 || len(qs) > 0 {
			return nil, fmt.Errorf(
				"codegen: this schema declares actions or queries, so the generated Register needs " +
					"hand-written Actions/Queries funcs the wiring emitter cannot supply; wire the " +
					"resource mount by hand instead of setting Options.WiringOperations")
		}
	}

	var mig, op wiringContribution
	var err error
	if hasMigrations {
		if mig, err = planWiring(opts.WiringMigrations, opts.Registry); err != nil {
			return nil, err
		}
	}
	if hasOperations {
		if op, err = planWiring(opts.WiringOperations, opts.Registry); err != nil {
			return nil, err
		}
	}

	if hasMigrations && hasOperations && mig.importPath == op.importPath && mig.pkgName != op.pkgName {
		return nil, fmt.Errorf(
			"codegen: WiringMigrations.Type and WiringOperations.Type both name the import %q but "+
				"disagree on the package name (%q vs %q)", mig.importPath, mig.pkgName, op.pkgName)
	}

	imports := map[string]bool{"go.uber.org/fx": true}
	if hasMigrations {
		imports["embed"] = true
		imports[mig.importPath] = true
	}
	if hasOperations {
		imports["github.com/danielgtaylor/huma/v2"] = true
		imports["github.com/mind-vm/sqlb"] = true
		imports[op.importPath] = true
	}

	b := header(opts.wiringPackage(), sortedSet(imports))

	fmt.Fprintln(b, "// FxModule is this schema's generated fx wiring: the migration history and")
	fmt.Fprintln(b, "// the resource mount, as one fx.Option a hand-written module composes")
	fmt.Fprintln(b, "// alongside whatever it provides itself. It never carries a hand edit, so")
	fmt.Fprintln(b, "// there is nothing in it to lose when the schema changes and this file")
	fmt.Fprintln(b, "// regenerates.")

	if hasMigrations {
		fmt.Fprintln(b)
		fmt.Fprintf(b, "//go:embed %s\n", path.Join(mig.embedDir, "*.sql"))
		fmt.Fprintln(b, "var wiringMigrationsFS embed.FS")
	}

	fmt.Fprintln(b)
	fmt.Fprintln(b, "var FxModule = fx.Options(")
	if hasMigrations {
		renderWiringProvider(b, mig.pkgName, mig.typeName, mig.group, func() {
			fmt.Fprintf(b, "\t\t\treturn %s.%s{\n", mig.pkgName, mig.typeName)
			fmt.Fprintf(b, "\t\t\t\tModule: %q,\n", mig.name)
			fmt.Fprintln(b, "\t\t\t\tFS:     wiringMigrationsFS,")
			fmt.Fprintf(b, "\t\t\t\tDir:    %q,\n", mig.embedDir)
			fmt.Fprintln(b, "\t\t\t}")
		}, "")
	}
	if hasOperations {
		renderWiringProvider(b, op.pkgName, op.typeName, op.group, func() {
			fmt.Fprintf(b, "\t\t\treturn %s.%s{\n", op.pkgName, op.typeName)
			fmt.Fprintf(b, "\t\t\t\tModule:   %q,\n", op.name)
			fmt.Fprintln(b, "\t\t\t\tRegister: func(api huma.API) error { return Register(api, db) },")
			fmt.Fprintln(b, "\t\t\t}")
		}, "db *sqlb.DB")
	}
	fmt.Fprintln(b, ")")

	return gofmt(opts.wiringFile(), b.Bytes())
}

// renderWiringProvider writes one fx.Provide(fx.Annotate(...)) block. body
// writes the constructor's return statement and closing brace; params is the
// constructor's parameter list, empty for one that takes nothing.
func renderWiringProvider(b *bytes.Buffer, pkgName, typeName, group string, body func(), params string) {
	fmt.Fprintln(b, "\tfx.Provide(fx.Annotate(")
	fmt.Fprintf(b, "\t\tfunc(%s) %s.%s {\n", params, pkgName, typeName)
	body()
	fmt.Fprintln(b, "\t\t},")
	fmt.Fprintf(b, "\t\tfx.ResultTags(`group:%q`),\n", group)
	fmt.Fprintln(b, "\t)),")
}
