// SPDX-License-Identifier: Apache-2.0
// Package postgres implements immutable, versioned metadata with namespace
// ownership and transactional schema migration.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shenli/ramet/storage"
)

const SchemaVersion = 1
const migrationLock int64 = 0x72616d65746d6967

type Store struct {
	mu        sync.Mutex
	conn      *pgx.Conn
	namespace string
}

// Open migrates schema under a database-wide advisory lock, then claims the
// namespace using the SAME connection used for all metadata operations. A lost
// connection cannot silently reconnect without re-establishing ownership.
func Open(ctx context.Context, dsn, namespace string) (*Store, error) {
	if namespace == "" || len(namespace) > 128 {
		return nil, errors.New("PostgreSQL namespace must contain 1–128 characters")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL DSN")
	}
	cfg.RuntimeParams["application_name"] = "ramet/" + namespace
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("cannot connect to PostgreSQL; check DSN, credentials, TLS, and reachability")
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close(context.Background())
		}
	}()
	if err = migrate(ctx, conn); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte("ramet/namespace/" + namespace))
	key := int64(binary.BigEndian.Uint64(hash[:8]))
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("%w: PostgreSQL namespace already has an active worker", storage.ErrConflict)
	}
	ok = true
	return &Store{conn: conn, namespace: namespace}, nil
}

func migrate(ctx context.Context, c *pgx.Conn) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLock); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS ramet_schema_version (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), version integer NOT NULL)`); err != nil {
		return err
	}
	var version int
	err = tx.QueryRow(ctx, "SELECT version FROM ramet_schema_version WHERE singleton").Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		version = 0
	} else if err != nil {
		return err
	}
	if version > SchemaVersion || version < 0 {
		return fmt.Errorf("unsupported PostgreSQL schema version %d (binary supports %d)", version, SchemaVersion)
	}
	if version == 0 {
		_, err = tx.Exec(ctx, `CREATE TABLE ramet_namespaces (
   namespace text PRIMARY KEY, content_identity text NOT NULL);
  CREATE TABLE ramet_snapshots (
   namespace text NOT NULL REFERENCES ramet_namespaces(namespace),
   id text NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
   parent text,
   manifest bytea NOT NULL,
   created_at timestamptz NOT NULL DEFAULT now(),
   PRIMARY KEY (namespace,id),
   FOREIGN KEY (namespace,parent) REFERENCES ramet_snapshots(namespace,id) ON DELETE RESTRICT);
  CREATE INDEX ramet_snapshots_parent_idx ON ramet_snapshots(namespace,parent);
  INSERT INTO ramet_schema_version(singleton,version) VALUES(true,1)
   ON CONFLICT(singleton) DO UPDATE SET version=EXCLUDED.version`)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) BindContent(ctx context.Context, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if identity == "" {
		return errors.New("content identity is required")
	}
	_, err := s.conn.Exec(ctx, "INSERT INTO ramet_namespaces(namespace,content_identity) VALUES($1,$2) ON CONFLICT DO NOTHING", s.namespace, identity)
	if err != nil {
		return err
	}
	var current string
	if err = s.conn.QueryRow(ctx, "SELECT content_identity FROM ramet_namespaces WHERE namespace=$1", s.namespace).Scan(&current); err != nil {
		return err
	}
	if current != identity {
		return fmt.Errorf("%w: namespace belongs to another content store; use the original endpoint/bucket/prefix", storage.ErrConflict)
	}
	return nil
}
func (s *Store) Publish(ctx context.Context, r storage.Record) error {
	if err := storage.ValidateRecord(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var parent any
	if r.Parent != "" {
		parent = r.Parent
	}
	_, err := s.conn.Exec(ctx, "INSERT INTO ramet_snapshots(namespace,id,parent,manifest) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", s.namespace, r.ID, parent, r.Data)
	if err != nil {
		return classify(err)
	}
	existing, err := s.get(ctx, r.ID)
	if err != nil {
		return err
	}
	if existing.Parent != r.Parent || !bytes.Equal(existing.Data, r.Data) {
		return fmt.Errorf("%w: immutable manifest differs", storage.ErrConflict)
	}
	return nil
}
func (s *Store) get(ctx context.Context, id string) (storage.Record, error) {
	r := storage.Record{ID: id}
	err := s.conn.QueryRow(ctx, "SELECT COALESCE(parent,''),manifest FROM ramet_snapshots WHERE namespace=$1 AND id=$2", s.namespace, id).Scan(&r.Parent, &r.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if err == nil {
		err = storage.ValidateRecord(r)
	}
	return r, err
}
func (s *Store) Get(ctx context.Context, id string) (storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, id)
}
func (s *Store) List(ctx context.Context) ([]storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.Query(ctx, "SELECT id,COALESCE(parent,''),manifest FROM ramet_snapshots WHERE namespace=$1 ORDER BY id", s.namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []storage.Record{}
	for rows.Next() {
		var r storage.Record
		if err = rows.Scan(&r.ID, &r.Parent, &r.Data); err != nil {
			return nil, err
		}
		if err = storage.ValidateRecord(r); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.conn.Exec(ctx, "DELETE FROM ramet_snapshots WHERE namespace=$1 AND id=$2", s.namespace, id)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	return nil
}
func (s *Store) Ping(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Ping(ctx)
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.conn.Close(ctx)
}
func classify(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23503" {
		return fmt.Errorf("%w: missing parent or snapshot still referenced", storage.ErrConflict)
	}
	return err
}
