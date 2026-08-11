package hcl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeColumnType_Canonicalizes covers the type canonicalizer: two
// spellings of one type must reduce to one string, so a whitespace edit or a
// reordered JSON hint list stops reading as drift and stops generating a no-op
// ALTER TABLE ... MODIFY COLUMN.
func TestNormalizeColumnType_Canonicalizes(t *testing.T) {
	cases := []struct {
		name     string
		in, want string
	}{
		{"scalar", "String", "String"},
		{"inner whitespace", "Map(String,   String)", "Map(String, String)"},
		{"parameter whitespace", "Decimal( 18 , 4 )", "Decimal(18, 4)"},
		{"nested whitespace", "LowCardinality( Nullable( String ) )", "LowCardinality(Nullable(String))"},
		{"enum spacing", "Enum8('a' = 1, 'b' = 2)", "Enum8('a'=1, 'b'=2)"},
		{"json hints sorted", "JSON(b String, a String)", "JSON(a String, b String)"},
		{"json already sorted", "JSON(a String, b String)", "JSON(a String, b String)"},
		{"json params first", "JSON(b String, max_dynamic_paths=16, a String)", "JSON(max_dynamic_paths=16, a String, b String)"},
		{"json param order matches clickhouse", "JSON(max_dynamic_paths=16, max_dynamic_types=8)", "JSON(max_dynamic_types=8, max_dynamic_paths=16)"},
		{"json skip sorted", "JSON(SKIP z, a String, SKIP c)", "JSON(a String, SKIP c, SKIP z)"},
		{"json nested in array", "Array(JSON(b String, a String))", "Array(JSON(a String, b String))"},
		{"json nested in map value", "Map(String, JSON(b String, a String))", "Map(String, JSON(a String, b String))"},
		{"json nested in a hint", "JSON(x JSON(d String, c String))", "JSON(x JSON(c String, d String))"},
		{"tuple element order is meaningful", "Tuple(b Int32, a String)", "Tuple(b Int32, a String)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizeColumnType(c.in)
			require.True(t, ok)
			assert.Equal(t, c.want, got)

			again, ok := normalizeColumnType(got)
			require.True(t, ok)
			assert.Equal(t, got, again, "normalization must be idempotent")
		})
	}
}

func TestNormalizeColumnType_UnparseableKeepsRaw(t *testing.T) {
	raw := "NotAType((("
	got, ok := normalizeColumnType(raw)
	assert.False(t, ok)
	assert.Equal(t, raw, got, "a type the parser cannot read is kept verbatim")
}

// TestNormalizeColumnType_KnownGapsDegradeSafely covers real ClickHouse types
// the SQL parser does not yet read. They must degrade to verbatim rather than to
// something lossy, which is the whole fallback contract. Asserted as "verbatim
// or content-preserving" so a parser release that adds support makes the test
// pass more strongly instead of failing.
func TestNormalizeColumnType_KnownGapsDegradeSafely(t *testing.T) {
	gaps := []string{
		"Dynamic(max_types=10)",
	}
	stripSpace := func(s string) string { return strings.ReplaceAll(s, " ", "") }
	for _, in := range gaps {
		got, ok := normalizeColumnType(in)
		if !ok {
			assert.Equal(t, in, got, "%q must be kept verbatim", in)
			continue
		}
		assert.Equal(t, stripSpace(in), stripSpace(got),
			"%q now parses; it must still only change layout", in)
	}
}

// TestNormalizeColumnType_ChangesLayoutOnly is the safety net under the whole
// canonicalizer. Normalization renders a third-party parser's AST back to text,
// so a parser release that accepts a type but drops part of it would silently
// change what hclexp generates — the one failure here that could alter a real
// table rather than just add noise. Every type below must survive with its
// content intact: identical to the input once whitespace is removed.
//
// Types whose canonical form deliberately reorders (JSON) are covered by
// TestNormalizeColumnType_Canonicalizes instead; this list is the
// layout-only set, and it is worth extending whenever a schema starts using a
// type shape not represented here.
func TestNormalizeColumnType_ChangesLayoutOnly(t *testing.T) {
	types := []string{
		"String",
		"UInt64",
		"Nullable(String)",
		"LowCardinality(Nullable(String))",
		"Array(Array(UInt8))",
		"Map(LowCardinality(String), Array(Nullable(Float64)))",
		"Tuple(a String, b Int32)",
		"Tuple(String, Int32)",
		"Nested(a String, b UInt8)",
		"Decimal(18, 4)",
		"Decimal32(4)",
		"Decimal256(38)",
		"FixedString(16)",
		"DateTime('Europe/London')",
		"DateTime64(3, 'UTC')",
		"Enum8('a'=1, 'b'=2)",
		"Enum16('x'=-1, 'y'=1000)",
		"AggregateFunction(sum, UInt64)",
		"AggregateFunction(quantiles(0.5, 0.9), UInt64)",
		"SimpleAggregateFunction(max, DateTime)",
		"SimpleAggregateFunction(maxMap, Tuple(Array(UInt8), Array(UInt8)))",
		"IPv4",
		"IPv6",
		"UUID",
		"Bool",
		"Int128",
		"Point",
		"Polygon",
		"Dynamic",
		"Variant(String, UInt64)",
		"JSON",
		"Array(Tuple(k String, v Nullable(Int64)))",
	}
	stripSpace := func(s string) string { return strings.ReplaceAll(s, " ", "") }
	for _, in := range types {
		t.Run(in, func(t *testing.T) {
			got, ok := normalizeColumnType(in)
			require.True(t, ok, "must canonicalize")
			assert.Equal(t, stripSpace(in), stripSpace(got),
				"canonicalization must only change layout, never content")
		})
	}
}

// TestNormalizeColumnType_KeepsSkipRegexpOrder pins the one JSON option
// ClickHouse does not sort. DataTypeObject::doGetName sorts typed paths and SKIP
// paths but writes path_regexps_to_skip in insertion order, so two orderings are
// two type names to ClickHouse. Sorting them here would canonicalize harder than
// the server does and hide a difference it can see.
func TestNormalizeColumnType_KeepsSkipRegexpOrder(t *testing.T) {
	got, ok := normalizeColumnType("JSON(SKIP REGEXP '^b', SKIP REGEXP '^a')")
	require.True(t, ok)
	assert.Contains(t, got, "SKIP REGEXP '^b'")
	assert.Less(t,
		strings.Index(got, "SKIP REGEXP '^b'"),
		strings.Index(got, "SKIP REGEXP '^a'"),
		"SKIP REGEXP order is preserved, not sorted")
}

// TestNormalizeColumnType_KeepsSmuggledModifiers guards the one way this could
// lose information: a type attribute that also carries a column modifier parses
// cleanly, but rendering the type node alone would silently drop the modifier
// from generated DDL. Such a value is kept verbatim.
func TestNormalizeColumnType_KeepsSmuggledModifiers(t *testing.T) {
	for _, raw := range []string{
		"UInt64 CODEC(ZSTD(1))",
		"UInt64 DEFAULT 5",
		"String COMMENT 'hi'",
	} {
		got, ok := normalizeColumnType(raw)
		assert.False(t, ok, "%q must not normalize", raw)
		assert.Equal(t, raw, got)
	}
}

// TestParseFile_CanonicalizesColumnTypes checks the canonicalizer runs on the
// load path for every column-bearing block, not just declared table columns.
func TestParseFile_CanonicalizesColumnTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`database "db" {
  table "events" {
    engine "merge_tree" {}
    order_by = ["id"]
    column "id"    { type = "UInt64" }
    column "props" { type = "JSON(b String,   a String)" }
  }

  patch_table "events" {
    column "extra" { type = "Map(String,   String)" }
  }

  materialized_view "mv" {
    to_table = "db.dest"
    query    = "SELECT id FROM db.events"
    column "id" { type = "Decimal( 18 , 4 )" }
  }

  dictionary "dict" {
    primary_key = ["id"]
    attribute "id"  { type = "UInt64" }
    attribute "val" { type = "LowCardinality( String )" }
    source "clickhouse" {
      host  = "localhost"
      table = "events"
    }
    layout "flat" {}
    lifetime {
      min = 0
      max = 60
    }
  }
}`), 0o600))

	s, err := ParseFile(path)
	require.NoError(t, err)
	db := s.Databases[0]

	assert.Equal(t, "JSON(a String, b String)", db.Tables[0].Columns[1].Type)
	assert.Equal(t, "Map(String, String)", db.Patches[0].Columns[0].Type)
	assert.Equal(t, "Decimal(18, 4)", db.MaterializedViews[0].Columns[0].Type)
	assert.Equal(t, "LowCardinality(String)", db.Dictionaries[0].Attributes[1].Type)
}

// TestDiff_TypeSpellingIsNotDrift is the behaviour the user sees: a schema that
// differs from the live one only in how a type is written must produce no
// change set and no DDL. Before column types were canonicalized this emitted an
// ALTER TABLE ... MODIFY COLUMN that did nothing but rewrite the column.
func TestDiff_TypeSpellingIsNotDrift(t *testing.T) {
	const createSQL = "CREATE TABLE db.events (`id` UInt64, `props` JSON(a String, b String), `tags` Map(String, String)) ENGINE = MergeTree ORDER BY id"

	live, err := buildTableFromCreateSQL(createSQL)
	require.NoError(t, err)
	live.Name = "events"
	liveDB := DatabaseSpec{Name: "db", Tables: []TableSpec{live}}
	canonicalize(&liveDB)

	path := filepath.Join(t.TempDir(), "schema.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`database "db" {
  table "events" {
    engine "merge_tree" {}
    order_by = ["id"]
    column "id"    { type = "UInt64" }
    column "props" { type = "JSON(b String, a String)" }
    column "tags"  { type = "Map(String,   String)" }
  }
}`), 0o600))
	desired, err := ParseFile(path)
	require.NoError(t, err)

	cs := Diff(&Schema{Databases: []DatabaseSpec{liveDB}}, desired)
	assert.True(t, cs.IsEmpty(), "type spelling alone must not diff: %+v", cs)

	gen := GenerateSQL(cs)
	assert.NotContains(t, strings.Join(gen.Statements, "\n"), "MODIFY COLUMN")
}
