package certs

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CreatePack issues a device cert for name (writing cert+key to outDir),
// then bundles name.crt, name.key, family-ca.crt, and a pre-filled config.json
// into a zip file at outDir/matechat-pack-<name>.zip.
// broker is written into config.json (e.g. "myserver.com:9000").
func CreatePack(caDir, outDir, name, broker string) (string, error) {
	// 1. Issue the device cert (writes name.crt + name.key to outDir)
	if err := IssueCert(caDir, outDir, name); err != nil {
		return "", fmt.Errorf("issue cert: %w", err)
	}

	// 2. Build the zip
	zipPath := filepath.Join(outDir, "matechat-pack-"+name+".zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return "", fmt.Errorf("create zip: %w", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	// Helper: add a file from disk into the zip (flat, no subdirs)
	addFile := func(src, nameInZip string) error {
		r, err := os.Open(src)
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := zw.Create(nameInZip)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return err
	}

	// Add cert, key, CA
	if err := addFile(filepath.Join(outDir, name+".crt"), name+".crt"); err != nil {
		return "", fmt.Errorf("zip cert: %w", err)
	}
	if err := addFile(filepath.Join(outDir, name+".key"), name+".key"); err != nil {
		return "", fmt.Errorf("zip key: %w", err)
	}
	if err := addFile(filepath.Join(caDir, "family-ca.crt"), "family-ca.crt"); err != nil {
		return "", fmt.Errorf("zip ca: %w", err)
	}

	// Add pre-filled config.json
	cfg := map[string]string{
		"broker": broker,
		"cert":   name + ".crt",
		"key":    name + ".key",
		"ca":     "family-ca.crt",
	}
	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	cw, err := zw.Create("config.json")
	if err != nil {
		return "", fmt.Errorf("zip config: %w", err)
	}
	if _, err := cw.Write(cfgBytes); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}

	return zipPath, nil
}
