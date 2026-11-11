package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const ingestKeyPrefix = "tr_ingest_"

var sourcePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type IngestKey struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Prefix string `json:"prefix"`
}

func ingestKeyDigest(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

func (s *Store) CreateIngestKey(ctx context.Context, project, source string) (IngestKey, string, error) {
	if !sourcePattern.MatchString(source) {
		return IngestKey{}, "", ErrInvalid
	}
	var secret [32]byte
	rand.Read(secret[:])
	token := ingestKeyPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	key := IngestKey{ID: rand.Text(), Source: source, Prefix: token[:len(ingestKeyPrefix)+8]}
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.bind(`INSERT INTO ingest_keys(id,project,source,prefix,digest,created) VALUES(?,?,?,?,?,?)`),
			key.ID, project, source, key.Prefix, ingestKeyDigest(token), time.Now().UnixNano())
		return err
	})
	if err != nil {
		return IngestKey{}, "", err
	}
	return key, token, nil
}

func (s *Store) IngestKeys(ctx context.Context, project string) ([]IngestKey, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT id,source,prefix FROM ingest_keys WHERE project=? ORDER BY created DESC,id`), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []IngestKey{}
	for rows.Next() {
		var key IngestKey
		if err := rows.Scan(&key.ID, &key.Source, &key.Prefix); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) RevokeIngestKey(ctx context.Context, project, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.bind(`DELETE FROM ingest_keys WHERE project=? AND id=?`), project, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return err
	})
}

func (s *Store) IngestKeyIdentity(ctx context.Context, token string) (project, source string, err error) {
	if len(token) != len(ingestKeyPrefix)+base64.RawURLEncoding.EncodedLen(32) || !strings.HasPrefix(token, ingestKeyPrefix) {
		return "", "", sql.ErrNoRows
	}
	err = s.db.QueryRowContext(ctx, s.bind(`SELECT project,source FROM ingest_keys WHERE digest=?`), ingestKeyDigest(token)).Scan(&project, &source)
	return
}
