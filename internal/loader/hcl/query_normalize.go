package hcl

import (
	"log/slog"
	"sort"
	"strings"

	chparser "github.com/orian/clickhouse-sql-parser/parser"
)

// normalizeQueries canonicalizes every view and materialized-view query in db
// to the beautified form. A query the parser can't handle is kept verbatim and
// a warning is logged: loading never fails on a query that is valid to
// ClickHouse but not yet expressible by the parser (it may, however, diff as
// drift until the parser catches up).
func normalizeQueries(db *DatabaseSpec) {
	for i := range db.Views {
		if q, ok := normalizeQuery(db.Views[i].Query); ok {
			db.Views[i].Query = q
		} else if strings.TrimSpace(db.Views[i].Query) != "" {
			slog.Warn("view query could not be parsed for normalization; keeping raw (may diff as drift)",
				"database", db.Name, "view", db.Views[i].Name)
		}
	}
	for i := range db.MaterializedViews {
		if q, ok := normalizeQuery(db.MaterializedViews[i].Query); ok {
			db.MaterializedViews[i].Query = q
		} else if strings.TrimSpace(db.MaterializedViews[i].Query) != "" {
			slog.Warn("materialized view query could not be parsed for normalization; keeping raw (may diff as drift)",
				"database", db.Name, "materialized_view", db.MaterializedViews[i].Name)
		}
	}
	for ti := range db.Tables {
		for pi := range db.Tables[ti].Projections {
			p := &db.Tables[ti].Projections[pi]
			if q, ok := normalizeQuery(p.Query); ok {
				p.Query = q
			} else if strings.TrimSpace(p.Query) != "" {
				slog.Warn("projection query could not be parsed for normalization; keeping raw (may diff as drift)",
					"database", db.Name, "table", db.Tables[ti].Name, "projection", p.Name)
			}
		}
	}
}

// beautifyNode renders an AST node as indented, multi-line SQL via the parser's
// BeautifyVisitor — the readable counterpart to formatNode. This is the
// canonical form for view / materialized-view queries: the same logical query
// renders identically whether it was authored (one-line, heredoc, or via
// file()) or introspected from a live cluster, so formatting never shows as
// drift. Redundant outermost clause parentheses are stripped first (see
// stripRedundantClauseParens) so ClickHouse's HAVING ((a) AND (b)) and the
// authored HAVING (a) AND (b) converge.
func beautifyNode(n chparser.Expr) string {
	if n == nil {
		return ""
	}
	stripRedundantClauseParens(n)
	v := chparser.NewBeautifyVisitor()
	if err := n.Accept(v); err != nil {
		return ""
	}
	return strings.TrimSpace(v.String())
}

// unwrapRootParens removes redundant outermost parentheses from a standalone
// expression. A parenthesised scalar `(x)` parses to a single-item
// ParamExprList (ColumnArgList == nil, exactly one Item), and the parser wraps
// every list item and clause value in an alias-less ColumnExpr; both are
// transparent at an expression-root position — a clause value, or a whole
// column / index expression — so peeling them is safe regardless of the inner
// operator's precedence. Tuples `(a, b)` (len > 1), aliased ColumnExprs, and
// subqueries (a distinct AST node) are left untouched. Only the outermost
// layer(s) are removed; inner parentheses are preserved because dropping them
// would require precedence analysis (e.g. `(a + b) * c`).
func unwrapRootParens(e chparser.Expr) chparser.Expr {
	for {
		switch n := e.(type) {
		case *chparser.ColumnExpr:
			if n.Alias != nil {
				return e
			}
			e = n.Expr
		case *chparser.ParamExprList:
			if n.ColumnArgList != nil || n.Items == nil || len(n.Items.Items) != 1 {
				return e
			}
			e = n.Items.Items[0]
		default:
			return e
		}
	}
}

// stripClauseParens canonicalizes a WHERE / PREWHERE / HAVING value. The parser
// stores it as an alias-less ColumnExpr; we keep that wrapper node and only
// canonicalize the expression inside it, so a clause with no redundant parens
// is left byte-identical (no snapshot churn) while ClickHouse's redundant outer
// pair is dropped — and both sides end up with the same node shape, so long
// clauses that the beautifier line-wraps format identically.
func stripClauseParens(e chparser.Expr) chparser.Expr {
	if ce, ok := e.(*chparser.ColumnExpr); ok && ce.Alias == nil {
		ce.Expr = unwrapRootParens(ce.Expr)
		return ce
	}
	return unwrapRootParens(e)
}

// clauseParenStripper unwraps redundant outermost parentheses from the WHERE /
// PREWHERE / HAVING value of every SELECT it visits, including nested CTE and
// subquery SELECTs.
type clauseParenStripper struct {
	chparser.DefaultASTVisitor
}

func (v *clauseParenStripper) Enter(e chparser.Expr) {
	sq, ok := e.(*chparser.SelectQuery)
	if !ok {
		return
	}
	if sq.Prewhere != nil {
		sq.Prewhere.Expr = stripClauseParens(sq.Prewhere.Expr)
	}
	if sq.Where != nil {
		sq.Where.Expr = stripClauseParens(sq.Where.Expr)
	}
	if sq.Having != nil {
		sq.Having.Expr = stripClauseParens(sq.Having.Expr)
	}
}

// stripRedundantClauseParens walks n in place, canonicalizing clause-level
// parentheses in every SELECT it contains. The AST is always a throwaway parse
// here, so mutating it is safe.
func stripRedundantClauseParens(n chparser.Expr) {
	v := &clauseParenStripper{}
	v.Self = v
	_ = n.Accept(v)
}

// normalizeExpr canonicalizes a single scalar expression — a column
// DEFAULT / MATERIALIZED / ALIAS expression, an index expression or type — to
// the same compact form introspection renders, so an authored expression and
// its live-introspected counterpart compare equal (issue #136 items 2 and 3).
// It parses the expression (wrapped in a throwaway SELECT so a bare expression
// is accepted), strips redundant outermost parentheses, and renders it via the
// same printer introspect uses. Returns ok=false with the input unchanged when
// it can't be parsed, so the caller can keep the raw text.
func normalizeExpr(s string) (string, bool) {
	if strings.TrimSpace(s) == "" {
		return s, true
	}
	stmt, err := parseCreateStatement("SELECT " + s)
	if err != nil {
		return s, false
	}
	sel, ok := stmt.(*chparser.SelectQuery)
	if !ok || len(sel.SelectItems) != 1 || sel.SelectItems[0].Alias != nil {
		return s, false
	}
	return formatNode(unwrapRootParens(sel.SelectItems[0].Expr)), true
}

// normalizeColumnType canonicalizes a column type to the text the parser's
// printer emits — the same rendering introspection produces in columnFromAST —
// so two spellings of one type compare equal instead of generating a no-op
// ALTER TABLE ... MODIFY COLUMN. It covers whitespace and punctuation
// (`Map(String,   String)`, `Decimal( 18 , 4 )`, `Enum8('a' = 1)`) and, via
// canonicalizeJSONOptions, the order of the options inside a JSON type. The
// type is parsed inside a throwaway CREATE TABLE because the grammar accepts a
// type only in a column position. Returns ok=false with the input unchanged
// when it can't be parsed, so the caller keeps the raw text.
func normalizeColumnType(s string) (string, bool) {
	if strings.TrimSpace(s) == "" {
		return s, true
	}
	stmt, err := parseCreateStatement("CREATE TABLE _norm_type (_c " + s + ") ENGINE = MergeTree ORDER BY tuple()")
	if err != nil {
		return s, false
	}
	ct, ok := stmt.(*chparser.CreateTable)
	if !ok || ct.TableSchema == nil || len(ct.TableSchema.Columns) != 1 {
		return s, false
	}
	cd, ok := ct.TableSchema.Columns[0].(*chparser.ColumnDef)
	if !ok || cd.Type == nil || !isBareColumnType(cd) {
		return s, false
	}
	canonicalizeJSONOptions(cd.Type)
	return formatNode(cd.Type), true
}

// isBareColumnType reports whether the parsed column carries a type and
// nothing else. A `type` that smuggles in a modifier — `type = "UInt64
// CODEC(ZSTD(1))"` — parses fine, but rendering only the type node would drop
// the modifier from the generated DDL. Those are kept verbatim instead, exactly
// like a type the parser cannot read.
func isBareColumnType(cd *chparser.ColumnDef) bool {
	return cd.NotNull == nil && cd.Nullable == nil &&
		cd.DefaultExpr == nil && cd.MaterializedExpr == nil &&
		!cd.IsEphemeral && cd.EphemeralExpr == nil && cd.AliasExpr == nil &&
		cd.Codec == nil && cd.TTL == nil &&
		cd.Comment == nil && cd.CompressionCodec == nil
}

// JSON option ranks, in the order ClickHouse's DataTypeObject::doGetName emits
// them: max_dynamic_types, max_dynamic_paths, typed-path hints, SKIP paths,
// SKIP REGEXP. The parser's printer groups the three coarse kinds itself
// (parameters, hints, skips), so these finer ranks refine that grouping rather
// than fight it.
const (
	rankMaxDynamicTypes = iota
	rankMaxDynamicPaths
	rankTypeHint
	rankSkipPath
	rankSkipRegexp
)

func jsonOptionRank(o *chparser.JSONOption) int {
	switch {
	case o.MaxDynamicTypes != nil:
		return rankMaxDynamicTypes
	case o.MaxDynamicPaths != nil:
		return rankMaxDynamicPaths
	case o.Column != nil:
		return rankTypeHint
	case o.SkipPath != nil:
		return rankSkipPath
	case o.SkipRegex != nil:
		return rankSkipRegexp
	default:
		return rankMaxDynamicTypes
	}
}

// jsonOptionSorter puts every JSON type's options into the order ClickHouse
// itself uses, and no stronger. ClickHouse holds a JSON type's typed paths in a
// hash map and its skip paths in a hash set, so it has to sort both to name the
// type at all: DataTypeObject::doGetName sorts `typed_paths` and
// `paths_to_skip` alphabetically. `JSON(b String, a String)` and
// `JSON(a String, b String)` are therefore one type to ClickHouse, and matching
// that here is what stops a reordered hint list reading as drift.
//
// SKIP REGEXP is deliberately left in place: ClickHouse writes
// `path_regexps_to_skip` in insertion order with no sort, so two orderings are
// two type names to ClickHouse, and reordering them here would canonicalize
// harder than the server does and hide a difference it can see.
type jsonOptionSorter struct {
	chparser.DefaultASTVisitor
}

func (v *jsonOptionSorter) Enter(e chparser.Expr) {
	j, ok := e.(*chparser.JSONType)
	if !ok || j.Options == nil {
		return
	}
	sort.SliceStable(j.Options.Items, func(a, b int) bool {
		x, y := j.Options.Items[a], j.Options.Items[b]
		rx, ry := jsonOptionRank(x), jsonOptionRank(y)
		if rx != ry {
			return rx < ry
		}
		if rx == rankSkipRegexp {
			return false // keep the authored order; ClickHouse does not sort these
		}
		return x.String() < y.String()
	})
	// The default walk stops at a JSON type's name, so the type of each hint is
	// descended into here: a JSON nested inside a hint needs the same treatment
	// as one at the top level.
	for _, item := range j.Options.Items {
		if item.Column != nil && item.Column.Type != nil {
			_ = item.Column.Type.Accept(v.Self)
		}
	}
}

// canonicalizeJSONOptions sorts the options of every JSON type in t, including
// those nested inside Array / Map / Tuple / Nested parameters. The AST is
// always a throwaway parse here, so mutating it is safe.
func canonicalizeJSONOptions(t chparser.Expr) {
	v := &jsonOptionSorter{}
	v.Self = v
	_ = t.Accept(v)
}

// normalizeColumnTypePtr canonicalizes an optional type string in place,
// leaving it untouched when unset or unparseable.
func normalizeColumnTypePtr(p **string) {
	if *p == nil {
		return
	}
	if nt, ok := normalizeColumnType(**p); ok {
		*p = &nt
	}
}

// normalizeTTL canonicalizes a table TTL clause to the same text introspection
// renders (formatTTLItems), so an authored TTL and its live-introspected
// counterpart compare equal. A stored TTL is rewritten by ClickHouse — INTERVAL
// 7 DAY becomes toIntervalDay(7), and a move rule (TO DISK / TO VOLUME) rides on
// the clause — so a raw string compare of authored vs introspected TTL never
// matches and the diff emits a perpetual no-op MODIFY TTL. Parsing both sides
// through the same printer removes that asymmetry (issue #136, TTL case).
// Returns ok=false with the input unchanged when it can't be parsed, so the
// caller keeps the raw text.
func normalizeTTL(s string) (string, bool) {
	if strings.TrimSpace(s) == "" {
		return s, true
	}
	stmt, err := parseCreateStatement("CREATE TABLE _norm_ttl (_x Int) ENGINE = MergeTree ORDER BY _x TTL " + s)
	if err != nil {
		return s, false
	}
	ct, ok := stmt.(*chparser.CreateTable)
	if !ok || ct.Engine == nil || ct.Engine.TTL == nil || len(ct.Engine.TTL.Items) == 0 {
		return s, false
	}
	return formatTTLItems(ct.Engine.TTL.Items), true
}

// normalizeTTLPtr canonicalizes a *string TTL field in place, leaving it
// untouched when nil or unparseable.
func normalizeTTLPtr(p **string) {
	if *p == nil {
		return
	}
	if v, ok := normalizeTTL(**p); ok {
		*p = &v
	}
}

// canonicalize brings every expression-bearing field of db to a single
// canonical string form, so a schema composed from HCL and the same schema
// introspected from a live cluster reduce to identical text and diff clean
// (issue #136). It is run at the tail of both the load path (ParseFile) and the
// introspect path.
func canonicalize(db *DatabaseSpec) {
	normalizeQueries(db)
	for ti := range db.Tables {
		t := &db.Tables[ti]
		normalizeColumnExprs(t.Columns)
		normalizePatchColumnExprs(t.ColumnPatches)
		normalizeIndexExprs(t.Indexes)
		normalizeTTLPtr(&t.TTL)
	}
	// A materialized view's explicit column list is diffed like a table's, so
	// its types and expressions need the same canonical form.
	for vi := range db.MaterializedViews {
		normalizeColumnExprs(db.MaterializedViews[vi].Columns)
	}
	for di := range db.Dictionaries {
		attrs := db.Dictionaries[di].Attributes
		for ai := range attrs {
			if nt, ok := normalizeColumnType(attrs[ai].Type); ok {
				attrs[ai].Type = nt
			}
		}
	}
	// Patch fields land verbatim on their targets at resolution, so they
	// must be canonicalized exactly like declared fields — otherwise a
	// patched expression would diff against its own introspected form.
	// (order_by/partition_by/sample_by are deliberately left verbatim, exactly
	// as they are on declared tables; ttl is normalized because ClickHouse
	// rewrites it — see normalizeTTL.)
	for pi := range db.Patches {
		p := &db.Patches[pi]
		normalizeColumnExprs(p.Columns)
		normalizeColumnExprs(p.ModifyColumns)
		normalizeIndexExprs(p.Indexes)
		for i := range p.Projections {
			projection := &p.Projections[i]
			if q, ok := normalizeQuery(projection.Query); ok {
				projection.Query = q
			} else if strings.TrimSpace(projection.Query) != "" {
				slog.Warn("patch_table projection query could not be parsed for normalization; keeping raw (may diff as drift)",
					"database", db.Name, "table", p.Name, "projection", projection.Name)
			}
		}
		normalizeTTLPtr(&p.TTL)
	}
	for pi := range db.MaterializedViewPatches {
		p := &db.MaterializedViewPatches[pi]
		normalizeColumnExprs(p.Columns)
		normalizeColumnExprs(p.ModifyColumns)
		if p.Query == nil {
			continue
		}
		if q, ok := normalizeQuery(*p.Query); ok {
			p.Query = &q
		} else if strings.TrimSpace(*p.Query) != "" {
			slog.Warn("patch_materialized_view query could not be parsed for normalization; keeping raw (may diff as drift)",
				"database", db.Name, "materialized_view", p.Name)
		}
	}
	for pi := range db.ViewPatches {
		p := &db.ViewPatches[pi]
		if p.Query == nil {
			continue
		}
		if q, ok := normalizeQuery(*p.Query); ok {
			p.Query = &q
		} else if strings.TrimSpace(*p.Query) != "" {
			slog.Warn("patch_view query could not be parsed for normalization; keeping raw (may diff as drift)",
				"database", db.Name, "view", p.Name)
		}
	}
}

// normalizeColumnExprs canonicalizes the type and the expression-bearing
// fields of each column in place.
func normalizeColumnExprs(cols []ColumnSpec) {
	for ci := range cols {
		c := &cols[ci]
		if nt, ok := normalizeColumnType(c.Type); ok {
			c.Type = nt
		}
		normalizeExprPtr(&c.Default)
		normalizeExprPtr(&c.Materialized)
		normalizeExprPtr(&c.Alias)
		normalizeExprPtr(&c.Ephemeral)
	}
}

// normalizePatchColumnExprs canonicalizes the expression-bearing fields of
// partial inherited-column patches exactly like full column declarations.
func normalizePatchColumnExprs(patches []PatchColumnSpec) {
	for i := range patches {
		p := &patches[i]
		normalizeColumnTypePtr(&p.Type)
		normalizeExprPtr(&p.Default)
		normalizeExprPtr(&p.Materialized)
		normalizeExprPtr(&p.Alias)
		normalizeExprPtr(&p.Ephemeral)
	}
}

// normalizeIndexExprs canonicalizes each index's expr and type in place.
func normalizeIndexExprs(idxs []IndexSpec) {
	for ii := range idxs {
		idx := &idxs[ii]
		if nx, ok := normalizeExpr(idx.Expr); ok {
			idx.Expr = nx
		}
		if nt, ok := normalizeExpr(idx.Type); ok {
			idx.Type = nt
		}
	}
}

// normalizeExprPtr canonicalizes an optional expression string in place,
// leaving it untouched when unset or unparseable.
func normalizeExprPtr(p **string) {
	if *p == nil {
		return
	}
	if nx, ok := normalizeExpr(**p); ok {
		*p = &nx
	}
}

// BeautifySQL parses a single CREATE statement and returns it re-rendered in
// the parser's beautified (indented, multi-line) form — the same visitor that
// produces readable view/MV queries elsewhere. It returns ok=false with the
// input unchanged when the statement can't be parsed, so callers can fall back
// to the verbatim SQL (e.g. for DDL the parser doesn't yet handle).
func BeautifySQL(sql string) (string, bool) {
	stmt, err := parseCreateStatement(sql)
	if err != nil {
		return sql, false
	}
	return beautifyNode(stmt), true
}

// normalizeQuery canonicalizes a view/MV SELECT body to the beautified form. It
// parses the query — wrapped in a throwaway CREATE VIEW so a bare SELECT is
// accepted — and beautifies the SELECT subtree, matching what introspect emits
// for the same query. Returns ok=false (and the input unchanged) when the query
// can't be parsed, so the caller can keep the raw text and warn.
func normalizeQuery(sql string) (string, bool) {
	if strings.TrimSpace(sql) == "" {
		return sql, true
	}
	stmt, err := parseCreateStatement("CREATE VIEW __normalize__ AS " + sql)
	if err != nil {
		return sql, false
	}
	cv, ok := stmt.(*chparser.CreateView)
	if !ok || cv.SubQuery == nil || cv.SubQuery.Select == nil {
		return sql, false
	}
	return beautifyNode(cv.SubQuery.Select), true
}
