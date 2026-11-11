package storage

import (
	"context"
	"database/sql"
	"time"
)

const Grace = 5 * time.Minute
const SweepBatch = 200

// A kept session protects traces under its original ID and any current resolved alias.
const protected = `EXISTS (
 SELECT 1 FROM spans m
 JOIN sessions kept_session ON kept_session.project=m.project AND kept_session.kept=TRUE
 LEFT JOIN session_aliases a ON a.project=m.project AND a.proxy_session=m.session_id
 WHERE m.project=t.project AND m.trace=t.id AND kept_session.id IN (m.session_id,a.agent_session)
 UNION ALL
 SELECT 1 FROM spans m
 JOIN session_aliases a ON a.project=m.project AND a.agent_session=m.session_id
 JOIN sessions kept_session ON kept_session.project=a.project AND kept_session.id=a.proxy_session AND kept_session.kept=TRUE
 WHERE m.project=t.project AND m.trace=t.id
)`

func (s *Store) Keep(ctx context.Context, project string, traces, sessions []string, keep bool) error {
	if len(traces)+len(sessions) == 0 || len(traces)+len(sessions) > 200 {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, group := range []struct {
			table string
			ids   []string
		}{{"traces", traces}, {"sessions", sessions}} {
			for _, id := range group.ids {
				result, err := tx.ExecContext(ctx, s.bind("UPDATE "+group.table+" SET kept=? WHERE project=? AND id=?"), keep, project, id)
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if n != 1 {
					return ErrInvalid
				}
			}
		}
		return nil
	})
}

func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		// Fixed units avoid timezone and DST differences between databases.
		rows, err := tx.QueryContext(ctx, s.bind(`SELECT t.project,t.id FROM traces t JOIN projects p ON p.id=t.project
   WHERE t.kept=FALSE AND t.latest < ? - CAST(p.retention_days AS BIGINT)*86400000000000
    AND t.received < ? AND NOT `+protected+` ORDER BY t.latest,t.project,t.id LIMIT ?`),
			now.UnixNano(), now.Add(-Grace).UnixNano(), SweepBatch)
		if err != nil {
			return err
		}
		var ids [][2]string
		for rows.Next() {
			var pair [2]string
			if err := rows.Scan(&pair[0], &pair[1]); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, pair)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		affected := map[string]map[string]bool{}
		for _, pair := range ids {
			if affected[pair[0]] == nil {
				affected[pair[0]] = map[string]bool{}
			}
			members, err := tx.QueryContext(ctx, s.bind(`SELECT DISTINCT session_id FROM spans WHERE project=? AND trace=? AND session_id IS NOT NULL`), pair[0], pair[1])
			if err != nil {
				return err
			}
			for members.Next() {
				var id string
				if err = members.Scan(&id); err != nil {
					members.Close()
					return err
				}
				affected[pair[0]][id] = true
			}
			if err = members.Close(); err != nil {
				return err
			}
			if err = members.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM traces WHERE project=? AND id=?`), pair[0], pair[1]); err != nil {
				return err
			}
		}
		for project, sessionIDs := range affected {
			if err := s.refreshSessionActivity(ctx, tx, project, sessionIDs); err != nil {
				return err
			}
		}
		count = int64(len(ids))
		// Preserve historical evidence while either side has activity or a Keep flag.
		obsolete, err := tx.QueryContext(ctx, `DELETE FROM session_claims WHERE
    NOT EXISTS(SELECT 1 FROM spans m WHERE m.project=session_claims.project AND m.session_id IN(session_claims.proxy_session,session_claims.agent_session))
    AND NOT EXISTS(SELECT 1 FROM sessions se WHERE se.project=session_claims.project AND se.kept=TRUE AND se.id IN(session_claims.proxy_session,session_claims.agent_session))
    RETURNING project,proxy_session`)
		if err != nil {
			return err
		}
		removed := map[[2]string]bool{}
		for obsolete.Next() {
			var pair [2]string
			if err := obsolete.Scan(&pair[0], &pair[1]); err != nil {
				obsolete.Close()
				return err
			}
			removed[pair] = true
		}
		if err := obsolete.Close(); err != nil {
			return err
		}
		if err := obsolete.Err(); err != nil {
			return err
		}
		for pair := range removed {
			if err := s.refreshAlias(ctx, tx, pair[0], pair[1]); err != nil {
				return err
			}
		}

		// Retain an old proxy URL while its resolved session still has traces.
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE kept=FALSE
    AND NOT EXISTS(SELECT 1 FROM spans m WHERE m.project=sessions.project AND (m.own_session=sessions.id OR m.session_id=sessions.id))
    AND NOT EXISTS(SELECT 1 FROM session_claims c WHERE c.project=sessions.project AND sessions.id IN(c.proxy_session,c.agent_session))`)
		return err
	})
	return count, err
}
