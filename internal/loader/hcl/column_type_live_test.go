package hcl

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/posthog/chschema/test/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCHLive_ColumnTypeCanonicalFormMatchesServer pins the assumption behind
// type canonicalization against a real server, so a ClickHouse upgrade that
// changes how a type is named breaks a test rather than production diffs.
//
// The rule the canonicalizer follows is that a type spelling must reduce to the
// same string whether it was authored or read back from a cluster. This asserts
// exactly that: each type is declared in a deliberately non-canonical spelling,
// introspected, and the introspected form compared against the canonicalized
// *authored* form.
//
// It is the direction that matters. If ClickHouse canonicalizes something we
// leave alone, the server's form and ours diverge and every schema carrying that
// type diffs forever — the reported bug, and this fails first. The reverse
// (ClickHouse canonicalizing less than we do) cannot cause phantom drift,
// because both sides pass through the same function.
//
// Each type gets its own table and a type the server rejects skips rather than
// fails, so running against an older or newer ClickHouse reports "not
// supported here" instead of a false alarm.
func TestCHLive_ColumnTypeCanonicalFormMatchesServer(t *testing.T) {
	if !*clickhouseLive {
		t.Skip("pass -clickhouse to run against a live ClickHouse")
	}
	conn := testhelpers.RequireClickHouse(t)
	dbName := testhelpers.CreateTestDatabase(t, conn)
	ctx := context.Background()

	// Settings retried with when a plain CREATE is rejected: some of these types
	// are behind an experimental flag on some versions, and gone from behind it
	// on others.
	optIn := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"allow_experimental_variant_type": 1,
		"enable_variant_type":             1,
		"enable_json_type":                1,
	}))

	cases := []struct {
		name     string
		authored string
	}{
		{"map", "Map(String,   String)"},
		{"decimal", "Decimal( 18 , 4 )"},
		{"lowcardinality", "LowCardinality( Nullable( String ) )"},
		{"tuple", "Tuple(b Int32, a String)"},
		{"enum", "Enum8('b' = 2, 'a' = 1)"},
		{"enum_implicit", "Enum8('b', 'a')"},
		{"variant", "Variant(UInt64, String)"},
		{"variant_nested", "Map(String, Variant(UInt64, String))"},
		{"json_hints", "JSON(b String, a String)"},
		{"json_skips", "JSON(SKIP z, b String, SKIP c, a String)"},
		{"json_params", "JSON(max_dynamic_paths=16, max_dynamic_types=8, b String, a String)"},
		{"json_regexp", "JSON(SKIP REGEXP '^b', SKIP REGEXP '^a')"},
		{"json_nested", "Array(JSON(b String, a String))"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table := "t_" + c.name
			stmt := fmt.Sprintf("CREATE TABLE %s.%s (`id` UInt64, `c` %s) ENGINE = MergeTree ORDER BY id",
				dbName, table, c.authored)
			if err := conn.Exec(ctx, stmt); err != nil {
				if err2 := conn.Exec(optIn, stmt); err2 != nil {
					t.Skipf("server rejected %s: %v", c.authored, err)
				}
			}

			db, err := Introspect(ctx, conn, dbName, false)
			require.NoError(t, err)
			var got string
			for _, tbl := range db.Tables {
				if tbl.Name != table {
					continue
				}
				for _, col := range tbl.Columns {
					if col.Name == "c" {
						got = col.Type
					}
				}
			}
			require.NotEmpty(t, got, "column not introspected")

			want, ok := normalizeColumnType(c.authored)
			require.True(t, ok, "authored type must canonicalize")
			assert.Equal(t, want, got,
				"the canonicalized authored type must equal the introspected type;\n"+
					"if this fails after a ClickHouse upgrade, the server's canonical form\n"+
					"for %s has changed and canonicalizeTypeOrder must follow it",
				c.authored)
		})
	}
}
