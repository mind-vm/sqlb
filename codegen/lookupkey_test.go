package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A session row is found by the hash of the token its holder presents, and the
// hash must never leave the process. Both columns here are Hidden; only one of
// them is a lookup key, which is what makes the pair worth generating from.
func lookupKeyFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("sessions",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("token_hash").Hidden().LookupKey(),
		schema.Text("recovery_secret").Hidden(),
		schema.Timestamp("expires_at").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})
	return r
}

// The typed column is generated for the lookup key and withheld from the other
// hidden column, so authenticating a request compiles and probing the recovery
// secret does not.
func TestLookupKeyKeepsItsTypedColumn(t *testing.T) {
	cols := generate(t, lookupKeyFixture())["columns_gen.go"]

	if !contains(cols, `TokenHash: sqlb.TextColumn[string]("token_hash")`) {
		t.Errorf("the lookup key has no typed column:\n%s", cols)
	}
	if strings.Contains(cols, "RecoverySecret:") {
		t.Errorf("an ordinary hidden column acquired a typed column:\n%s", cols)
	}
	// The generated comment says which of the two kinds each is, because that is
	// the fact a reader of the schema cannot otherwise recover from this file.
	for _, want := range []string{
		"Hidden columns are omitted: a predicate against one should not compile.",
		"Omitted here: recovery_secret.",
		"A hidden column declaring LookupKey is here",
	} {
		if !contains(cols, want) {
			t.Errorf("the facade comment is missing %q:\n%s", want, cols)
		}
	}
	// The way back is not repeated to a reader who has already met the word two
	// lines further down. The pointer exists for the table that has no lookup
	// key at all, which is where the word is unknown.
	if strings.Contains(cols, "Declaring LookupKey beside Hidden") {
		t.Errorf("the pointer is repeated to a table that already declares a lookup key:\n%s", cols)
	}
}

// A table whose hidden columns are all omitted is the case the pointer exists
// for: the facade is silent about a column the reader is looking for, and a
// closed door with no key beside it sends them to untyped sqlb.F instead (#256).
func TestOmittedHiddenColumnsAreNamedWithTheWayBack(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("api_tokens",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("token_hash").Hidden(),
		schema.Text("recovery_secret").Hidden(),
		schema.Timestamp("expires_at").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	cols := generate(t, r)["columns_gen.go"]

	for _, want := range []string{
		// Named, in declaration order: absent is also what a misspelling looks
		// like, so the comment says which columns are missing on purpose.
		"Omitted here: token_hash, recovery_secret.",
		"Declaring LookupKey beside Hidden returns one to this facade",
		"It stays off the wire either way.",
	} {
		if !contains(cols, want) {
			t.Errorf("the facade comment is missing %q:\n%s", want, cols)
		}
	}
}

// The list of names is wrapped. A vault-shaped table hides most of what it
// stores, and one comment line carrying a dozen column names is a line gofmt
// will not break and a reader will not read.
func TestOmittedHiddenColumnsWrap(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("credentials",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("password_hash").Hidden(),
		schema.Text("recovery_secret").Hidden(),
		schema.Text("totp_seed").Hidden(),
		schema.Text("backup_codes").Hidden(),
		schema.Text("session_token_hash").Hidden(),
		schema.Text("legacy_password_hash").Hidden(),
	).Expose(schema.REST{Ops: schema.OpRead})

	cols := generate(t, r)["columns_gen.go"]

	var listed []string
	for line := range strings.SplitSeq(cols, "\n") {
		switch {
		case strings.HasPrefix(line, "// Omitted here: "):
			listed = append(listed, line)
		case len(listed) > 0 && strings.HasPrefix(line, "// ") && strings.HasSuffix(line, "."):
			listed = append(listed, line)
		}
		if len(listed) > 0 && strings.HasSuffix(line, ".") {
			break
		}
	}
	if len(listed) < 2 {
		t.Fatalf("six hidden columns fitted on %d line(s), so nothing wrapped:\n%s", len(listed), cols)
	}
	for _, line := range listed {
		// The Go convention's 75 columns, plus the comment marker.
		if len(line) > 78 {
			t.Errorf("comment line is %d characters:\n%s", len(line), line)
		}
	}
	if !contains(cols, "legacy_password_hash.") {
		t.Errorf("the wrapped list lost its last name:\n%s", cols)
	}
}

// Nothing about the REST surface moves. A hidden column has no capability, so
// the filter grammar refuses it either way — a client that can probe a
// credential column by equality has an oracle.
func TestLookupKeyChangesNothingOnTheWire(t *testing.T) {
	files := generate(t, lookupKeyFixture())

	// The row type carries both, unserialised and with the same capabilities:
	// the declaration is about Go, so it adds no token the engine reads.
	models := files["models_gen.go"]
	for _, want := range []string{
		`TokenHash string ` + "`" + `db:"token_hash" json:"-" sqlb:"type:text,hidden"` + "`",
		`RecoverySecret string ` + "`" + `db:"recovery_secret" json:"-" sqlb:"type:text,hidden"` + "`",
	} {
		if !contains(models, want) {
			t.Errorf("models are missing %q:\n%s", want, models)
		}
	}

	// Nowhere else names it. A hidden column has no capability, so the filter
	// grammar refuses `?token_hash=eq.…` with a 400 naming what would have been
	// accepted, and that must stay true: a client that can probe a credential
	// column by equality has an oracle.
	for name, src := range files {
		if name == "columns_gen.go" || name == "models_gen.go" {
			continue
		}
		if strings.Contains(src, "token_hash") {
			t.Errorf("%s names the hidden lookup key:\n%s", name, src)
		}
	}
}

// LookupKey on a visible column is refused: the facade carries it anyway, so the
// word would be a claim with no effect, and the generated comment would be
// calling an ordinary column a secret.
func TestLookupKeyWithoutHiddenIsRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("sessions",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("token_hash").LookupKey(),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("expected LookupKey without Hidden to be refused")
	}
	if !strings.Contains(err.Error(), "LookupKey applies to a Hidden column") {
		t.Errorf("error = %q, want it to name the missing declaration", err)
	}
}
