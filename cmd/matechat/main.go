package main

import (
	"archive/zip"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"matechat/internal/broker"
	"matechat/internal/certs"
	"matechat/internal/client"
	"matechat/internal/config"
	"matechat/internal/peer"
	"matechat/internal/proto"
	"matechat/internal/store"
	"matechat/internal/updater"
)

var version = "dev"

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

var certsPackCmd = &cobra.Command{
	Use:   "pack",
	Short: "Issue a cert and bundle it into a ready-to-send zip pack",
	RunE:  runCertsPack,
}

var setupCmd = &cobra.Command{
	Use:   "setup <pack.zip>",
	Short: "Install a matechat-pack zip into ~/.matechat/",
	Args:  cobra.ExactArgs(1),
	RunE:  runSetup,
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Check for a newer release and self-update",
	RunE:  runUpdate,
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
	flagCertsDir  string
	flagCertsOut  string
	flagCertName  string
	flagPackBroker string
)

func init() {
	rootCmd.Version = version
	rootCmd.PersistentFlags().StringVar(&flagCert, "cert", "", "path to device certificate")
	rootCmd.PersistentFlags().StringVar(&flagKey, "key", "", "path to device private key")
	rootCmd.PersistentFlags().StringVar(&flagCA, "ca", "", "path to family CA certificate")
	rootCmd.Flags().StringVar(&flagBroker, "broker", "", "broker address (default from config or localhost:9000)")
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

	certsPackCmd.Flags().StringVar(&flagCertsDir, "ca-dir", ".", "directory containing CA files")
	certsPackCmd.Flags().StringVar(&flagCertsOut, "out-dir", ".", "output directory for cert files and zip")
	certsPackCmd.Flags().StringVar(&flagCertName, "name", "", "device name")
	certsPackCmd.Flags().StringVar(&flagPackBroker, "broker", "", "broker address to embed in pack (e.g. myserver.com:9000)")
	certsPackCmd.MarkFlagRequired("name")
	certsPackCmd.MarkFlagRequired("broker")

	certsCmd.AddCommand(certsInitCmd)
	certsCmd.AddCommand(certsIssueCmd)
	certsCmd.AddCommand(certsRevokeCmd)
	certsCmd.AddCommand(certsPackCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(certsCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(updateCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runClient(cmd *cobra.Command, args []string) error {
	// Load config file, then apply CLI flags as overrides
	cfg, _ := config.Load()
	if flagCert != "" {
		cfg.Cert = flagCert
	}
	if flagKey != "" {
		cfg.Key = flagKey
	}
	if flagCA != "" {
		cfg.CA = flagCA
	}
	if flagBroker != "" {
		cfg.Broker = flagBroker
	}
	if cfg.Broker == "" {
		cfg.Broker = "localhost:9000"
	}

	if cfg.Cert == "" || cfg.Key == "" || cfg.CA == "" {
		return fmt.Errorf("--cert, --key, and --ca are required (or set in ~/.matechat/config.json)")
	}

	// Load TLS configs
	clientTLS, err := certs.LoadClientTLS(cfg.Cert, cfg.Key, cfg.CA)
	if err != nil {
		return fmt.Errorf("load client TLS: %w", err)
	}
	serverTLS, err := certs.LoadServerTLS(cfg.Cert, cfg.Key, cfg.CA)
	if err != nil {
		return fmt.Errorf("load server TLS: %w", err)
	}

	// Determine self name from cert
	selfName, err := selfNameFromCert(cfg.Cert)
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
	fileCh := make(chan client.FileReceivedMsg, 16)

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
	onFileReceived := func(from, filename, localPath string) {
		select {
		case fileCh <- client.FileReceivedMsg{From: from, Filename: filename, Path: localPath}:
		default:
		}
	}

	// Create peer manager
	mgr := peer.NewManager(selfName, flagListen, clientTLS, serverTLS, st,
		onMessage, onPeerJoin, onPeerLeave, onFileReceived)

	// Create TUI model with manager and shared channels
	model := client.New(mgr, st, msgCh, joinCh, leaveCh, fileCh, version)

	// Connect to broker
	if err := mgr.ConnectBroker(cfg.Broker); err != nil {
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
	// Load config file, then apply CLI flags as overrides
	cfg, _ := config.Load()
	if flagCert != "" {
		cfg.Cert = flagCert
	}
	if flagKey != "" {
		cfg.Key = flagKey
	}
	if flagCA != "" {
		cfg.CA = flagCA
	}

	if cfg.Cert == "" || cfg.Key == "" || cfg.CA == "" {
		return fmt.Errorf("--cert, --key, and --ca are required (or set in ~/.matechat/config.json)")
	}

	tlsCfg, err := certs.LoadServerTLS(cfg.Cert, cfg.Key, cfg.CA)
	if err != nil {
		return fmt.Errorf("load server TLS: %w", err)
	}

	revokedDir := filepath.Dir(cfg.CA)
	revoked, _ := certs.LoadRevokedNames(revokedDir)

	b := broker.New(tlsCfg, revoked, version)
	return b.ListenAndServe(flagServeAddr)
}

func runUpdate(cmd *cobra.Command, args []string) error {
	if version == "dev" {
		fmt.Println("running a dev build — update only available for tagged releases")
		return nil
	}
	fmt.Printf("current version: %s\nChecking for updates...\n", version)
	newVer, err := updater.CheckAndUpdate(version)
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	if newVer == "" {
		fmt.Printf("already up to date (%s)\n", version)
		return nil
	}
	fmt.Printf("updated to %s — restart matechat to use the new version\n", newVer)
	return nil
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

func runCertsPack(cmd *cobra.Command, args []string) error {
	zipPath, err := certs.CreatePack(flagCertsDir, flagCertsOut, flagCertName, flagPackBroker)
	if err != nil {
		return err
	}
	fmt.Printf("Pack created: %s\n", zipPath)
	fmt.Println("Send this file to the family member.")
	fmt.Println("They run: matechat setup matechat-pack-" + flagCertName + ".zip")
	return nil
}

func runSetup(cmd *cobra.Command, args []string) error {
	packPath := args[0]

	home, _ := os.UserHomeDir()
	destDir := filepath.Join(home, ".matechat")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("create ~/.matechat: %w", err)
	}

	r, err := zip.OpenReader(packPath)
	if err != nil {
		return fmt.Errorf("open pack: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		dest := filepath.Join(destDir, filepath.Base(f.Name)) // flat extract, no path traversal

		// Never overwrite existing private keys
		if strings.HasSuffix(f.Name, ".key") {
			if _, err := os.Stat(dest); err == nil {
				fmt.Printf("  skipped (already exists): %s\n", f.Name)
				continue
			}
		}

		perm := os.FileMode(0644)
		if strings.HasSuffix(f.Name, ".key") {
			perm = 0600
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}

		if err := os.WriteFile(dest, data, perm); err != nil {
			return fmt.Errorf("write %s: %w", f.Name, err)
		}
		fmt.Printf("  installed: %s\n", f.Name)
	}

	fmt.Println("\nSetup complete! Run: matechat")
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
