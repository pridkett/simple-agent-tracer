package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

type Filter struct {
	From, To                                                        *int64
	Source, Service, Session, Operation, Model, Status, Tool, Trace string
	MetadataKey, MetadataType, MetadataDigest                       string
}

type Position struct {
	Time  int64  `json:"time,string"`
	Trace string `json:"trace,omitempty"`
	ID    string `json:"id"`
}

type Entry struct {
	Number    int    `json:"-"`
	ID        string `json:"id"`
	Time      int64  `json:"-"`
	Received  int64  `json:"-"`
	External  string `json:"externalId,omitempty"`
	Source    string `json:"source,omitempty"`
	Kept      bool   `json:"kept"`
	Protected bool   `json:"protectedBySession,omitempty"`
	Conflict  bool   `json:"sessionConflict,omitempty"`
}

type SpanRow struct {
	Position
	Parent      string          `json:"-"`
	Session     string          `json:"sessionId,omitempty"`
	Turn        string          `json:"turnId,omitempty"`
	Conflicts   int64           `json:"conflictingExports,omitempty"`
	Linked      bool            `json:"linkedModel,omitempty"`
	Background  bool            `json:"background,omitempty"`
	UsageClient string          `json:"usageFallbackSpanId,omitempty"`
	Data        json.RawMessage `json:"data"`
}

func childFilter(f Filter) (string, []any) {
	var q strings.Builder
	args := []any{}
	for _, pair := range []struct{ value, condition string }{
		{f.Source, "m.source=?"}, {f.Service, "m.service=?"}, {f.Operation, "m.operation=?"}, {f.Status, "m.status=?"},
		{f.Model, "(m.request_model=? OR m.response_model=?)"}, {f.Tool, "(m.tool=? OR m.name=?)"},
	} {
		if pair.value == "" {
			continue
		}
		q.WriteString(" AND " + pair.condition)
		args = append(args, pair.value)
		if strings.Count(pair.condition, "?") == 2 {
			args = append(args, pair.value)
		}
	}
	if f.Session != "" {
		q.WriteString(` AND (m.session_id=? OR EXISTS(
   SELECT 1 FROM sessions sx LEFT JOIN session_aliases ax ON ax.project=sx.project AND ax.proxy_session=sx.id
   WHERE sx.project=m.project AND (sx.id=? OR sx.external=?) AND COALESCE(ax.agent_session,sx.id)=
    COALESCE((SELECT ay.agent_session FROM session_aliases ay WHERE ay.project=m.project AND ay.proxy_session=m.session_id),m.session_id))
   OR EXISTS(SELECT 1 FROM span_metadata sm WHERE sm.project=m.project AND sm.trace=m.trace AND sm.span=m.id
    AND sm.key IN ('gen_ai.conversation.id','session.id','langfuse.session.id') AND sm.type='string' AND sm.digest=?))`)
		raw, _ := json.Marshal(f.Session)
		args = append(args, f.Session, f.Session, f.Session, MetadataDigest(raw))
	}
	if f.MetadataKey != "" {
		q.WriteString(` AND EXISTS(SELECT 1 FROM span_metadata sm WHERE sm.project=m.project AND sm.trace=m.trace AND sm.span=m.id AND sm.key=?`)
		args = append(args, f.MetadataKey)
		if f.MetadataType != "" {
			q.WriteString(" AND sm.type=? AND sm.digest=?")
			args = append(args, f.MetadataType, f.MetadataDigest)
		}
		q.WriteByte(')')
	}
	return q.String(), args
}

const sessionLatest = `COALESCE((SELECT MAX(activity) FROM (
 SELECT e.latest AS activity
 UNION ALL
 SELECT proxy.latest FROM session_aliases a JOIN sessions proxy ON proxy.project=a.project AND proxy.id=a.proxy_session
 WHERE a.project=e.project AND a.agent_session=e.id
) activity_rows),0)`

const sessionSpans = `m.session_id IN (
 SELECT ? UNION ALL SELECT a.proxy_session FROM session_aliases a WHERE a.project=? AND a.agent_session=?)`

func (s *Store) Entries(ctx context.Context, project, kind string, f Filter, after *Position, limit int) ([]Entry, error) {
	conditions, childArgs := childFilter(f)
	args := []any{project}
	var q string
	if kind == "traces" {
		q = `SELECT e.id,e.latest,e.received,'','',e.kept FROM traces e WHERE e.project=?`
		if conditions != "" {
			q += " AND EXISTS (SELECT 1 FROM spans m WHERE m.project=e.project AND m.trace=e.id" + conditions + ")"
			args = append(args, childArgs...)
		}
	} else {
		q = `SELECT e.id,` + sessionLatest + ` AS latest,
     CAST(0 AS BIGINT),e.external,e.source,(e.kept OR EXISTS(
      SELECT 1 FROM session_aliases a JOIN sessions ps ON ps.project=a.project AND ps.id=a.proxy_session
      WHERE a.project=e.project AND a.agent_session=e.id AND ps.kept=TRUE))
     FROM sessions e WHERE e.project=? AND NOT EXISTS(SELECT 1 FROM session_aliases a WHERE a.project=e.project AND a.proxy_session=e.id)`
		if conditions != "" || f.Trace != "" {
			matchArgs := append([]any{}, childArgs...)
			if f.Trace != "" {
				conditions += " AND m.trace=?"
				matchArgs = append(matchArgs, f.Trace)
			}
			q += ` AND (EXISTS(SELECT 1 FROM spans m WHERE m.project=e.project AND m.session_id=e.id` + conditions + `)
    OR EXISTS(SELECT 1 FROM session_aliases a CROSS JOIN spans m
      WHERE a.project=e.project AND a.agent_session=e.id AND m.project=a.project AND m.session_id=a.proxy_session` + conditions + "))"
			args = append(args, matchArgs...)
			args = append(args, matchArgs...)
		}
	}
	q = "SELECT * FROM (" + q + ") results WHERE 1=1"
	if f.From != nil {
		q += " AND latest>=?"
		args = append(args, *f.From)
	}
	if f.To != nil {
		q += " AND latest<?"
		args = append(args, *f.To)
	}
	if kind == "traces" && f.Trace != "" {
		q += " AND id=?"
		args = append(args, f.Trace)
	}
	if after != nil {
		q += " AND (latest<? OR (latest=? AND id<?))"
		args = append(args, after.Time, after.Time, after.ID)
	}
	q += " ORDER BY latest DESC,id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.bind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Time, &e.Received, &e.External, &e.Source, &e.Kept); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Entry(ctx context.Context, project, kind, id string) (Entry, error) {
	var e Entry
	var err error
	if kind == "traces" {
		err = s.db.QueryRowContext(ctx, s.bind(`SELECT t.id,t.latest,t.received,t.kept,`+protected+`,
    ((SELECT COUNT(DISTINCT COALESCE(a.agent_session,m.session_id)) FROM spans m LEFT JOIN session_aliases a
      ON a.project=m.project AND a.proxy_session=m.session_id WHERE m.project=t.project AND m.trace=t.id)>1 OR
    EXISTS(SELECT 1 FROM session_claims c WHERE c.project=t.project AND c.trace=t.id AND NOT EXISTS(
      SELECT 1 FROM session_aliases a WHERE a.project=c.project AND a.proxy_session=c.proxy_session)))
   FROM traces t WHERE t.project=? AND t.id=?`), project, id).
			Scan(&e.ID, &e.Time, &e.Received, &e.Kept, &e.Protected, &e.Conflict)
	} else {
		err = s.db.QueryRowContext(ctx, s.bind(`SELECT e.id,e.external,e.source,e.kept,
    `+sessionLatest+`,
    EXISTS(SELECT 1 FROM session_aliases a JOIN sessions ps ON ps.project=a.project AND ps.id=a.proxy_session
     WHERE a.project=e.project AND a.agent_session=e.id AND ps.kept=TRUE),
    EXISTS(SELECT 1 FROM session_claims c WHERE c.project=e.project AND e.id IN(c.agent_session,c.proxy_session) AND NOT EXISTS(
      SELECT 1 FROM session_aliases a WHERE a.project=c.project AND a.proxy_session=c.proxy_session))
   FROM sessions original LEFT JOIN session_aliases resolved ON resolved.project=original.project AND resolved.proxy_session=original.id
    JOIN sessions e ON e.project=original.project AND e.id=COALESCE(resolved.agent_session,original.id)
   WHERE original.project=? AND original.id=?`), project, id).
			Scan(&e.ID, &e.External, &e.Source, &e.Kept, &e.Time, &e.Protected, &e.Conflict)
	}
	return e, err
}

const visibleTurn = `(m.turn_from_proxy=FALSE OR EXISTS(
 SELECT 1 FROM session_aliases va WHERE va.project=m.project AND va.proxy_session=m.session_id))`

const spanRowSelect = `SELECT m.sort_time,m.trace,m.id,m.parent,COALESCE(m.session_id,''),CASE WHEN ` + visibleTurn + ` THEN m.turn_id ELSE '' END,m.conflicts,m.linked_model,m.background,m.usage_client,m.payload FROM spans m WHERE m.project=?`

// SpanParents returns stored direct parents outside the window, within the same project and trace.
func (s *Store) SpanParents(ctx context.Context, project string, window []SpanRow) ([]SpanRow, error) {
	type key struct{ trace, span string }
	seen := make(map[key]bool, len(window))
	for _, row := range window {
		seen[key{row.Trace, row.ID}] = true
	}
	var pairs []string
	args := []any{project}
	for _, row := range window {
		parent := key{row.Trace, row.Parent}
		if parent.span != "" && !seen[parent] {
			seen[parent] = true
			pairs = append(pairs, "(?,?)")
			args = append(args, parent.trace, parent.span)
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	q := spanRowSelect + " AND (m.trace,m.id) IN (VALUES " + strings.Join(pairs, ",") + ")"
	return s.readSpanRows(ctx, q, args)
}

func (s *Store) SpanRows(ctx context.Context, project, kind, id, turn, span string, after *Position, limit int) ([]SpanRow, error) {
	return s.spanRows(ctx, project, kind, id, turn, spanSelection{span: span, cursor: after, limit: limit})
}

func (s *Store) PrecedingModel(ctx context.Context, project, id, turn string, before *Position) ([]SpanRow, error) {
	return s.spanRows(ctx, project, "sessions", id, turn, spanSelection{cursor: before, limit: 1, reverse: true, modelOnly: true})
}

// FocusedSpanRows returns a window around a span in the given timeline.
// The extra row lets the handler determine whether another page follows.
func (s *Store) FocusedSpanRows(ctx context.Context, project, kind, id, turn, trace, span string, limit int) ([]SpanRow, error) {
	target, err := s.spanRows(ctx, project, kind, id, turn, spanSelection{trace: trace, span: span, limit: 1})
	if err != nil {
		return nil, err
	}
	if len(target) == 0 {
		return nil, sql.ErrNoRows
	}
	position := &target[0].Position
	var before []SpanRow
	if contextSize := min(8, limit/3); contextSize > 0 {
		before, err = s.spanRows(ctx, project, kind, id, turn, spanSelection{cursor: position, limit: contextSize, reverse: true})
		if err != nil {
			return nil, err
		}
		slices.Reverse(before)
	}
	after, err := s.SpanRows(ctx, project, kind, id, turn, "", position, limit-len(before))
	if err != nil {
		return nil, err
	}
	return append(append(before, target[0]), after...), nil
}

type spanSelection struct {
	trace, span        string
	cursor             *Position
	limit              int
	reverse, modelOnly bool
}

func (s *Store) spanRows(ctx context.Context, project, kind, id, turn string, selection spanSelection) ([]SpanRow, error) {
	q := spanRowSelect
	args := []any{project, id}
	if kind == "traces" {
		q += " AND m.trace=?"
	} else {
		q += " AND " + sessionSpans
		args = append(args, project, id)
	}
	if turn != "" {
		q += " AND " + visibleTurn + " AND m.turn_id=?"
		args = append(args, turn)
	}
	if selection.trace != "" {
		q += " AND m.trace=?"
		args = append(args, selection.trace)
	}
	if selection.span != "" {
		q += " AND m.id=?"
		args = append(args, selection.span)
	}
	operator, order := ">", "ASC"
	if selection.modelOnly {
		q += " AND m.model_call=TRUE AND m.linked_model=FALSE AND m.background=FALSE"
	}
	if selection.reverse {
		operator, order = "<", "DESC"
	}
	if cursor := selection.cursor; cursor != nil {
		q += " AND (m.sort_time,m.trace,m.id)" + operator + "(?,?,?)"
		args = append(args, cursor.Time, cursor.Trace, cursor.ID)
	}
	q += " ORDER BY m.sort_time " + order + ",m.trace " + order + ",m.id " + order + " LIMIT ?"
	args = append(args, selection.limit)
	return s.readSpanRows(ctx, q, args)
}

func (s *Store) readSpanRows(ctx context.Context, q string, args []any) ([]SpanRow, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpanRow{}
	for rows.Next() {
		var r SpanRow
		var raw string
		if err := rows.Scan(&r.Time, &r.Trace, &r.ID, &r.Parent, &r.Session, &r.Turn, &r.Conflicts, &r.Linked, &r.Background, &r.UsageClient, &raw); err != nil {
			return nil, err
		}
		r.Data = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

type Observation struct {
	Source, Application, Status string
	Model, Linked               bool
	Usage                       json.RawMessage
	Fallback                    json.RawMessage
}

func (s *Store) Observations(ctx context.Context, project, kind, id string, visit func(Observation) error) error {
	q := `SELECT m.source,m.application,m.status,m.model_call,m.linked_model,m.usage,COALESCE(c.usage,'') FROM spans m
 LEFT JOIN spans c ON c.project=m.project AND c.trace=m.trace AND c.id=m.usage_client WHERE m.project=?`
	args := []any{project, id}
	if kind == "traces" {
		q += " AND m.trace=?"
	} else {
		q += " AND " + sessionSpans
		args = append(args, project, id)
	}
	rows, err := s.db.QueryContext(ctx, s.bind(q), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o Observation
		var raw, fallback string
		if err := rows.Scan(&o.Source, &o.Application, &o.Status, &o.Model, &o.Linked, &raw, &fallback); err != nil {
			return err
		}
		o.Usage = json.RawMessage(raw)
		o.Fallback = json.RawMessage(fallback)
		if err := visit(o); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) Turns(ctx context.Context, project, id string, after *Position, limit int) ([]Entry, error) {
	q := `SELECT m.turn_id AS id,MIN(m.sort_time) AS started FROM spans m WHERE m.project=? AND m.turn_id<>'' AND ` + visibleTurn + `
  AND ` + sessionSpans + `
  GROUP BY m.turn_id`
	args := []any{project, id, project, id}
	q = "SELECT * FROM (SELECT id,started,ROW_NUMBER() OVER(ORDER BY started,id) FROM (" + q + ") grouped) turns"
	if after != nil {
		q += " WHERE (started,id)>(?,?)"
		args = append(args, after.Time, after.ID)
	}
	q += " ORDER BY started,id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.bind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Time, &e.Number); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type MetadataValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	JSON  string          `json:"json"`
}

func (s *Store) Metadata(ctx context.Context, project, key string, limit int) (any, error) {
	var rows *sql.Rows
	var err error
	if key == "" {
		rows, err = s.db.QueryContext(ctx, s.bind(`SELECT DISTINCT key FROM span_metadata WHERE project=? ORDER BY key LIMIT ?`), project, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, s.bind(`SELECT DISTINCT type,value FROM span_metadata WHERE project=? AND key=? AND LENGTH(value)<=4096 ORDER BY type,value LIMIT ?`), project, key, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if key == "" {
		out := []string{}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	}
	out := []MetadataValue{}
	for rows.Next() {
		var v MetadataValue
		var raw string
		if err := rows.Scan(&v.Type, &raw); err != nil {
			return nil, err
		}
		v.Value = json.RawMessage(raw)
		v.JSON = raw
		out = append(out, v)
	}
	return out, rows.Err()
}
