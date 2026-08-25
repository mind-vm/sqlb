package migrate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
)

// dbmateFormat is a format sqlb does not ship, written here to check the claim
// that supporting a new runner is a small, local piece of work rather than a
// fork. If this ever stops being short, the interface is wrong.
type dbmateFormat struct{}

func (dbmateFormat) Name() string { return "dbmate" }

func (dbmateFormat) Render(m migrate.Migration, opts migrate.Options) (map[string]string, error) {
	var b strings.Builder
	b.WriteString("-- migrate:up\n")
	for _, c := range m.Changes {
		b.WriteString(strings.TrimSpace(c.Up) + "\n")
	}
	b.WriteString("\n-- migrate:down\n")
	for i := len(m.Changes) - 1; i >= 0; i-- {
		if d := strings.TrimSpace(m.Changes[i].Down); d != "" {
			b.WriteString(d + "\n")
		}
	}
	return map[string]string{fmt.Sprintf("%s_%s.sql", m.Version, m.Name): b.String()}, nil
}

func TestAThirdPartyFormatIsCheapToAdd(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000", Name: "add_view_count",
		Changes: []migrate.Change{addColumn(), addIndex()},
	}, migrate.Options{Format: dbmateFormat{}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The custom format gets the concurrent-index split for free: that logic
	// lives in Render, not in the format, because it is a property of how
	// runners handle transactions rather than of any one runner's syntax.
	if len(files) != 2 {
		t.Fatalf("want the index split out even for a custom format, got %v", keys(files))
	}
	for name, body := range files {
		if !strings.Contains(body, "-- migrate:up") {
			t.Errorf("%s did not use the custom format:\n%s", name, body)
		}
	}
}
