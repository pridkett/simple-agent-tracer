package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrInvalid = errors.New("invalid request")
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type Store struct {
	db       *sql.DB
	postgres bool
	writer   sync.Mutex
}

type Project struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Retention int    `json:"retentionDays"`
	Capture   bool   `json:"captureContent"`
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	driver := "sqlite"
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		driver = "pgx"
	} else {
		if !strings.HasPrefix(databaseURL, "file:") {
			databaseURL = (&url.URL{Scheme: "file", Opaque: url.PathEscape(databaseURL)}).String()
		}
		separator := "?"
		if strings.Contains(databaseURL, "?") {
			separator = "&"
		}
		databaseURL += separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open(driver, databaseURL)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: driver == "pgx"}
	if s.postgres {
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
	} else {
		db.SetMaxOpenConns(1)
	}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = s.CreateProject(ctx, "default", 30, true); err != nil && !errors.Is(err, ErrInvalid) {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error                    { return s.db.Close() }
func (s *Store) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) bind(q string) string {
	if !s.postgres {
		return q
	}
	var out strings.Builder
	index := 0
	for _, r := range q {
		if r == '?' {
			index++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(index))
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (s *Store) migrate(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
			return err
		}
		const latest = 2
		if version > latest {
			return fmt.Errorf("database schema version %d is newer than supported", version)
		}
		backend := "sqlite"
		if s.postgres {
			backend = "postgres"
		}
		for next := version + 1; next <= latest; next++ {
			path := fmt.Sprintf("migrations/%03d_%s.sql", next, backend)
			if next == 2 {
				path = "migrations/002_ingest_keys.sql"
			}
			ddl, err := migrations.ReadFile(path)
			if err != nil {
				return err
			}
			for _, statement := range strings.Split(string(ddl), ";") {
				if strings.TrimSpace(statement) == "" {
					continue
				}
				if _, err = tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, s.bind(`INSERT INTO schema_migrations(version) VALUES (?)`), next); err != nil {
				return err
			}
		}
		return nil
	})
}

// Serialize mutations across instances on PostgreSQL and within the SQLite process.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	if !s.postgres {
		s.writer.Lock()
		defer s.writer.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(42175814029117932)`); err != nil {
			return err
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Project(ctx context.Context, key string) (Project, error) {
	var p Project
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT id,slug,retention_days,capture FROM projects WHERE slug=? OR id=?`), key, key).
		Scan(&p.ID, &p.Slug, &p.Retention, &p.Capture)
	return p, err
}

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,slug,retention_days,capture FROM projects ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.Retention, &p.Capture); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) CreateProject(ctx context.Context, slug string, retention int, capture bool) (Project, error) {
	if !slugPattern.MatchString(slug) || retention < 1 || retention > 36500 {
		return Project{}, ErrInvalid
	}
	p := Project{"p_" + identity(slug), slug, retention, capture}
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.bind(`INSERT INTO projects(id,slug,retention_days,capture) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`), p.ID, slug, retention, capture)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return ErrInvalid
		}
		return err
	})
	return p, err
}

func (s *Store) UpdateProject(ctx context.Context, p Project, retention *int, capture *bool) (Project, error) {
	if retention != nil && (*retention < 1 || *retention > 36500) {
		return Project{}, ErrInvalid
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, s.bind(`UPDATE projects SET retention_days=COALESCE(?,retention_days),capture=COALESCE(?,capture)
  WHERE id=? RETURNING id,slug,retention_days,capture`), retention, capture, p.ID).Scan(&p.ID, &p.Slug, &p.Retention, &p.Capture)
	})
	return p, err
}

func identity(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(strconv.Itoa(len(part)) + ":" + part))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func MetadataType(raw json.RawMessage) string {
	switch raw[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case '[':
		return "array"
	case '{':
		return "object"
	default:
		if strings.ContainsAny(string(raw), ".eE") {
			return "double"
		}
		return "integer"
	}
}

func MetadataDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
