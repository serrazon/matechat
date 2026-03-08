package store

import (
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertAndQuery(t *testing.T) {
	s := testStore(t)

	s.InsertMessage("alice", "hello", 1000)
	s.InsertMessage("bob", "hi", 2000)
	s.InsertMessage("alice", "how are you", 3000)

	msgs, err := s.MessagesSince(0, 100)
	if err != nil {
		t.Fatalf("MessagesSince: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].From != "alice" || msgs[0].Body != "hello" {
		t.Fatalf("unexpected first message: %+v", msgs[0])
	}

	msgs2, err := s.MessagesSince(1500, 100)
	if err != nil {
		t.Fatalf("MessagesSince(1500): %v", err)
	}
	if len(msgs2) != 2 {
		t.Fatalf("expected 2 messages since ts=1500, got %d", len(msgs2))
	}
}

func TestLatestTS(t *testing.T) {
	s := testStore(t)

	ts, err := s.LatestTS()
	if err != nil {
		t.Fatalf("LatestTS on empty: %v", err)
	}
	if ts != 0 {
		t.Fatalf("expected 0 on empty db, got %d", ts)
	}

	s.InsertMessage("alice", "hello", 5000)
	s.InsertMessage("bob", "hi", 3000)

	ts, err = s.LatestTS()
	if err != nil {
		t.Fatalf("LatestTS: %v", err)
	}
	if ts != 5000 {
		t.Fatalf("expected 5000, got %d", ts)
	}
}

func TestRecentMessages(t *testing.T) {
	s := testStore(t)

	for i := 0; i < 20; i++ {
		s.InsertMessage("alice", "msg", int64(i*1000))
	}

	msgs, err := s.RecentMessages(5)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5, got %d", len(msgs))
	}
	// Should be in ascending order, most recent 5
	if msgs[0].TS != 15000 {
		t.Fatalf("expected first ts=15000, got %d", msgs[0].TS)
	}
	if msgs[4].TS != 19000 {
		t.Fatalf("expected last ts=19000, got %d", msgs[4].TS)
	}
}

func TestTransfers(t *testing.T) {
	s := testStore(t)

	err := s.InsertTransfer("tid-123", "photo.jpg", 204800, "/tmp/photo.jpg", 9999)
	if err != nil {
		t.Fatalf("InsertTransfer: %v", err)
	}

	tr, err := s.Transfer("tid-123")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if tr == nil {
		t.Fatal("expected transfer, got nil")
	}
	if tr.Filename != "photo.jpg" || tr.Size != 204800 {
		t.Fatalf("unexpected transfer: %+v", tr)
	}

	// Non-existent
	tr2, err := s.Transfer("nope")
	if err != nil {
		t.Fatalf("Transfer(nope): %v", err)
	}
	if tr2 != nil {
		t.Fatal("expected nil for non-existent transfer")
	}
}
