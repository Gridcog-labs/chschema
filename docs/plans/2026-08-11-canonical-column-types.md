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

## Several ClickHouse versions at once

A fleet is not on one version. Cloud upgrades itself, a self-hosted stack
lags, a cluster spends time mid-upgrade with nodes on either side, and dev
and prod are rarely in step. Every comparison hclexp makes can therefore
straddle two versions: `diff` against each of two environments, `plan`
against a dump topology, and `drift` between per-node dumps.

That rules out the obvious fix of normalizing per server version. A dump
outlives the connection that produced it — `drift` compares two dump files
with no server attached — so a canonical form that depended on the version
would make two nodes' dumps incomparable, which is the one thing `drift`
exists to do. **The canonical form has to be version-independent.**

Which gives the rule this change follows. Do not copy a version's printer;
canonicalize where the type constructor is *semantically* a set or a map,
because there no version can differ:

- a JSON type's typed paths are a map and its skip paths are a set —
  ClickHouse cannot even name the type without sorting them;
- `Variant(T1, T2) = Variant(T2, T1)` is documented type identity, and
  `DataTypeVariant`'s constructor sorts by type name to enforce it;
- an enum's elements are a set of (name, value) pairs, and `EnumValues`
  sorts them by value.

Reordering any of those cannot change storage or query results on any
version, so reducing them to one form is version-proof rather than
version-coupled. Positional argument lists — `Tuple`, `Nested`, a `Map`'s
key and value, `Decimal`'s precision and scale, an `AggregateFunction`'s
argument types — are never touched.

Applying that rule turned up two cases where hclexp was *weaker* than every
ClickHouse: `Variant(UInt64, String)` and `Enum8('b' = 2, 'a' = 1)` both
diffed forever against the server's own spelling. Both are now canonical.

It also reverses one earlier decision. `SKIP REGEXP` is sorted, even though
`doGetName` prints `path_regexps_to_skip` in insertion order, because a list
of patterns to ignore is a set: a path is skipped if any pattern matches, so
order has no meaning, and two nodes differing only in that order behave
identically. Sorting it is deliberately stronger than one version's type
name, and it is the only place that is true. The trade is explicit — an
operator comparing `SHOW CREATE TABLE` by eye would see a difference hclexp
calls equal — and it buys immunity from a version that decides to sort them.

### Which direction is harmful

The two directions of divergence are not symmetrical.

**Normalizing less than a server in the fleet** is the bug. That server
reports a form we do not produce, the authored type never matches it, and
every schema carrying that type diffs forever.

**Normalizing more than a server in the fleet** is benign. Both the
authored and the introspected type pass through our canonicalizer, so they
still match; version skew is absorbed rather than reported. The cost is a
cosmetic false negative on a difference that cannot affect behaviour.

So the bias is deliberate: where a constructor is order-insensitive, being
stronger than the weakest version in the fleet is what keeps a mixed-version
estate quiet.

`TestCHLive_ColumnTypeCanonicalFormMatchesServer` guards the harmful
direction in CI. The `test-live` job runs the live suite against the
docker-compose ClickHouse on every pull request and `build` depends on it.
Each type is declared in a non-canonical spelling, introspected, and the
introspected form compared against the canonicalized authored form, so a
version bump that changes the server's naming rules fails on the bump rather
than in someone's diff. Each case gets its own table and a type the server
rejects skips instead of failing, so the test is itself version-tolerant.

Version differences that are *not* about ordering — a renamed engine, a new
default setting, a clause a version starts emitting — are out of reach of
canonicalization and handled where they arise, at the introspect edge. The
`cloud_mode_engine` handling for Cloud's `Shared*MergeTree` rewriting is the
precedent: converge the flavour-specific spelling onto the single HCL
vocabulary as the schema is read, never downstream of it.

The deeper risk is the SQL parser, not ClickHouse: normalization renders a
third-party AST back to text, so a parser release that accepts a type but
drops part of it would change what hclexp generates — the only failure here
that could alter a real table rather than add noise.
`TestNormalizeColumnType_ChangesLayoutOnly` pins that: a corpus of real
types must come back identical once whitespace is removed. Auditing ~34
type shapes found no content loss today, and one type the parser cannot
read at all (`Dynamic(max_types=10)`), which correctly degrades to
verbatim. That degradation used to be silent; `warnUncanonicalType` now
reports it once per distinct type per run, so a column that diffs forever
has a visible cause.

A more conservative design was considered and rejected: canonicalize only
for the comparison and keep the authored text for DDL, so a lossy parser
render could never reach a cluster. It would make types the one field whose
stored value is not canonical, breaking the property that a dump and a
declaration converge, and it contradicts how queries and TTL already work.
The layout-only corpus addresses the same risk without that cost.

## Deliberately out of scope

An enum with any implicit element (`Enum8('a', 'b')`) is left in declared
order. Sorting by value is safe for a fully explicit enum and is what
ClickHouse does, but an implicit element takes its number from its position,
so position and value cannot be separated without renumbering the enum.
Ordering is skipped for the whole type rather than guessed at.

A `max_dynamic_paths` or `max_dynamic_types` written at its default value
still diffs, because ClickHouse omits a default parameter from the type name
entirely. Matching that means hardcoding the defaults (1024 paths, 32 types
at the time of writing), and those are version-specific constants in the
server — exactly the version coupling this design avoids elsewhere — so it
is left alone.

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
