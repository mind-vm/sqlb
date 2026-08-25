package codegen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// postsReg builds a registry with one exposed posts table. statusFilterable
// toggles whether status is a filter, so a test can drop a capability between
// two baselines — an un-expose that emits no DDL.
func postsReg(statusFilterable bool) *schema.Registry {
	r := schema.NewRegistry()
	status := schema.Enum("status", "draft", "published").Default(schema.Value("draft")).Sortable()
	if statusFilterable {
		status = status.Filterable()
	}
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Searchable().Sortable(),
		status,
	).Expose(schema.REST{Path: "/posts", Ops: schema.OpRead | schema.OpList})
	return r
}

func impactProject(reg *schema.Registry, contractFile string) codegen.Project {
	return codegen.Project{
		Options:      codegen.Options{Dir: ".", Package: "blog", Registry: reg},
		ContractFile: contractFile,
	}
}

func TestImpactNoBaselineAsksToRecordOne(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restcontract.json")
	code, out := run(t, impactProject(postsReg(true), file), "impact")
	if code != 2 {
		t.Fatalf("want exit 2 with no baseline, got %d (%s)", code, out)
	}
	if !strings.Contains(out, "impact -write") {
		t.Errorf("should point at -write to record a baseline, got: %s", out)
	}
}

func TestImpactWriteRecordsAReadableBaseline(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restcontract.json")
	code, out := run(t, impactProject(postsReg(true), file), "impact", "-write")
	if code != 0 {
		t.Fatalf("write should succeed, got %d (%s)", code, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("baseline not written: %v", err)
	}
	var snap restcompat.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("baseline is not valid JSON: %v", err)
	}
	if snap.Version != restcompat.SnapshotVersion {
		t.Errorf("baseline version = %d, want %d", snap.Version, restcompat.SnapshotVersion)
	}
	if len(snap.Resources) != 1 || snap.Resources[0].Path != "/posts" {
		t.Errorf("baseline should hold one /posts resource, got %+v", snap.Resources)
	}
}

func TestImpactUnchangedSchemaIsClean(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restcontract.json")
	run(t, impactProject(postsReg(true), file), "impact", "-write")

	code, out := run(t, impactProject(postsReg(true), file), "impact")
	if code != 0 {
		t.Fatalf("unchanged schema should exit 0, got %d (%s)", code, out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("should say the contract is unchanged, got: %s", out)
	}
	for _, noise := range []string{"removed", "renamed", "breaking"} {
		if strings.Contains(out, noise) {
			t.Errorf("unchanged schema should report no %q line, got: %s", noise, out)
		}
	}
}

// The gate: a breaking change is stated by default (exit 0) and fails only under
// -error. This is ADR-0039's "state by default, gate on demand".
func TestImpactBreakingChangeStatesByDefaultGatesOnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restcontract.json")
	run(t, impactProject(postsReg(true), file), "impact", "-write")

	// Drop the status filter — a capability un-expose, no DDL.
	changed := impactProject(postsReg(false), file)

	code, out := run(t, changed, "impact")
	if code != 0 {
		t.Fatalf("breaking change should still exit 0 without -error, got %d (%s)", code, out)
	}
	if !strings.Contains(out, "filter") || !strings.Contains(out, "status") {
		t.Errorf("report should name the dropped status filter, got: %s", out)
	}
	if !strings.Contains(out, "1 breaking") {
		t.Errorf("summary should count the breaking change, got: %s", out)
	}

	code, _ = run(t, changed, "impact", "-error")
	if code != 1 {
		t.Fatalf("breaking change under -error should exit 1, got %d", code)
	}
}

func TestImpactRejectsWriteAndErrorTogether(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restcontract.json")
	code, _ := run(t, impactProject(postsReg(true), file), "impact", "-write", "-error")
	if code != 2 {
		t.Fatalf("contradictory flags should be a usage error (2), got %d", code)
	}
}
