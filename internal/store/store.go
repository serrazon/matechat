// Package store provides client-side SQLite storage for message history
// and file transfer metadata. Each device maintains its own local database.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Message struct {
	ID   int64
	From string
	Body string
	TS   int64
}

type Transfer struct {
	TransferID string
	Filename   string
	Size       int64
	LocalPath  string
	ReceivedAt int64
}

// Open opens (or creates) a SQLite database at path and runs migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode for better concurrent read/write performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			from_ TEXT    NOT NULL,
			body  TEXT    NOT NULL,
			ts    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(ts);

		CREATE TABLE IF NOT EXISTS transfers (
			transfer_id TEXT    PRIMARY KEY,
			filename    TEXT    NOT NULL,
			size        INTEGER NOT NULL,
			local_path  TEXT    NOT NULL,
			received_at INTEGER NOT NULL
		);
	`)
	return err
}

// InsertMessage stores a chat message.
func (s *Store) InsertMessage(from, body string, ts int64) error {
	_, err := s.db.Exec(
		"INSERT INTO messages (from_, body, ts) VALUES (?, ?, ?)",
		from, body, ts,
	)
	return err
}

// MessagesSince returns messages with ts > sinceTS, ordered by ts ascending, limited to limit.
func (s *Store) MessagesSince(sinceTS int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		"SELECT id, from_, body, ts FROM messages WHERE ts > ? ORDER BY ts ASC LIMIT ?",
		sinceTS, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.TS); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// RecentMessages returns the most recent N messages, ordered oldest first.
func (s *Store) RecentMessages(limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, from_, body, ts FROM (
			SELECT id, from_, body, ts FROM messages ORDER BY ts DESC LIMIT ?
		) ORDER BY ts ASC`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.TS); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// LatestTS returns the timestamp of the most recent message, or 0 if empty.
func (s *Store) LatestTS() (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow("SELECT MAX(ts) FROM messages").Scan(&ts)
	if err != nil {
		return 0, err
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// InsertTransfer records a completed file transfer.
func (s *Store) InsertTransfer(transferID, filename string, size int64, localPath string, receivedAt int64) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO transfers (transfer_id, filename, size, local_path, received_at) VALUES (?, ?, ?, ?, ?)",
		transferID, filename, size, localPath, receivedAt,
	)
	return err
}

// Transfer looks up a completed file transfer by ID.
func (s *Store) Transfer(transferID string) (*Transfer, error) {
	var t Transfer
	err := s.db.QueryRow(
		"SELECT transfer_id, filename, size, local_path, received_at FROM transfers WHERE transfer_id = ?",
		transferID,
	).Scan(&t.TransferID, &t.Filename, &t.Size, &t.LocalPath, &t.ReceivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}
