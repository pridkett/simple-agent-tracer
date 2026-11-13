package query

import (
	"slices"
	"strconv"
	"strings"

	"github.com/xwb1989/sqlparser"
)

type groupOrder struct {
	column string
	desc   bool
}

func parseModelSQL(q ModelQuery) (ModelQuery, []string, []groupOrder, error) {
	if len(q.SQL) > 8192 {
		return q, nil, nil, invalidQuery("sql exceeds 8192 bytes")
	}
	statement, err := sqlparser.ParseStrictDDL(q.SQL)
	s, ok := statement.(*sqlparser.Select)
	if err != nil || !ok || len(s.From) != 1 || s.Distinct != "" || s.Having != nil || s.Lock != "" || s.Cache != "" || s.Hints != "" {
		return q, nil, nil, invalidQuery("expected a simple SELECT from model_calls")
	}
	table, ok := s.From[0].(*sqlparser.AliasedTableExpr)
	if !ok || !table.As.IsEmpty() || table.Hints != nil || len(table.Partitions) != 0 {
		return q, nil, nil, invalidQuery("expected FROM model_calls")
	}
	name, ok := table.Expr.(sqlparser.TableName)
	if !ok || !name.Qualifier.IsEmpty() || !strings.EqualFold(name.Name.String(), "model_calls") {
		return q, nil, nil, invalidQuery("expected FROM model_calls")
	}
	var aliases []string
	for _, selection := range s.SelectExprs {
		switch expr := selection.(type) {
		case *sqlparser.StarExpr:
			if len(s.SelectExprs) != 1 || !expr.TableName.IsEmpty() || len(s.GroupBy) != 0 {
				return q, nil, nil, invalidQuery("use an explicit column list for this SELECT")
			}
		case *sqlparser.AliasedExpr:
			column := sqlColumn(expr.Expr)
			if column == "" {
				column = sqlAggregate(expr.Expr)
			}
			if column == "" {
				return q, nil, nil, invalidQuery("unsupported SELECT expression")
			}
			alias := expr.As.String()
			if alias == "" {
				alias = column
			}
			q.Select = append(q.Select, column)
			aliases = append(aliases, alias)
		default:
			return q, nil, nil, invalidQuery("unsupported SELECT expression")
		}
	}
	for _, expr := range s.GroupBy {
		q.GroupBy = append(q.GroupBy, sqlColumn(expr))
	}
	if s.Where != nil {
		if err := sqlWhere(&q, s.Where.Expr); err != nil {
			return q, nil, nil, err
		}
	}
	if s.Limit != nil {
		value, ok := s.Limit.Rowcount.(*sqlparser.SQLVal)
		if !ok || value.Type != sqlparser.IntVal || s.Limit.Offset != nil || q.Limit != nil {
			return q, nil, nil, invalidQuery("specify a single integer LIMIT without OFFSET")
		}
		limit, err := strconv.Atoi(string(value.Val))
		if err != nil || limit < 1 {
			return q, nil, nil, invalidQuery("invalid SQL LIMIT")
		}
		q.Limit = &limit
	}
	aggregate := len(q.GroupBy) != 0
	for _, column := range q.Select {
		aggregate = aggregate || slices.Contains(aggregateColumns, column) || column == "cost_total"
	}
	var order []groupOrder
	for _, expr := range s.OrderBy {
		column := sqlColumn(expr.Expr)
		if !aggregate {
			if len(s.OrderBy) != 1 || column != "started_at" || expr.Direction != sqlparser.DescScr {
				return q, nil, nil, invalidQuery("row queries support only ORDER BY started_at DESC")
			}
			continue
		}
		for i, alias := range aliases {
			if strings.EqualFold(alias, column) {
				column = q.Select[i]
				break
			}
		}
		switch {
		case slices.Contains(q.GroupBy, column):
		case slices.Contains(q.Select, column) && slices.Contains([]string{"calls", "cost_total", "known_cost_calls", "p95_duration_ms",
			"known_duration_calls", "p95_time_to_first_chunk_ms", "known_first_chunk_calls"}, column):
		default:
			return q, nil, nil, invalidQuery("ORDER BY requires a group column or selected scalar aggregate")
		}
		order = append(order, groupOrder{column, expr.Direction == sqlparser.DescScr})
	}
	return q, aliases, order, nil
}

func sqlColumn(expr sqlparser.Expr) string {
	if c, ok := expr.(*sqlparser.ColName); ok && c.Qualifier.IsEmpty() {
		return strings.ToLower(c.Name.String())
	}
	return ""
}

func sqlAggregate(expr sqlparser.Expr) string {
	fn, ok := expr.(*sqlparser.FuncExpr)
	if !ok || !fn.Qualifier.IsEmpty() || fn.Distinct || len(fn.Exprs) != 1 {
		return ""
	}
	if star, ok := fn.Exprs[0].(*sqlparser.StarExpr); ok && star.TableName.IsEmpty() && fn.Name.EqualString("count") {
		return "calls"
	}
	arg, ok := fn.Exprs[0].(*sqlparser.AliasedExpr)
	if !ok || !arg.As.IsEmpty() {
		return ""
	}
	column := sqlColumn(arg.Expr)
	switch {
	case fn.Name.EqualString("sum") && column == "cost_amount":
		return "cost_total"
	case fn.Name.EqualString("count") && column == "cost_amount":
		return "known_cost_calls"
	case fn.Name.EqualString("count") && column == "duration_ms":
		return "known_duration_calls"
	case fn.Name.EqualString("count") && column == "time_to_first_chunk_ms":
		return "known_first_chunk_calls"
	case fn.Name.EqualString("p95") && column == "duration_ms":
		return "p95_duration_ms"
	case fn.Name.EqualString("p95") && column == "time_to_first_chunk_ms":
		return "p95_time_to_first_chunk_ms"
	}
	return ""
}

func sqlWhere(q *ModelQuery, root sqlparser.Expr) error {
	expressions := []sqlparser.Expr{root}
	for len(expressions) != 0 {
		expr := expressions[len(expressions)-1]
		expressions = expressions[:len(expressions)-1]
		switch value := expr.(type) {
		case *sqlparser.ParenExpr:
			expressions = append(expressions, value.Expr)
		case *sqlparser.AndExpr:
			expressions = append(expressions, value.Right, value.Left)
		case *sqlparser.ComparisonExpr:
			if err := sqlComparison(q, value); err != nil {
				return err
			}
		default:
			return invalidQuery("WHERE supports comparisons joined by AND")
		}
	}
	return nil
}

func sqlComparison(q *ModelQuery, comparison *sqlparser.ComparisonExpr) error {
	column := sqlColumn(comparison.Left)
	value, literal := comparison.Right.(*sqlparser.SQLVal)
	f := &q.Filters
	var target *string
	switch column {
	case "started_at":
		switch comparison.Operator {
		case ">=":
			target = &q.From
		case "<":
			target = &q.To
		}
	case "duration_ms":
		if comparison.Operator == ">=" && literal && (value.Type == sqlparser.IntVal || value.Type == sqlparser.FloatVal) && f.MinDurationMs == nil {
			if ms, err := strconv.ParseFloat(string(value.Val), 64); err == nil {
				f.MinDurationMs = &ms
				return nil
			}
		}
	case "has_output":
		if yes, ok := comparison.Right.(sqlparser.BoolVal); ok && comparison.Operator == "=" && f.HasOutput == nil {
			b := bool(yes)
			f.HasOutput = &b
			return nil
		}
	default:
		if comparison.Operator == "=" {
			target = map[string]*string{
				"source": &f.Source, "service": &f.Service, "session_id": &f.Session, "trace_id": &f.Trace,
				"model": &f.Model, "requested_model": &f.RequestedModel, "served_model": &f.ServedModel,
				"application": &f.Application, "provider": &f.Provider, "operation": &f.Operation,
				"status": &f.Status, "currency": &f.Currency,
			}[column]
		}
	}
	if target == nil || *target != "" || !literal || value.Type != sqlparser.StrVal || len(value.Val) == 0 || comparison.Escape != nil {
		return invalidQuery("unsupported or repeated WHERE comparison for " + column)
	}
	*target = string(value.Val)
	return nil
}
