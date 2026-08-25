package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/schema"
)

// One declaration, five surfaces (#309).
//
// The claim of REST.CreateInput is the one ADR-0036 makes about a column name:
// a property declared once has to arrive in the Go body, the TypeScript client,
// the Dart client, the CLI and the ejected package, spelled the same way. So
// this fixture is generated once and every emitter is asked what it did with
// it, rather than each emitter being tested against its own idea of the shape.
func createInputFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Filterable().Sortable(),
		schema.Varchar("pin_hash", 255).Hidden(),
	).Expose(schema.REST{
		Path: "/children",
		Ops:  schema.OpCreate | schema.Reads,
		CreateInput: schema.Body(
			schema.Varchar("pin", 4).Comment("Four digits. Hashed on the way in; never stored as sent."),
			schema.Text("invite_code").Nullable(),
		),
	})
	return r
}

// The Go body carries the property, and Row() does not write it: a property is
// not a column, and the row has no field for it. What carries it instead is
// Input(), which is the whole of rest.CreateInput.
func TestCreateInputReachesTheGoBody(t *testing.T) {
	src := generateAll(t, createInputFixture())["rest_gen.go"]

	for _, want := range []string{
		"Pin        string  `json:\"pin\"`",
		"InviteCode *string `json:\"invite_code,omitempty\"`",
		"type CreateChildrenInput struct",
		"func (c ChildrenCreate) Input() any",
		"Pin:        c.Pin,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rest_gen.go is missing %q:\n%s", want, src)
		}
	}

	row := src[strings.Index(src, "func (c ChildrenCreate) Row()"):]
	row = row[:strings.Index(row, "\n}")]
	if strings.Contains(row, "Pin") {
		t.Errorf("Row() writes a property that is not a column:\n%s", row)
	}
}

// A schema that declares none emits exactly what it always did. This is what
// makes the feature additive: the interface is optional, and a body that does
// not implement it goes down the path it went down before.
func TestWithoutACreateInputNothingIsEmitted(t *testing.T) {
	src := generateAll(t, restFixture())["rest_gen.go"]
	for _, unwanted := range []string{"Input() any", "CreateInput"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("a schema declaring no create input emitted %q:\n%s", unwanted, src)
		}
	}
}

// The clients send it. A property the server requires and no client can spell
// is the drift a generated client exists to remove.
func TestCreateInputReachesTheClients(t *testing.T) {
	files := generateAll(t, createInputFixture())

	ts := files["client.gen.ts"]
	create := ts[strings.Index(ts, "export interface ChildrenCreate {"):]
	create = create[:strings.Index(create, "}")]
	for _, want := range []string{"pin: string;", "invite_code?: string | null;"} {
		if !strings.Contains(create, want) {
			t.Errorf("the TypeScript create body is missing %q:\n%s", want, create)
		}
	}

	dart := files["client.gen.dart"]
	body := dart[strings.Index(dart, "class ChildrenCreate {"):]
	body = body[:strings.Index(body, "\n}")]
	for _, want := range []string{"required this.pin", "final String pin;", "'pin': _wire(pin)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Dart create body is missing %q:\n%s", want, body)
		}
	}

	cli := files["cli_gen.go"]
	cmd := cli[strings.LastIndex(cli, "func newChildrenCreateCommand"):]
	for _, want := range []string{
		`flags.StringVar(&valPin, "pin"`,
		`_ = cmd.MarkFlagRequired("pin")`,
		`body["pin"] = valPin`,
		"--pin, --invite-code are not a column",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the CLI create command is missing %q:\n%s", want, cmd)
		}
	}
	// The nullable one is optional there too, as it is everywhere else.
	if strings.Contains(cmd, `MarkFlagRequired("invite-code")`) {
		t.Errorf("a nullable property was marked required:\n%s", cmd)
	}
}

// The exit carries the property, and says what it cannot do with it. An
// ejected package has no hooks, so the seam is a function field and Register
// refuses a nil one — the arrangement ADR-0030's Confine already uses.
func TestCreateInputReachesTheEjectedPackage(t *testing.T) {
	src := eject(t, createInputFixture())["handlers.go"]

	for _, want := range []string{
		"Derive func(*http.Request, map[string]any) (map[string]any, error)",
		"func decodeChildrenCreateInput(data []byte) (map[string]any, error)",
		"if h.Derive == nil {",
		"derived, err := h.Derive(r, inputs)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the ejected package is missing %q:\n%s", want, src)
		}
	}

	// The column decoder must not put the property in the INSERT, and must not
	// refuse it as unknown either. Both halves: it is named in the switch, and
	// nothing is assigned under it.
	decoder := src[strings.Index(src, "func decodeChildrenCreate(data []byte)"):]
	decoder = decoder[:strings.Index(decoder, "\n}\n")]
	if !strings.Contains(decoder, `case "pin":`) {
		t.Errorf("the column decoder would refuse the declared property as unknown:\n%s", decoder)
	}
	if strings.Contains(decoder, `out["pin"]`) {
		t.Errorf("the column decoder puts a property with no column into the INSERT:\n%s", decoder)
	}
}

// The document an agent reads. A property that is not a column is absent from
// the column table by construction, so this line is the only thing that says a
// create body assembled from the columns is incomplete.
func TestCreateInputReachesTheSkill(t *testing.T) {
	skill := skillOf(t, createInputFixture())
	for _, want := range []string{
		"`POST /children` carries more than the columns.",
		"`pin` (varchar, required)",
		"`invite_code` (text, optional)",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill is missing %q:\n%s", want, skill)
		}
	}
}

// The ejected package has to compile, and the substring assertions above cannot
// say whether it does: they name the mistake in advance, and the mistakes an
// emitter makes are the ones nobody predicted. TestGeneratedGoCompiles is this
// argument for the generated half; the exit had no equivalent, and this feature
// is the first thing to add a *statement* to an ejected handler rather than a
// declaration.
func TestEjectedCreateInputCompiles(t *testing.T) {
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH: %v", err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Inside the module and behind a leading dot, for the reason compile_test.go
	// gives: `go build ./...` skips it, and an explicit path still builds it.
	scratch, err := os.MkdirTemp(root, ".sqlb-gencheck-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })

	dir := filepath.Join(scratch, "ejected")
	if _, err := codegen.Eject(codegen.EjectOptions{
		Registry: createInputFixture(), Dir: dir, Package: "ejected",
	}); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	build := exec.Command(gotool, "build", "./"+filepath.Base(scratch)+"/ejected")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Errorf("the ejected package does not compile: %v\n%s", err, out)
	}
}
