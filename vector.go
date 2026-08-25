package sqlb

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Vector is a pgvector embedding.
//
// It is a plain []float32, so a caller's embedder output goes in without a
// conversion and comes back out the same way:
//
//	chunk.Embedding = sqlb.Vector(embedder.Embed(ctx, text))
//
// Declare the column with schema.Vector(name, dim). The dimension belongs to
// the column rather than to this type: a vector(1536) column refuses a
// 768-component value, and the check is Postgres's.
//
// # Register the codec
//
// Values move in pgvector's binary format, which is worth about 2.7× the time
// and 21× the memory of the text form on a page of 1,536-component embeddings
// — the measurement [ADR-0040] was decided on. Binary needs the type's OID,
// which an extension type only has once it is installed, so it is registered
// per connection:
//
//	cfg, err := pgxpool.ParseConfig(dsn)
//	cfg.AfterConnect = sqlb.RegisterVectorType
//	pool, err := pgxpool.NewWithConfig(ctx, cfg)
//
// Without that registration a Vector still works and moves as text, which is
// correct and slower. [RegisterVectorType] says what to do about a database
// that does not have the extension at all.
//
// [ADR-0040]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#the-driver-is-a-dependency
type Vector []float32

// String renders the pgvector text form, `[1,2,3]`. It is what the type sends
// when no codec is registered, and what a %v in a log will show.
func (v Vector) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// vectorCodec encodes and decodes pgvector's `vector` type.
//
// The wire format is two int16s of header — the number of dimensions, and a
// word pgvector reserves and sends as zero — followed by one big-endian float32
// per component. It is written out here rather than taken from pgvector-go because
// the engine depends on pgx and nothing else (ADR-0040), and this is eighty
// lines against a dependency every consumer would inherit.
type vectorCodec struct{}

func (vectorCodec) FormatSupported(format int16) bool {
	return format == pgtype.TextFormatCode || format == pgtype.BinaryFormatCode
}

func (vectorCodec) PreferredFormat() int16 { return pgtype.BinaryFormatCode }

func (c vectorCodec) PlanEncode(_ *pgtype.Map, _ uint32, format int16, value any) pgtype.EncodePlan {
	if _, ok := value.(Vector); !ok {
		return nil
	}
	switch format {
	case pgtype.BinaryFormatCode:
		return encodeVectorBinary{}
	case pgtype.TextFormatCode:
		return encodeVectorText{}
	}
	return nil
}

func (c vectorCodec) PlanScan(_ *pgtype.Map, _ uint32, format int16, target any) pgtype.ScanPlan {
	if _, ok := target.(*Vector); !ok {
		return nil
	}
	switch format {
	case pgtype.BinaryFormatCode:
		return scanVectorBinary{}
	case pgtype.TextFormatCode:
		return scanVectorText{}
	}
	return nil
}

func (c vectorCodec) DecodeDatabaseSQLValue(m *pgtype.Map, oid uint32, format int16, src []byte) (driver.Value, error) {
	if src == nil {
		return nil, nil
	}
	var v Vector
	if err := c.decodeInto(format, src, &v); err != nil {
		return nil, err
	}
	return v.String(), nil
}

func (c vectorCodec) DecodeValue(m *pgtype.Map, oid uint32, format int16, src []byte) (any, error) {
	if src == nil {
		return nil, nil
	}
	var v Vector
	if err := c.decodeInto(format, src, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (vectorCodec) decodeInto(format int16, src []byte, dst *Vector) error {
	switch format {
	case pgtype.BinaryFormatCode:
		return decodeVectorBinary(src, dst)
	case pgtype.TextFormatCode:
		return decodeVectorText(src, dst)
	}
	return fmt.Errorf("sqlb: unknown wire format %d for a vector", format)
}

type encodeVectorBinary struct{}

func (encodeVectorBinary) Encode(value any, buf []byte) ([]byte, error) {
	v, ok := value.(Vector)
	if !ok {
		return nil, fmt.Errorf("sqlb: cannot encode %T as a vector", value)
	}
	if len(v) > math.MaxUint16 {
		return nil, fmt.Errorf("sqlb: vector has %d dimensions, more than the wire format can carry", len(v))
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(v)))
	// Reserved by pgvector and sent as zero. Writing the constant rather than
	// naming it: there is nothing to name until pgvector uses it for something.
	buf = binary.BigEndian.AppendUint16(buf, 0)
	for _, f := range v {
		buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return buf, nil
}

type encodeVectorText struct{}

func (encodeVectorText) Encode(value any, buf []byte) ([]byte, error) {
	v, ok := value.(Vector)
	if !ok {
		return nil, fmt.Errorf("sqlb: cannot encode %T as a vector", value)
	}
	return append(buf, v.String()...), nil
}

type scanVectorBinary struct{}

func (scanVectorBinary) Scan(src []byte, target any) error {
	dst, ok := target.(*Vector)
	if !ok {
		return fmt.Errorf("sqlb: cannot scan a vector into %T", target)
	}
	if src == nil {
		*dst = nil
		return nil
	}
	return decodeVectorBinary(src, dst)
}

type scanVectorText struct{}

func (scanVectorText) Scan(src []byte, target any) error {
	dst, ok := target.(*Vector)
	if !ok {
		return fmt.Errorf("sqlb: cannot scan a vector into %T", target)
	}
	if src == nil {
		*dst = nil
		return nil
	}
	return decodeVectorText(src, dst)
}

func decodeVectorBinary(src []byte, dst *Vector) error {
	if len(src) < 4 {
		return fmt.Errorf("sqlb: vector header is %d bytes, want at least 4", len(src))
	}
	dim := int(binary.BigEndian.Uint16(src[0:2]))
	if want := 4 + dim*4; len(src) != want {
		return fmt.Errorf("sqlb: vector says %d dimensions, which needs %d bytes, but %d arrived",
			dim, want, len(src))
	}
	out := make(Vector, dim)
	for i := range out {
		out[i] = math.Float32frombits(binary.BigEndian.Uint32(src[4+i*4:]))
	}
	*dst = out
	return nil
}

func decodeVectorText(src []byte, dst *Vector) error {
	s := strings.TrimSpace(string(src))
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return fmt.Errorf("sqlb: %q is not a vector literal", s)
	}
	s = s[1 : len(s)-1]
	if s == "" {
		*dst = Vector{}
		return nil
	}
	parts := strings.Split(s, ",")
	out := make(Vector, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return fmt.Errorf("sqlb: component %d of a vector is not a number: %w", i, err)
		}
		out[i] = float32(f)
	}
	*dst = out
	return nil
}

// RegisterVectorType teaches a connection pgvector's binary format. It is the
// shape pgxpool.Config.AfterConnect wants, so it goes straight on:
//
//	cfg.AfterConnect = sqlb.RegisterVectorType
//
// The registration is per connection because the OID is per database: `vector`
// is an extension type, so it has no fixed number and has to be looked up
// wherever the connection landed.
//
// A database without the extension installed is not an error. There is no
// vector type to register, so nothing is registered and the connection is
// returned as it is — which is what lets one AfterConnect serve a pool that
// reaches databases with and without it, and keeps this from being a startup
// failure for an application that declares no vector column.
func RegisterVectorType(ctx context.Context, conn *pgx.Conn) error {
	var oid uint32
	err := conn.QueryRow(ctx, `SELECT oid FROM pg_type WHERE typname = 'vector'`).Scan(&oid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("sqlb: looking up the vector type: %w", err)
	}
	conn.TypeMap().RegisterType(&pgtype.Type{Name: "vector", OID: oid, Codec: vectorCodec{}})
	return nil
}

// VectorPoolConfig is RegisterVectorType applied to a pool config, for the
// common case where that is the only AfterConnect a caller wants:
//
//	cfg, err := sqlb.VectorPoolConfig(dsn)
//	if err != nil {
//	    return err
//	}
//	pool, err := pgxpool.NewWithConfig(ctx, cfg)
//
// It replaces any AfterConnect already set rather than chaining onto it, which
// is why it takes a DSN rather than a config: a function that silently dropped
// a hook somebody else installed would be worse than one that cannot.
func VectorPoolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlb: %w", err)
	}
	cfg.AfterConnect = RegisterVectorType
	return cfg, nil
}

// The codec satisfies what pgx asks of one. Stated here rather than discovered
// at run time, where a missing method surfaces as a value moving as text and
// nobody noticing except the latency graph.
var _ pgtype.Codec = vectorCodec{}
