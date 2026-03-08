package certs

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestInitCAAndIssueCert(t *testing.T) {
	dir := t.TempDir()

	// Init CA
	if err := InitCA(dir); err != nil {
		t.Fatalf("InitCA: %v", err)
	}

	// Verify files exist
	for _, f := range []string{"family-ca.crt", "family-ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}

	// Init again should fail
	if err := InitCA(dir); err == nil {
		t.Fatal("expected error on second InitCA")
	}

	// Issue a device cert
	if err := IssueCert(dir, dir, "alice"); err != nil {
		t.Fatalf("IssueCert: %v", err)
	}
	for _, f := range []string{"alice.crt", "alice.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}

	// Issue server cert
	if err := IssueCert(dir, dir, "server"); err != nil {
		t.Fatalf("IssueCert server: %v", err)
	}

	// Verify certs load as TLS pairs
	caFile := filepath.Join(dir, "family-ca.crt")
	_, err := LoadServerTLS(
		filepath.Join(dir, "server.crt"),
		filepath.Join(dir, "server.key"),
		caFile,
	)
	if err != nil {
		t.Fatalf("LoadServerTLS: %v", err)
	}

	_, err = LoadClientTLS(
		filepath.Join(dir, "alice.crt"),
		filepath.Join(dir, "alice.key"),
		caFile,
	)
	if err != nil {
		t.Fatalf("LoadClientTLS: %v", err)
	}
}

func TestRevocation(t *testing.T) {
	dir := t.TempDir()
	InitCA(dir)

	if err := RevokeCert(dir, "bob"); err != nil {
		t.Fatalf("RevokeCert: %v", err)
	}

	// Double revoke should fail
	if err := RevokeCert(dir, "bob"); err == nil {
		t.Fatal("expected error on double revoke")
	}

	names, err := LoadRevokedNames(dir)
	if err != nil {
		t.Fatalf("LoadRevokedNames: %v", err)
	}
	if !names["bob"] {
		t.Fatal("bob should be revoked")
	}
}

func TestPeerName(t *testing.T) {
	dir := t.TempDir()
	InitCA(dir)
	IssueCert(dir, dir, "server")
	IssueCert(dir, dir, "alice")

	caFile := filepath.Join(dir, "family-ca.crt")
	serverTLS, _ := LoadServerTLS(
		filepath.Join(dir, "server.crt"),
		filepath.Join(dir, "server.key"),
		caFile,
	)
	clientTLS, _ := LoadClientTLS(
		filepath.Join(dir, "alice.crt"),
		filepath.Join(dir, "alice.key"),
		caFile,
	)
	clientTLS.ServerName = "server"

	// Create a TLS pipe
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			done <- "handshake: " + err.Error()
			return
		}
		name, err := PeerName(tlsConn)
		if err != nil {
			done <- "peername: " + err.Error()
			return
		}
		done <- name
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	name := <-done
	if name != "alice" {
		t.Fatalf("expected alice, got %q", name)
	}
}
