package sqlb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// users builds n rows that all write the same columns, so that the bind count
// is a clean multiple of the width.
func users(n int) []*User {
	rows := make([]*User, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, &User{ID: "u", Email: "e@example.com", Name: "n", OrgID: "acme"})
	}
	return rows
}

// The guard is proven both ways here (ADR-0016): a batch over the ceiling is
// refused, and the largest batch the refusal names still compiles. Without the
// check the first case returns a nil error and 72000 parameters, and the
// statement is rejected by the server instead — which is the failure this
// replaces, not one it introduces.
func TestAnInsertTooWideForOneStatementIsRefused(t *testing.T) {
	_, _, err := sqlb.InsertRows(users(20000)...).SQL()
	if err == nil {
		t.Fatal("a 20000-row insert compiled; expected the bind budget to refuse it")
	}
	for _, want := range []string{"20000 rows", "users", "65535", "insert at most"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// The suggested batch size is the load-bearing half of the message: a ceiling
// that does not itself fit would send the caller round the loop twice.
func TestTheSuggestedBatchSizeFits(t *testing.T) {
	_, _, err := sqlb.InsertRows(users(20000)...).SQL()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var fits int
	if _, scanErr := fmt.Sscanf(err.Error()[strings.Index(err.Error(), "insert at most "):],
		"insert at most %d rows", &fits); scanErr != nil {
		t.Fatalf("could not read the suggested batch size from %q: %v", err, scanErr)
	}
	if fits <= 0 {
		t.Fatalf("suggested batch size %d is not usable", fits)
	}
	_, args, err := sqlb.InsertRows(users(fits)...).SQL()
	if err != nil {
		t.Fatalf("the suggested batch of %d rows did not compile: %v", fits, err)
	}
	if len(args) > 65535 {
		t.Fatalf("the suggested batch of %d rows binds %d values, over the limit", fits, len(args))
	}
	// One more row must not fit, or the ceiling is lower than it needs to be.
	if _, _, err := sqlb.InsertRows(users(fits + 1)...).SQL(); err == nil {
		t.Errorf("a batch of %d rows also compiled; the suggested ceiling is too low", fits+1)
	}
}

// A wide predicate reaches the same ceiling by a different route, and gets the
// generic message because no arithmetic about rows applies to it.
func TestAWidePredicateIsRefusedWithoutInsertAdvice(t *testing.T) {
	values := make([]any, 70000)
	for i := range values {
		values[i] = i
	}
	_, _, err := sqlb.Query[User]().Where(sqlb.F("age").OneOf(values...)).SQL()
	if err == nil {
		t.Fatal("a 70000-value predicate compiled; expected the bind budget to refuse it")
	}
	if !strings.Contains(err.Error(), "65535") {
		t.Errorf("error does not name the limit: %v", err)
	}
	if strings.Contains(err.Error(), "insert at most") {
		t.Errorf("a select was given advice about inserting: %v", err)
	}
}

// The ordinary statement must not pay for the check, and must not trip it.
func TestAnOrdinaryStatementIsUnaffected(t *testing.T) {
	if _, _, err := sqlb.InsertRows(users(100)...).SQL(); err != nil {
		t.Fatalf("a 100-row insert was refused: %v", err)
	}
}
