# Canonical column types

## Problem

A column's `type` is the only expression-shaped field that is never
canonicalized. Every other one — view and MV queries, column
DEFAULT/MATERIALIZED/ALIAS/EPHEMERAL expressions, index expressions and
index types, table TTL — is parsed and re-rendered through the parser's
printer at the tail of both the load path and the introspect path, so an
authored form and its live-introspected counterpart reduce to the same
text (issue #136). The type string is carried verbatim from HCL and
compared byte-for-byte in `columnsEqual`.

Two spellings of the same type therefore read as a change and generate an
`ALTER TABLE … MODIFY COLUMN` that does nothing:

- whitespace and punctuation — `Map(String,   String)` against the
  server's `Map(String, String)`, `Decimal( 18 , 4 )`, `Enum8('a' = 1)`
  against `Enum8('a'=1)`;
- ordering inside a `JSON` type — `JSON(b String, a String)` against
  `JSON(a String, b String)`. JSON typed-path hints, SKIP paths and the
  `max_dynamic_*` parameters are a set, not a sequence, so the order they
  are written in carries no meaning.

The rogue statement is worse than noise: `MODIFY COLUMN` on a large table
is a mutation, so a whitespace edit reads as a heavyweight migration.

## Approach

Add `normalizeColumnType` alongside `normalizeExpr` and `normalizeTTL` in
`internal/loader/hcl/query_normalize.go`. It parses the type inside a
throwaway `CREATE TABLE` and renders the resulting `ColumnType` node with
`formatNode` — the identical call `columnFromAST` already makes on the
introspect side, so both sides converge on one spelling by construction.
A type the parser cannot read is kept verbatim, as unparseable
expressions and queries already are. So is a `type` that smuggles in a
column modifier (`type = "UInt64 CODEC(ZSTD(1))"`): it parses cleanly, but
rendering the type node alone would drop the modifier from the generated
DDL, so `isBareColumnType` rejects it and the raw text stands.

Before rendering, the type tree is walked and every `JSON` type's options
are put into the order ClickHouse itself uses. The walk descends through
`Array`, `Map`, `Tuple` and `Nested` parameters, and through the type of
each JSON hint, so a nested `Array(JSON(b String, a String))`
canonicalizes too.

The ordering is copied from `DataTypeObject::doGetName`
(`src/DataTypes/DataTypeObject.cpp`) rather than invented, on the rule that
nothing should be canonicalized harder than the server does it — otherwise
a difference ClickHouse can see would be hidden. What that source shows:

- `typed_paths` is a hash map and `paths_to_skip` a hash set, so both are
  `std::sort`ed before printing. ClickHouse has no choice: an unordered
  container cannot produce a deterministic type name. Two hint orderings
  are therefore one type to the server, and sorting here matches it.
- `path_regexps_to_skip` is printed in insertion order with no sort, so
  two `SKIP REGEXP` orderings are two type names. Ours are left in place.
- Parameters come first, `max_dynamic_types` before `max_dynamic_paths`,
  and each is omitted when it equals its default.

The parser's printer groups options coarsely (parameters, hints, skips) on
its own, so the ranks here refine that grouping rather than fight it.

`canonicalize` then normalizes the type of every column it can reach:
declared table columns, `patch_table` `column` / `modify_column`,
`patch_column` specializations, materialized-view columns (and the MV
patch forms), and dictionary attributes. MV columns were not visited at
all before, so their expressions are now canonicalized as well, matching
declared table columns.

## Deliberately out of scope

`Enum8('b' = 2, 'a' = 1)` is also order-independent when every element
carries an explicit value, but reordering an enum whose values are
implicit (`Enum8('a', 'b')`) silently renumbers it. The distinction is
worth a separate change, not a rider on this one. Worth checking first
whether `DataTypeEnum`'s name generation sorts, as `DataTypeObject`'s does;
if it sorts by value, an authored reorder currently diffs against the
server and the same argument applies.

A `max_dynamic_paths` or `max_dynamic_types` written at its default value
also still diffs, because ClickHouse omits a default parameter from the
type name entirely. Matching that means hardcoding the defaults (1024
paths, 32 types at the time of writing), which are version-specific
constants in the server, so it is left alone.

## Tests

All in `column_type_normalize_test.go`:

- unit cases over `normalizeColumnType` — whitespace, nested types, JSON
  option ordering, nested JSON, `Tuple` order left alone, an unparseable
  type and a smuggled modifier kept verbatim, idempotence throughout;
- a parse-level test that a loaded HCL file stores canonical types for a
  table column, a `patch_table` column, an MV column and a dictionary
  attribute;
- the end-to-end guard: an HCL schema and a live `CREATE TABLE` differing
  only in type spelling produce an empty change set and no
  `MODIFY COLUMN`. Reverting the normalizer call makes it fail with
  `ALTER TABLE db.events MODIFY COLUMN props JSON(b String, a String),
  MODIFY COLUMN tags Map(String,   String)` — the reported bug.
