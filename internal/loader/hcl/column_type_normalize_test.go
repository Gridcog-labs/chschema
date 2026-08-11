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
