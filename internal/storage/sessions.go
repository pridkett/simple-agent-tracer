package storage

import (
	"context"
	"database/sql"
)

func (s *Store) refreshSessionActivity(ctx context.Context, tx *sql.Tx, project string, ids map[string]bool) error {
	for id := range ids {
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE sessions SET latest=COALESCE((
    SELECT MAX(m.activity) FROM spans m WHERE m.project=? AND m.session_id=?),0)
    WHERE project=? AND id=?`), project, id, project, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) refreshAlias(ctx context.Context, tx *sql.Tx, project, proxy string) error {
	if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM session_aliases WHERE project=? AND proxy_session=?`), project, proxy); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, s.bind(`INSERT INTO session_aliases(project,proxy_session,agent_session)
  SELECT project,proxy_session,MIN(agent_session) FROM session_claims WHERE project=? AND proxy_session=?
  GROUP BY project,proxy_session HAVING COUNT(DISTINCT agent_session)=1`), project, proxy)
	return err
}
