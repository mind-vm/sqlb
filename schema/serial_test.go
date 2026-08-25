package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The construct that had no declaration at all, and so blocked every table with
// an auto-incrementing integer key — and, because a drift gate is per registry,
// every module holding one (issue #132).
func TestSerialAndIdentityDeclare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field *schema.Field
		want  schema.FieldDesc
	}{
		{"smallserial", schema.SmallSerial("id"), schema.FieldDesc{Type: schema.TypeSmallInt, Auto: schema.AutoSerial}},
		{"serial", schema.Serial("id"), schema.FieldDesc{Type: schema.TypeInt, Auto: schema.AutoSerial}},
		{"bigserial", schema.BigSerial("id"), schema.FieldDesc{Type: schema.TypeBigInt, Auto: schema.AutoSerial}},
		{"serial modifier", schema.BigInt("id").Serial(), schema.FieldDesc{Type: schema.TypeBigInt, Auto: schema.AutoSerial}},
		{"identity", schema.BigInt("id").Identity(), schema.FieldDesc{Type: schema.TypeBigInt, Auto: schema.AutoIdentity}},
		{"identity always", schema.Int("id").IdentityAlways(),
			schema.FieldDesc{Type: schema.TypeInt, Auto: schema.AutoIdentityAlways, ReadOnly: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.field.Desc()
			if d.Type != tc.want.Type {
				t.Errorf("Type = %q, want %q", d.Type, tc.want.Type)
			}
			if d.Auto != tc.want.Auto {
				t.Errorf("Auto = %q, want %q", d.Auto, tc.want.Auto)
			}
			if d.ReadOnly != tc.want.ReadOnly {
				t.Errorf("ReadOnly = %v, want %v", d.ReadOnly, tc.want.ReadOnly)
			}
			// The whole reason Auto is not a Type: the Go type, the filter
			// grammar and the sort machinery are the plain integer's.
			if got, want := d.Type.GoType(), tc.want.Type.GoType(); got != want {
				t.Errorf("GoType = %q, want %q", got, want)
			}
			// And the write path is told what it needs: the database supplies
			// this column, so an INSERT that does not name it is not missing a
			// value. Nothing downstream of the DDL has to learn what a sequence
			// is for that to be true.
			if !d.DatabaseSupplied() {
				t.Error("an auto column is not reported as database-supplied, so an insert would write a zero over it")
			}
			if !strings.Contains(d.Capabilities(), "default") {
				t.Errorf("the struct tag does not carry `default`, so the runtime writes the Go zero: %q", d.Capabilities())
			}
		})
	}
}

// Each of these is a combination Postgres has no reading for, refused where the
// declaration is rather than as rejected DDL halfway through a migration.
func TestAutoRefusesWhatPostgresWould(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*schema.Registry)
		want  string
	}{
		{"over a uuid", func(r *schema.Registry) {
			r.Table("t", schema.UUID("id").PrimaryKey().Identity())
		}, "must be smallint, int or bigint"},
		{"over text", func(r *schema.Registry) {
			r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.Text("code").Serial())
		}, "must be smallint, int or bigint"},
		{"an array", func(r *schema.Registry) {
			r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("ns").Serial().Array())
		}, "cannot be an Array"},
		{"nullable", func(r *schema.Registry) {
			r.Table("t", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("n").Identity().Nullable())
		}, "cannot be Nullable"},
		{"beside a default", func(r *schema.Registry) {
			r.Table("t", schema.BigSerial("id").PrimaryKey().Default(schema.Value(1)))
		}, "cannot also have a Default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := schema.NewRegistry()
			tc.build(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected validation to fail with %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The ordinary declarations validate, which is the half a list of refusals does
// not cover: a check written slightly too wide would fail these instead.
func TestAutoColumnsValidate(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("audit_log",
		schema.BigSerial("id").PrimaryKey(),
		schema.Text("action"),
	)
	r.Table("coprocess_steps",
		schema.BigInt("seq").Identity().PrimaryKey(),
		schema.UUID("session_id"),
	).Index("session_id", "seq")
	r.Table("tickets", schema.Int("n").IdentityAlways().PrimaryKey())
	if err := r.Validate(); err != nil {
		t.Fatalf("a schema of ordinary auto-incrementing keys does not validate: %v", err)
	}
}
