// Package roomsschema declares the schema for example/rooms: a room-booking
// service whose one hard invariant is that two confirmed bookings on the same
// room may never overlap in time.
//
// That invariant is not a unique key and not a CHECK — it is a relationship
// between every pair of rows, which is exactly what an EXCLUDE constraint is
// for. schema.Exclusion and TableDef.AddExclude are a real, tested feature
// (issue #121), not something this package works around; see the doc comment
// on Booking's AddExclude call below and ../README.md for what it settles.
package roomsschema

import "github.com/mind-vm/sqlb/schema"

// Room is the thing being booked.
var Room = schema.Table("rooms",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("name").Filterable(),
)

// Booking reserves a Room for a time range. starts_at and ends_at are plain
// timestamptz columns rather than a native Postgres range type — sqlb has no
// range column, and the exclusion element list builds the range in SQL
// (tstzrange(starts_at, ends_at)) from the two columns, so a native range type
// is not needed to get a native exclusion constraint.
var Booking = schema.Table("bookings",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Ref("room", Room).Expandable(),
	schema.Timestamp("starts_at").Filterable(),
	schema.Timestamp("ends_at").Filterable(),
	// pending is the state a booking is created in and never overlaps
	// anything by itself; only a confirmed booking claims the room, which is
	// what the exclusion's WHERE clause narrows to.
	schema.Enum("status", "pending", "confirmed", "cancelled").
		Default(schema.Value("pending")).Filterable(),
)

func init() {
	// Postgres stores the parse tree and renders it back in its own spelling
	// (schema.Exclusion's doc comment explains why Elements and Where are
	// hand-written SQL rather than a structured form). The double-quoted
	// column name matches what pg_get_constraintdef returns, which is what the
	// diff's normalisation pass compares against.
	Booking.AddExclude(schema.Exclusion{
		Name:     "bookings_no_double_booking",
		Using:    "gist",
		Elements: `"room_id" WITH =, tstzrange(starts_at, ends_at) WITH &&`,
		Where:    `status = 'confirmed'`,
	})
}
