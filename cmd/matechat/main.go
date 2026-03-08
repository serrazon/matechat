package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"matechat/internal/broker"
	"matechat/internal/certs"
	"matechat/internal/client"
	"matechat/internal/peer"
	"matechat/internal/proto"
	"matechat/internal/store"
)

var rootCmd = &cobra.Command{
	Use:   "matechat",
	Short: "Family chat CLI — peer-to-peer, end-to-end encrypted",
	RunE:  runClient,
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the broker (directory + signaling + relay)",
	RunE:  runServe,
}

var certsCmd = &cobra.Command{
	Use:   "certs",
	Short: "Certificate management",
}

var certsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a new family CA",
	RunE:  runCertsInit,
}

var certsIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Issue a new device certificate",
	RunE:  runCertsIssue,
}

var certsRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke a device certificate",
	RunE:  runCertsRevoke,
}

// Global flags
var (
	flagCert   string
	flagKey    string
	flagCA     string
	flagBroker string
	flagListen string
	flagDB     string
)

// Serve flags
var (
	flagServeAddr string
)

// Certs flags
var (
	flagCertsDir string
	flagCertsOut string
	flagCertName string
)

func init() {
	rootCmd.PersistentFlags().StringVar(&flagCert, "cert", "", "path to device certificate")
	rootCmd.PersistentFlags().StringVar(&flagKey, "key", "", "path to device private key")
	rootCmd.PersistentFlags().StringVar(&flagCA, "ca", "", "path to family CA certificate")
	rootCmd.Flags().StringVar(&flagBroker, "broker", "localhost:9000", "broker address")
	rootCmd.Flags().StringVar(&flagListen, "listen", ":9100", "listen address for peer connections")
	rootCmd.Flags().StringVar(&flagDB, "db", "", "path to local message database (default ~/.matechat/history.db)")

	serveCmd.Flags().StringVar(&flagServeAddr, "addr", ":9000", "broker bind address")

	certsInitCmd.Flags().StringVar(&flagCertsDir, "dir", ".", "directory to store CA files")
	certsIssueCmd.Flags().StringVar(&flagCertsDir, "ca-dir", ".", "directory containing CA files")
	certsIssueCmd.Flags().StringVar(&flagCertsOut, "out-dir", ".", "output directory for cert files")
	certsIssueCmd.Flags().StringVar(&flagCertName, "name", "", "device name (used as cert CN)")
	certsIssueCmd.MarkFlagRequired("name")
	certsRevokeCmd.Flags().StringVar(&flagCertsDir, "ca-dir", ".", "directory containing CA files")
	certsRevokeCmd.Flags().StringVar(&flagCertName, "name", "", "device name to revoke")
	certsRevokeCmd.MarkFlagRequired("name")

	certsCmd.AddCommand(certsInitCmd)
	certsCmd.AddCommand(certsIssueCmd)
	certsCmd.AddCommand(certsRevokeCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(certsCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runClient(cmd *cobra.Command, args []string) error {
	if flagCert == "" || flagKey == "" || flagCA == "" {
		return fmt.Errorf("--cert, --key, and --ca are required")
	}

	// Load TLS configs
	clientTLS, err := certs.LoadClientTLS(flagCert, flagKey, flagCA)
	if err != nil {
		return fmt.Errorf("load client TLS: %w", err)
	}
	serverTLS, err := certs.LoadServerTLS(flagCert, flagKey, flagCA)
	if err != nil {
		return fmt.Errorf("load server TLS: %w", err)
	}

	// Determine self name from cert
	selfName, err := selfNameFromCert(flagCert)
	if err != nil {
		return err
	}

	// Open local store
	dbPath := flagDB
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".matechat", "history.db")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Create shared channels for manager → TUI communication
	msgCh := make(chan proto.ChatMsg, 64)
	joinCh := make(chan string, 16)
	leaveCh := make(chan string, 16)

	onMessage := func(msg proto.ChatMsg) {
		select {
		case msgCh <- msg:
		default:
		}
	}
	onPeerJoin := func(name string) {
		select {
		case joinCh <- name:
		default:
		}
	}
	onPeerLeave := func(name string) {
		select {
		case leaveCh <- name:
		default:
		}
	}

	// Create peer manager
	mgr := peer.NewManager(selfName, flagListen, clientTLS, serverTLS, st,
		onMessage, onPeerJoin, onPeerLeave)

	// Create TUI model with manager and shared channels
	model := client.New(mgr, st, msgCh, joinCh, leaveCh)

	// Connect to broker
	if err := mgr.ConnectBroker(flagBroker); err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}

	// Start peer manager (register + listen + discover)
	if err := mgr.Start(); err != nil {
		return fmt.Errorf("start peer manager: %w", err)
	}

	// Run TUI
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI: %w", err)
	}

	mgr.Shutdown()
	return nil
}

func runServe(cmd *cobra.Command, args []string) error {
	if flagCert == "" || flagKey == "" || flagCA == "" {
		return fmt.Errorf("--cert, --key, and --ca are required")
	}

	tlsCfg, err := certs.LoadServerTLS(flagCert, flagKey, flagCA)
	if err != nil {
		return fmt.Errorf("load server TLS: %w", err)
	}

	revokedDir := filepath.Dir(flagCA)
	revoked, _ := certs.LoadRevokedNames(revokedDir)

	b := broker.New(tlsCfg, revoked)
	log.Printf("starting broker on %s", flagServeAddr)
	return b.ListenAndServe(flagServeAddr)
}

func runCertsInit(cmd *cobra.Command, args []string) error {
	if err := certs.InitCA(flagCertsDir); err != nil {
		return err
	}
	fmt.Printf("Family CA created in %s/\n", flagCertsDir)
	fmt.Println("  family-ca.crt — distribute to all devices")
	fmt.Println("  family-ca.key — keep offline and safe!")
	return nil
}

func runCertsIssue(cmd *cobra.Command, args []string) error {
	if err := certs.IssueCert(flagCertsDir, flagCertsOut, flagCertName); err != nil {
		return err
	}
	fmt.Printf("Certificate issued for %q\n", flagCertName)
	fmt.Printf("  %s/%s.crt\n", flagCertsOut, flagCertName)
	fmt.Printf("  %s/%s.key\n", flagCertsOut, flagCertName)
	return nil
}

func runCertsRevoke(cmd *cobra.Command, args []string) error {
	if err := certs.RevokeCert(flagCertsDir, flagCertName); err != nil {
		return err
	}
	fmt.Printf("Certificate for %q revoked\n", flagCertName)
	fmt.Println("Send broker a SIGHUP to reload")
	return nil
}

// selfNameFromCert reads the CN from the device certificate.
func selfNameFromCert(certFile string) (string, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return "", fmt.Errorf("read cert: %w", err)
	}

	// Use a temporary tls.LoadX509KeyPair-style parse for just the cert
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM block in cert file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert: %w", err)
	}
	if cert.Subject.CommonName == "" {
		return "", fmt.Errorf("cert has no CN")
	}
	return cert.Subject.CommonName, nil
}
