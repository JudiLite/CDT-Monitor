package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/security"
	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	cipher  *security.Cipher
	dataDir string
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	cipher, err := security.LoadOrCreateCipher(dataDir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "data.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err = db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	s := &Store{db: db, cipher: cipher, dataDir: dataDir}
	if err = s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migratePlaintextSecrets(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.recoverInterruptedWork(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) recoverInterruptedWork(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status='queued', locked_at=0, available_at=unixepoch(), updated_at=unixepoch()
WHERE status='running' AND locked_at < unixepoch()-120;
UPDATE notification_outbox SET status='queued', available_at=unixepoch(), updated_at=unixepoch()
WHERE status='sending' AND updated_at < unixepoch()-120;
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.db.PingContext(ctx)
}

func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Encrypt(value string) (string, error) { return s.cipher.Encrypt(value) }
func (s *Store) Decrypt(value string) (string, error) { return s.cipher.Decrypt(value) }

func nullTime(unix sql.NullInt64) *time.Time {
	if !unix.Valid || unix.Int64 <= 0 {
		return nil
	}
	t := time.Unix(unix.Int64, 0).UTC()
	return &t
}

func isNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
