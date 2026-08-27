package shadow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A replay that stops because the history names a schema the scratch database
// does not have says what to do about it. The case is a foreign key into a
// platform's tables — auth.users on Supabase — where the database the
// migration was written against has the schema and a fresh scratch one never
// does.
func TestAMissingSchemaSaysWhereToCreateIt(t *testing.T) {
	f := file{Name: "003_profiles.sql", Statements: []string{"a", "b", "c"}}
	err := statementError(f, 1, `ALTER TABLE "profiles" ADD CONSTRAINT "x" FOREIGN KEY ("user_id") REFERENCES "auth"."users" ("id");`,
		&pgconn.PgError{Code: "3F000", Message: `schema "auth" does not exist`})

	for _, want := range []string{
		"003_profiles.sql",             // which file
		`schema "auth" does not exist`, // what Postgres said
		"shadow database",              // and where the fix goes
		"docs/supabase.md",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q:\n%v", want, err)
		}
	}
}

// Every other failure is left exactly as it was: a hint that appears on
// unrelated errors is one a reader learns to skip past.
func TestAnUnrelatedFailureGetsNoHint(t *testing.T) {
	f := file{Name: "004_index.sql", Statements: []string{"a"}}
	for _, err := range []error{
		&pgconn.PgError{Code: "42P07", Message: `relation "profiles" already exists`},
		errors.New("connection refused"),
		fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42703", Message: `column "x" does not exist`}),
	} {
		if got := statementError(f, 0, "CREATE TABLE profiles ()", err).Error(); strings.Contains(got, "docs/supabase.md") {
			t.Errorf("an unrelated failure carried the missing-schema hint:\n%s", got)
		}
	}
}

// The hint follows a wrapped error too, since a driver error rarely arrives
// bare.
func TestTheHintFollowsAWrappedError(t *testing.T) {
	f := file{Name: "003_profiles.sql", Statements: []string{"a"}}
	wrapped := fmt.Errorf("exec: %w", &pgconn.PgError{Code: "3F000", Message: `schema "auth" does not exist`})
	if got := statementError(f, 0, "ALTER TABLE …", wrapped).Error(); !strings.Contains(got, "docs/supabase.md") {
		t.Errorf("a wrapped missing-schema failure lost the hint:\n%s", got)
	}
}
