package storage

import (
	"context"
	"strings"
)

// ModelCall contains the canonical span and the usage from its linked client, if any.
type ModelCall struct {
	Position
	Session, Turn, Fallback, Payload string
	Background                       bool
}

func (s *Store) ModelCalls(ctx context.Context, project string, f Filter, application, requestedModel, servedModel string, after *Position, limit int, visit func(ModelCall) (bool, error)) error {
	conditions, childArgs := childFilter(f)
	var q strings.Builder
	q.WriteString(`SELECT m.sort_time,m.trace,m.id,COALESCE(a.agent_session,m.session_id,''),
 m.turn_id,m.background,COALESCE(c.usage,''),m.payload
 FROM spans m
 LEFT JOIN spans c ON c.project=m.project AND c.trace=m.trace AND c.id=m.usage_client
 LEFT JOIN session_aliases a ON a.project=m.project AND a.proxy_session=m.session_id
 WHERE m.project=? AND m.model_call=TRUE AND m.linked_model=FALSE AND m.sort_time>=? AND m.sort_time<?`)
	q.WriteString(conditions)
	args := append([]any{project, *f.From, *f.To}, childArgs...)
	for _, field := range []struct{ column, value string }{
		{"m.trace", f.Trace}, {"m.application", application}, {"m.request_model", requestedModel}, {"m.response_model", servedModel},
	} {
		if field.value != "" {
			q.WriteString(" AND " + field.column + "=?")
			args = append(args, field.value)
		}
	}
	if after != nil {
		q.WriteString(" AND (m.sort_time,m.trace,m.id)<(?,?,?)")
		args = append(args, after.Time, after.Trace, after.ID)
	}
	q.WriteString(" ORDER BY m.sort_time DESC,m.trace DESC,m.id DESC LIMIT ?")
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.bind(q.String()), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var call ModelCall
		if err := rows.Scan(&call.Time, &call.Trace, &call.ID, &call.Session, &call.Turn, &call.Background, &call.Fallback, &call.Payload); err != nil {
			return err
		}
		more, err := visit(call)
		if err != nil || !more {
			return err
		}
	}
	return rows.Err()
}
