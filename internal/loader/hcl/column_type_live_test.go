package hcl

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
// exactly that: each type is declared in one spelling, introspected, and the
// introspected form is compared against the canonicalized *authored* form.
//
// It is the direction that matters. If ClickHouse ever canonicalizes something
// we leave alone — sorting SKIP REGEXP, say — the server's form and ours diverge
// and every schema carrying that type diffs forever. That is the reported bug,
// and this test fails first. The reverse (ClickHouse canonicalizing less than we
// do) cannot cause phantom drift, because both sides pass through the same
// function.
func TestCHLive_ColumnTypeCanonicalFormMatchesServer(t *testing.T) {
	if !*clickhouseLive {
		t.Skip("pass -clickhouse to run against a live ClickHouse")
	}
	conn := testhelpers.RequireClickHouse(t)
	dbName := testhelpers.CreateTestDatabase(t, conn)
	ctx := context.Background()

	// Each authored spelling is deliberately not in canonical form: reordered
	// JSON hints and skip paths, loose whitespace, spaced enum assignments.
	cases := []struct {
		column   string
		authored string
	}{
		{"c_map", "Map(String,   String)"},
		{"c_decimal", "Decimal( 18 , 4 )"},
		{"c_enum", "Enum8('b' = 2, 'a' = 1)"},
		{"c_lc", "LowCardinality( Nullable( String ) )"},
		{"c_tuple", "Tuple(b Int32, a String)"},
		{"c_json_hints", "JSON(b String, a String)"},
		{"c_json_skips", "JSON(SKIP z, b String, SKIP c, a String)"},
		{"c_json_params", "JSON(max_dynamic_paths=16, max_dynamic_types=8, b String, a String)"},
		{"c_json_regexp", "JSON(SKIP REGEXP '^b', SKIP REGEXP '^a')"},
		{"c_json_nested", "Array(JSON(b String, a String))"},
	}

	cols := make([]string, 0, len(cases))
	for _, c := range cases {
		cols = append(cols, fmt.Sprintf("`%s` %s", c.column, c.authored))
	}
	stmt := fmt.Sprintf("CREATE TABLE %s.types (`id` UInt64, %s) ENGINE = MergeTree ORDER BY id",
		dbName, strings.Join(cols, ", "))
	require.NoError(t, conn.Exec(ctx, stmt), "DDL rejected:\n%s", stmt)

	db, err := Introspect(ctx, conn, dbName, false)
	require.NoError(t, err)
	require.Len(t, db.Tables, 1)

	byName := map[string]string{}
	for _, c := range db.Tables[0].Columns {
		byName[c.Name] = c.Type
	}

	for _, c := range cases {
		t.Run(c.column, func(t *testing.T) {
			want, ok := normalizeColumnType(c.authored)
			require.True(t, ok, "authored type must canonicalize")
			assert.Equal(t, want, byName[c.column],
				"the canonicalized authored type must equal the introspected type;\n"+
					"if this fails after a ClickHouse upgrade, the server's own canonical\n"+
					"form for %s has changed and jsonOptionSorter needs to follow it",
				c.authored)
		})
	}
}
