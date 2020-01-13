package analyzer

import (
	"errors"
	"github.com/src-d/go-mysql-server/sql"
	"github.com/src-d/go-mysql-server/sql/expression"
	"github.com/src-d/go-mysql-server/sql/plan"
)

type Aliases map[string]sql.Expression

// optimizeJoins takes two-table InnerJoins where the join condition is an equality on an index of one of the tables,
// and replaces it with an equivalent IndexedJoin of the same two tables.
func optimizeJoins(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, ctx := ctx.Span("optimize_joins")
	defer span.Finish()

	a.Log("optimize_joins, node of type: %T", n)
	if !n.Resolved() {
		return n, nil
	}

	// skip certain queries (list is probably incomplete)
	switch n.(type) {
	case *plan.InsertInto, *plan.CreateIndex:
		return n, nil
	}

	numTables := 0
	plan.Inspect(n, func(node sql.Node) bool {
		switch node.(type) {
		case *plan.ResolvedTable:
			numTables++
		}
		return true
	})

	if numTables > 2 {
		a.Log("skipping join optimization, more than 2 tables")
		return n, nil
	}

	a.Log("finding indexes for joins")
	indexes, aliases, err := findJoinIndexes(ctx, a, n)
	if err != nil {
		return nil, err
	}

	a.Log("replacing LeftJoin,RightJoin,InnerJoin with IndexJoin")
	return transformJoins(a, n, indexes, aliases)
}

func transformJoins(a *Analyzer, n sql.Node, indexes map[string]sql.Index, aliases Aliases) (sql.Node, error) {

	var replacedIndexedJoin bool
	node, err := plan.TransformUp(n, func(node sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", node)
		switch node := node.(type) {
		case *plan.InnerJoin, *plan.LeftJoin, *plan.RightJoin:

			var cond sql.Expression
			var bnode plan.BinaryNode
			var joinType plan.JoinType

			switch node := node.(type) {
			case *plan.InnerJoin:
				cond = node.Cond
				bnode = node.BinaryNode
				joinType = plan.JoinTypeInner
			case *plan.LeftJoin:
				cond = node.Cond
				bnode = node.BinaryNode
				joinType = plan.JoinTypeLeft
			case *plan.RightJoin:
				cond = node.Cond
				bnode = node.BinaryNode
				joinType = plan.JoinTypeRight
			}

			primaryTable, secondaryTable, primaryTableExpr, secondaryTableIndex, err := analyzeJoinIndexes(bnode, cond, indexes, joinType)
			if err != nil {
				a.Log("Cannot apply index to join: %s", err.Error())
				return node, nil
			}

			joinSchema := append(primaryTable.Schema(), secondaryTable.Schema()...)
			joinCond, err := fixFieldIndexes(joinSchema, cond)
			if err != nil {
				return nil, err
			}
			replacedIndexedJoin = true

			secondaryTable, err = plan.TransformUp(secondaryTable, func(node sql.Node) (sql.Node, error) {
				a.Log("transforming node of type: %T", node)
				if rt, ok := node.(*plan.ResolvedTable); ok {
					return plan.NewIndexedTable(rt), nil
				}
				return node, nil
			})
			if err != nil {
				return nil, err
			}

			return plan.NewIndexedJoin(primaryTable, secondaryTable, joinType, joinCond, primaryTableExpr, secondaryTableIndex), nil
		default:
			return node, nil
		}
	})

	if err != nil {
		return nil, err
	}

	if replacedIndexedJoin {
		// Fix the field indexes as necessary
		node, err = plan.TransformUp(node, func(node sql.Node) (sql.Node, error) {
			// TODO: should we just do this for every query plan as a final part of the analysis?
			//  This would involve enforcing that every type of Node implement Expressioner.
			a.Log("transforming node of type: %T", node)
			return fixFieldIndexesForExpressions(node)
		})
	}

	return node, err
}

// Analyzes the join's tables and condition to select a left and right table, and an index to use for lookups in the
// right table. Returns an error if no suitable index can be found.
func analyzeJoinIndexes(
	node plan.BinaryNode,
	cond sql.Expression,
	indexes map[string]sql.Index,
	joinType plan.JoinType,
) (primary sql.Node, secondary sql.Node, primaryTableExpr []sql.Expression, secondaryTableIndex sql.Index, err error) {

	leftTableName := findTableName(node.Left)
	rightTableName := findTableName(node.Right)

	exprByTable := joinExprsByTable(splitConjunction(cond))
	if len(exprByTable) < 2 {
		return nil, nil, nil, nil, errors.New("couldn't determine suitable indexes to use for tables")
	}

	// Choose a primary and secondary table based on available indexes. We can't choose the left table as secondary for a
	// left join, or the right as secondary for a right join.
	if indexes[rightTableName] != nil && joinType != plan.JoinTypeRight {
		primaryTableExpr, err := fixFieldIndexesOnExpressions(node.Left.Schema(), exprByTable[leftTableName]...)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return node.Left, node.Right, primaryTableExpr, indexes[rightTableName], nil
	}

	if indexes[leftTableName] != nil && joinType != plan.JoinTypeLeft {
		primaryTableExpr, err := fixFieldIndexesOnExpressions(node.Right.Schema(), exprByTable[rightTableName]...)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return node.Right, node.Left, primaryTableExpr, indexes[leftTableName], nil
	}

	return nil, nil, nil, nil, errors.New("couldn't determine suitable indexes to use for tables")
}

// indexMatches returns whether the given index matches the given expression using the expression's string
// representation. Compare to logic in IndexRegistry.IndexByExpression
func indexMatches(index sql.Index, expr sql.Expression) bool {
	if len(index.Expressions()) != 1 {
		return false
	}

	indexExprStr := index.Expressions()[0]
	return indexExprStr == expr.String()
}

// Returns the underlying table name for the node given
func findTableName(node sql.Node) string {
	var tableName string
	plan.Inspect(node, func(node sql.Node) bool {
		switch node := node.(type) {
		case *plan.ResolvedTable:
			// TODO: this is over specific, we only need one side of the join to be indexable
			if it, ok := node.Table.(sql.IndexableTable); ok {
				tableName = it.Name()
				return false
			}
		}
		return true
	})

	return tableName
}

// Returns the table and field names from the expression given
func getTableNameFromExpression(expr sql.Expression) (tableName string, fieldName string) {
	switch expr := expr.(type) {
	case *expression.GetField:
		tableName = expr.Table()
		fieldName = expr.Name()
	}

	return tableName, fieldName
}

// index munging

// Assign indexes to the join conditions and returns the sql.Indexes assigned, as well as returning any aliases used by
// join conditions
func findJoinIndexes(ctx *sql.Context, a *Analyzer, node sql.Node) (map[string]sql.Index, Aliases, error) {
	a.Log("finding indexes, node of type: %T", node)

	indexSpan, _ := ctx.Span("find_join_indexes")
	defer indexSpan.Finish()

	var indexes map[string]sql.Index
	// release all unused indexes
	defer func() {
		if indexes == nil {
			return
		}

		for _, idx := range indexes {
			a.Catalog.ReleaseIndex(idx)
		}
	}()

	aliases := make(Aliases)
	var err error

	fn := func(ex sql.Expression) bool {
		if alias, ok := ex.(*expression.Alias); ok {
			if _, ok := aliases[alias.Name()]; !ok {
				aliases[alias.Name()] = alias.Child
			}
		}
		return true
	}

	plan.Inspect(node, func(node sql.Node) bool {
		switch node := node.(type) {
		case *plan.InnerJoin, *plan.LeftJoin, *plan.RightJoin:

			var cond sql.Expression
			switch node := node.(type) {
			case *plan.InnerJoin:
				cond = node.Cond
			case *plan.LeftJoin:
				cond = node.Cond
			case *plan.RightJoin:
				cond = node.Cond
			}

			fn(cond)

			var err error
			indexes, err = getJoinIndexes(cond, aliases, a)
			if err != nil {
				return false
			}
		}

		return true
	})

	return indexes, aliases, err
}

// Returns the left and right indexes for the two sides of the equality expression given.
func getJoinEqualityIndex(
		a *Analyzer,
		e *expression.Equals,
		aliases map[string]sql.Expression,
) (leftIdx sql.Index, rightIdx sql.Index) {

	// Only handle column expressions for these join indexes. Evaluable expression like `col=literal` will get pushed
	// down where possible.
	if isEvaluable(e.Left()) || isEvaluable(e.Right()) {
		return nil, nil
	}

	leftIdx, rightIdx =
			a.Catalog.IndexByExpression(a.Catalog.CurrentDatabase(), unifyExpressions(aliases, e.Left())...),
			a.Catalog.IndexByExpression(a.Catalog.CurrentDatabase(), unifyExpressions(aliases, e.Right())...)

	return leftIdx, rightIdx
}

func getJoinIndexes(e sql.Expression, aliases map[string]sql.Expression, a *Analyzer) (map[string]sql.Index, error) {

	switch e := e.(type) {
	case *expression.Equals:
		result := make(map[string]sql.Index)
		leftIdx, rightIdx := getJoinEqualityIndex(a, e, aliases)
		if leftIdx != nil {
			result[leftIdx.Table()] = leftIdx
		}
		if rightIdx != nil {
			result[rightIdx.Table()] = rightIdx
		}
		return result, nil
	case *expression.And:
		exprs := splitConjunction(e)
		for _, expr := range exprs {
			if _, ok := expr.(*expression.Equals); !ok {
				return nil, nil
			}
		}

		return getMultiColumnJoinIndex(exprs, a, aliases), nil
	}

	return nil, nil
}

func getMultiColumnJoinIndex(exprs []sql.Expression, a *Analyzer, aliases map[string]sql.Expression, ) map[string]sql.Index {
	result := make(map[string]sql.Index)

	exprsByTable := joinExprsByTable(exprs)
	for table, cols := range exprsByTable {
		idx := a.Catalog.IndexByExpression(a.Catalog.CurrentDatabase(), unifyExpressions(aliases, cols...)...)
		if idx != nil {
			result[table] = idx
		}
	}

	return result
}

func joinExprsByTable(exprs []sql.Expression) map[string][]sql.Expression {
	var result = make(map[string][]sql.Expression)

	for _, expr := range exprs {
		leftExpr, rightExpr := extractJoinColumnExpr(expr)
		if leftExpr == nil || rightExpr == nil {
			continue
		}

		result[leftExpr.col.Table()] = append(result[leftExpr.col.Table()], leftExpr.col)
		result[rightExpr.col.Table()] = append(result[rightExpr.col.Table()], rightExpr.col)
	}

	return result
}

// Extracts a pair of column expressions from a join condition, which must be an equality on two columns.
func extractJoinColumnExpr(e sql.Expression) (leftCol *columnExpr, rightCol *columnExpr) {
	switch e := e.(type) {
	case *expression.Equals:
		left, right := e.Left(), e.Right()
		if isEvaluable(left) || isEvaluable(right) {
			return nil, nil
		}

		leftCol, ok := left.(*expression.GetField)
		if !ok {
			return  nil, nil
		}

		rightCol, ok := right.(*expression.GetField)
		if !ok {
			return nil, nil
		}

		return &columnExpr{leftCol, right, e}, &columnExpr{rightCol, left, e}
	default:
		return nil, nil
	}
}