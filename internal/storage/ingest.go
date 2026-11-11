package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pridkett/simple_agent_tracer/internal/semantic"
)

var ErrCaptureChanged = errors.New("capture policy changed")

func (s *Store) Ingest(ctx context.Context, p Project, source string, spans []semantic.Span) error {
	if len(spans) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		exec := func(q string, args ...any) (sql.Result, error) { return tx.ExecContext(ctx, s.bind(q), args...) }
		var capture bool
		if err := tx.QueryRowContext(ctx, s.bind(`SELECT capture FROM projects WHERE id=?`), p.ID).Scan(&capture); err != nil {
			return err
		}
		if capture != p.Capture {
			return ErrCaptureChanged
		}
		if _, err := exec(`INSERT INTO sources(project,id) VALUES(?,?) ON CONFLICT DO NOTHING`, p.ID, source); err != nil {
			return err
		}
		changed := map[string]bool{}
		for _, span := range spans {
			raw, err := json.Marshal(span)
			if err != nil {
				return err
			}
			var previous string
			err = tx.QueryRowContext(ctx, s.bind(`SELECT payload FROM spans WHERE project=? AND trace=? AND id=?`), p.ID, span.Trace, span.ID).Scan(&previous)
			if err == nil {
				var old struct {
					Received int64 `json:"received,string"`
				}
				if err = json.Unmarshal([]byte(previous), &old); err != nil {
					return err
				}
				span.Received = old.Received
				raw, err = json.Marshal(span)
				if err != nil {
					return err
				}
				if !bytes.Equal(raw, []byte(previous)) {
					if _, err = exec(`UPDATE spans SET conflicts=conflicts+1 WHERE project=? AND trace=? AND id=?`, p.ID, span.Trace, span.ID); err != nil {
						return err
					}
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			latest := span.End
			if latest == 0 {
				latest = span.Received
			}
			if _, err = exec(`INSERT INTO traces(project,id,latest,received) VALUES(?,?,?,?)
     ON CONFLICT(project,id) DO UPDATE SET latest=CASE WHEN excluded.latest>traces.latest THEN excluded.latest ELSE traces.latest END,
     received=CASE WHEN excluded.received>traces.received THEN excluded.received ELSE traces.received END`, p.ID, span.Trace, latest, span.Received); err != nil {
				return err
			}
			var session any
			if span.Conversation != "" {
				session = identity(p.ID, source, span.Conversation)
				if _, err = exec(`INSERT INTO sessions(project,id,source,external) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, p.ID, session, source, clean(span.Conversation)); err != nil {
					return err
				}
			}
			usage, err := json.Marshal(span.Usage)
			if err != nil {
				return err
			}
			started := span.Start
			if started == 0 {
				started = span.Received
			}
			_, err = exec(`INSERT INTO spans(project,trace,id,parent,source,service,status,operation,request_model,response_model,name,tool,
    proxy_app,application,span_kind,own_session,own_turn,session_id,model_call,sort_time,activity,usage,payload)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, span.Trace, span.ID, span.Parent, source, clean(span.Service),
				span.Status, clean(span.Operation), clean(span.RequestModel), clean(span.ResponseModel), clean(span.Name), clean(span.Tool),
				clean(span.ProxyApp), clean(span.Application), span.Kind, session, clean(span.Turn), session, span.ModelCall, started, latest, string(usage), string(raw))
			if err != nil {
				return err
			}
			for _, group := range []struct {
				location string
				attrs    semantic.Attributes
			}{
				{"span", span.Attributes}, {"resource", span.Resource}, {"scope", span.Scope},
			} {
				for key, value := range group.attrs {
					if semantic.IsContentKey(key) {
						continue
					}
					data, err := json.Marshal(value)
					if err != nil {
						return err
					}
					if _, err = exec(`INSERT INTO span_metadata(project,trace,span,location,key,type,digest,value) VALUES(?,?,?,?,?,?,?,?)`,
						p.ID, span.Trace, span.ID, group.location, clean(key), MetadataType(data), MetadataDigest(data), string(data)); err != nil {
						return err
					}
				}
			}
			changed[span.Trace] = true
		}
		traces := make([]string, 0, len(changed))
		for trace := range changed {
			traces = append(traces, trace)
		}
		slices.Sort(traces)
		sessionIDs := map[string]bool{}
		for _, trace := range traces {
			if err := s.correlate(ctx, tx, p.ID, trace, sessionIDs); err != nil {
				return err
			}
		}
		return s.refreshSessionActivity(ctx, tx, p.ID, sessionIDs)
	})
}

func clean(value string) string { return strings.ReplaceAll(value, "\x00", "�") }

type member struct {
	id, parent, source, kind, op, app, ownTurn, turn             string
	external                                                     string
	own, session, previous                                       sql.NullString
	previousTurn                                                 string
	usageClient, previousUsageClient                             string
	background, previousBackground, proxyTurn, previousProxyTurn bool
	model, linked, previousLinked                                bool
}

func (s *Store) correlate(ctx context.Context, tx *sql.Tx, project, trace string, sessionIDs map[string]bool) error {
	rows, err := tx.QueryContext(ctx, s.bind(`SELECT m.id,m.parent,m.source,m.span_kind,m.operation,m.proxy_app,m.own_turn,m.own_session,
  m.session_id,m.turn_id,m.model_call,m.linked_model,m.usage_client,m.background,m.turn_from_proxy,COALESCE(s.external,'')
  FROM spans m LEFT JOIN sessions s ON s.project=m.project AND s.id=m.own_session WHERE m.project=? AND m.trace=?`), project, trace)
	if err != nil {
		return err
	}
	members := map[string]*member{}
	for rows.Next() {
		m := new(member)
		if err := rows.Scan(&m.id, &m.parent, &m.source, &m.kind, &m.op, &m.app, &m.ownTurn, &m.own, &m.previous, &m.previousTurn, &m.model, &m.previousLinked, &m.previousUsageClient, &m.previousBackground, &m.previousProxyTurn, &m.external); err != nil {
			rows.Close()
			return err
		}
		members[m.id] = m
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	exec := func(q string, args ...any) error { _, err := tx.ExecContext(ctx, s.bind(q), args...); return err }
	proxyIDs := map[string]bool{}
	prior, err := tx.QueryContext(ctx, s.bind(`SELECT proxy_session FROM session_claims WHERE project=? AND trace=?`), project, trace)
	if err != nil {
		return err
	}
	for prior.Next() {
		var id string
		if err := prior.Scan(&id); err != nil {
			prior.Close()
			return err
		}
		proxyIDs[id] = true
	}
	if err := prior.Close(); err != nil {
		return err
	}
	if err := prior.Err(); err != nil {
		return err
	}
	if len(proxyIDs) > 0 {
		if err := exec(`DELETE FROM session_claims WHERE project=? AND trace=?`, project, trace); err != nil {
			return err
		}
	}
	claims := map[[2]string]bool{}
	pairedModels := map[string][]*member{}
	for _, m := range members {
		m.session = m.own
		m.background = semantic.IsBackground(m.op)
		var proxyServer *member
		var client *member
		for ancestor, steps := m, 0; ancestor != nil && steps < len(members); ancestor, steps = members[ancestor.parent], steps+1 {
			if m.session.Valid == false && ancestor.source == m.source && ancestor.own.Valid {
				m.session = ancestor.own
			}
			if m.turn == "" && ancestor.source == m.source && ancestor.op == "invoke_agent" && ancestor.ownTurn != "" {
				m.turn = identity(ancestor.source, ancestor.own.String, ancestor.ownTurn)
			}
			if ancestor.app != "" && ancestor.kind == "SPAN_KIND_SERVER" && ancestor.own.Valid {
				parent := members[ancestor.parent]
				if parent != nil && parent.source != ancestor.source && parent.kind == "SPAN_KIND_CLIENT" && parent.app == "" && parent.own.Valid &&
					ancestor.external == fmt.Sprintf("app:%s:%x", ancestor.app, sha256.Sum256([]byte(parent.external))) {
					proxyServer, client = ancestor, parent
				}
			}
		}
		if proxyServer == nil || m.source != proxyServer.source || (m.own.Valid && m.own != proxyServer.own) || (m.app != "" && m.app != proxyServer.app) {
			continue
		}
		claims[[2]string{proxyServer.own.String, client.own.String}] = true
		m.background = m.background || semantic.IsBackground(client.op)
		for ancestor, steps := client, 0; ancestor != nil && ancestor.source == client.source && steps < len(members); ancestor, steps = members[ancestor.parent], steps+1 {
			if ancestor.op == "invoke_agent" && ancestor.ownTurn != "" && ancestor.own == client.own {
				m.turn = identity(ancestor.source, ancestor.own.String, ancestor.ownTurn)
				m.proxyTurn = true
				break
			}
		}
		if m.model {
			client.linked = client.model
			pairedModels[client.id] = append(pairedModels[client.id], m)
		}
	}
	for id, models := range pairedModels {
		if len(models) == 1 && members[id].model {
			models[0].usageClient = id
		}
	}
	for pair := range claims {
		proxyIDs[pair[0]] = true
		if err := exec(`INSERT INTO session_claims(project,trace,proxy_session,agent_session) VALUES(?,?,?,?)`, project, trace, pair[0], pair[1]); err != nil {
			return err
		}
	}
	for _, m := range members {
		if m.session.Valid {
			sessionIDs[m.session.String] = true
		}
		if m.previous.Valid {
			sessionIDs[m.previous.String] = true
		}
		if m.session != m.previous || m.turn != m.previousTurn || m.linked != m.previousLinked || m.usageClient != m.previousUsageClient || m.background != m.previousBackground || m.proxyTurn != m.previousProxyTurn {
			if err := exec(`UPDATE spans SET session_id=?,turn_id=?,linked_model=?,usage_client=?,background=?,turn_from_proxy=? WHERE project=? AND trace=? AND id=?`,
				m.session, m.turn, m.linked, m.usageClient, m.background, m.proxyTurn, project, trace, m.id); err != nil {
				return err
			}
		}
	}
	for id := range proxyIDs {
		if err := s.refreshAlias(ctx, tx, project, id); err != nil {
			return err
		}
	}
	return nil
}
