# Canonical column types

## Problem

Before this work, column `type`s are not canonicalized. This results in
spurious diffs.

Clickhouse itself has its own mechanism for this, which we need to acknowledge.
We don't want to get too bogged down in reproducing that, though - apart
from being poorly specified, it can also change in Clickhouse updates.

Example situations where a diff is spurious:

- whitespace and punctuation:
  - `Map(String,   String)` vs `Map(String, String)`
  - `Decimal(18, 4)` vs `Decimal( 18 , 4 )`
- complex arguments:
  - `JSON(a String, b String)` vs `JSON(b String, a String)`
  - `JSON(a String, SKIP b, max_dynamic_paths=200)` vs `JSON(SKIP b, max_dynamic_paths=200, a String)`
  - `Enum('a'=1, 'b'=2)` vs `Enum('b'=2, 'a'=1)`

Counter-examples, situations where a diff is important and desired:

- complex arguments:
  - `Enum('a', 'b')` vs `Enum('b', 'a')` (for enums **without** values, order is meaningful)

### Aims

**Normalization is desirable**: When `hclexp` compares two types, and they are stringwise different but Clickhouse considers them
equal, it is desirable if `hclexp` considers them equal as well. Note that we probably can't and
shouldn't do this in all cases - it's acceptable to keep the current spurious diff in hairy or
difficult situations, as the user is generally diffing code against a DB and is able to modify the code.

But, **false negatives must not happen**: If `hclexp` were to **suppress** a diff that Clickhouse
considers meaningful, this is buggy from the user's perspective. We must not do this.

### Breakdown

There are two (?) normalization approaches we do.

1) Formatting. We can do this perfectly.

2) Semantic. This is hairier - for example, `Enum('a'=1, 'b'2)` == `Enum('b'=2, 'a'=1)`, but `Enum('a', 'b')` != `Enum('b', 'a')`. We probably can't always get this right, so we will err on the side of continuing to emit a diff in ambiguous / difficult cases.

(are there more?)

## Approach

Add `normalizeColumnType` alongside `normalizeExpr` and `normalizeTTL` in
`internal/loader/hcl/query_normalize.go`. It parses the type inside a
throwaway `CREATE TABLE` and renders the resulting `ColumnType` node with
`formatNode` — the identical call `columnFromAST` already makes on the
introspect side, so both sides converge on one spelling by construction.
A type the parser cannot read is kept verbatim, as unparseable
expressions and queries already are.

### The synthetic column position, and what it costs

The wrapping is forced: the parser exports no way to parse a bare type
(`parseColumnType` is unexported, `ParseStmts` is the whole public surface),
so the type has to be interpolated into a column position. That creates one
hazard, which is an artefact of this technique rather than anything in HCL
or ClickHouse. In a column position the grammar reads whatever trails the
type as a *modifier on the synthetic column*, so it lands outside the type
node and disappears when only that node is rendered:

    type = "UInt64 CODEC(ZSTD(1))"
      → ColumnDef{Type: UInt64, Codec: ZSTD(1)}
      → rendering cd.Type gives "UInt64", and the codec is gone

Nobody should write that — `codec`, `default`, `ttl` and `comment` are
first-class `column` attributes — but `columnDefSQL` interpolates the type
verbatim into the column position of generated DDL, so a value like it did
produce the DDL its author meant. Canonicalizing it would silently drop part
of a schema that worked. `isBareColumnType` therefore requires the parsed
column to carry a type and nothing else; anything else keeps its raw text
and warns, naming the modifier as the reason. The same guard covers
`String NULL`, `UInt64 DEFAULT 5` and `String COMMENT 'x'`.

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

`SKIP REGEXP` is the one order-insensitive-looking list left alone.
`doGetName` prints `path_regexps_to_skip` in insertion order, so two
orderings are two type names to the server, and reordering them would be the
only place the canonical form claims two distinct server types are one. See
*What each direction actually costs* for why that trade is not worth making.

### What each direction actually costs

Calling under-normalization a bug overstates it. Sorted honestly:

**Normalizing less than the server** produces a false positive. The
authored type never matches the introspected one, so the column shows a
change on every run and `diff -sql` emits an `ALTER TABLE … MODIFY COLUMN`
that rewrites the column to what it already is. In the ordinary case an
author fixes it in a minute by spelling the type the way the server does —
or never meets it at all, having dumped the schema from a cluster to begin
with. So it is an inconvenience with a manual remedy, not a wrong answer.

Two things make it worth fixing anyway, and neither is severity:

- Until it is fixed the generated DDL contains a real mutation. On a large
  table that is expensive, and in a pipeline that applies generated SQL it
  is executed rather than read.
- It is a trap that recurs. It cannot be fixed once; it returns for every
  new author, table and column that spells the type naturally.

And two places have no manual remedy:

- `drift` compares machine-generated dumps. There is no HCL to reword, so a
  naming difference between two nodes is reported and cannot be settled —
  and drift exits non-zero as a CI guard, so the choice becomes a
  permanently red build or a suppression that hides real drift.
- If two versions in the fleet name one type differently, no single
  authored spelling satisfies both: fixing one environment breaks the other.
  This is the only true bug in the class, and the clearest example of it is
  the `max_dynamic_*` default handling this change leaves out of scope.

**Normalizing more than the server** produces a false negative: hclexp says
two types are equal where `SHOW CREATE TABLE` shows different names. Version
skew gets absorbed rather than reported, which is convenient, but the tool
now disagrees with the server about type identity.

**The resolution is to match the server exactly, with no exceptions.** Every
list reordered here is one ClickHouse itself reorders. Nothing is sorted
that the server leaves alone — notably `SKIP REGEXP`, which was sorted in an
earlier revision of this change and is not any more. An exception-free rule
is worth more than the marginal robustness of pre-empting a version that
might one day sort something: the rule states in one line, one test checks
it, and if a version does move, that test says so and we follow.

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
