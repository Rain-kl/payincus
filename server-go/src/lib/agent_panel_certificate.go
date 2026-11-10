// Port of server/src/lib/panel-certificate.ts.
//
// The panel's Incus client certificate is handed to the node during installation
// (GET /api/agent/install-config/:token?format=json) so the node can pin the
// panel's public key. Only the PUBLIC certificate is ever returned: the private
// key lives next to it and must never leave the panel host.
package lib

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// panelPEMCertificatePattern mirrors the TypeScript regexp: exactly one
// certificate block, no key material, no extra text.
var panelPEMCertificatePattern = regexp.MustCompile(`^\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*$`)

// PanelCertificatePaths mirrors getPanelCertificatePaths().
func PanelCertificatePaths() (certPath, keyPath string) {
	appDir := os.Getenv("INCUDAL_APP_DIR")
	if appDir == "" {
		appDir, _ = os.Getwd()
	}
	installDir := os.Getenv("INCUDAL_INSTALL_DIR")
	if installDir == "" {
		installDir = os.Getenv("INSTALL_DIR")
	}
	if installDir == "" && strings.HasSuffix(appDir, "/current") {
		installDir = filepath.Dir(appDir)
	}

	stableCert := ""
	stableKey := ""
	if installDir != "" {
		stableCert = filepath.Join(installDir, "server/certs/client.crt")
		stableKey = filepath.Join(installDir, "server/certs/client.key")
	}

	certPath = os.Getenv("PANEL_CRT_PATH")
	if certPath == "" {
		switch {
		case stableCert != "" && fileExists(stableCert):
			certPath = stableCert
		default:
			// The TypeScript resolves `../../certs/client.crt` relative to
			// server/src/lib. The Go binary lives elsewhere, so look for the same
			// repository file from the working directory and the executable.
			if found, ok := findRepoPath("server/certs/client.crt"); ok {
				certPath = found
			} else {
				certPath = filepath.Join(moduleDir(), "../../certs/client.crt")
			}
		}
	}

	keyPath = os.Getenv("PANEL_KEY_PATH")
	if keyPath == "" {
		switch {
		case stableKey != "" && fileExists(stableKey):
			keyPath = stableKey
		default:
			if found, ok := findRepoPath("server/certs/client.key"); ok {
				keyPath = found
			} else {
				keyPath = filepath.Join(moduleDir(), "../../certs/client.key")
			}
		}
	}
	return certPath, keyPath
}

// moduleDir approximates `dirname(fileURLToPath(import.meta.url))` for the Go
// build: the package's own source directory, discovered from the working
// directory, falling back to the executable's directory.
func moduleDir() string {
	if wd, err := os.Getwd(); err == nil {
		for _, candidate := range []string{
			filepath.Join(wd, "server/src/lib"),
			filepath.Join(wd, "../server/src/lib"),
			filepath.Join(wd, "src/lib"),
		} {
			if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
				return candidate
			}
		}
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Dir(executable)
	}
	return "."
}

// findRepoPath walks up from the working directory and the executable looking
// for rel. Mirrors the helper the hosts route uses for the install script.
func findRepoPath(rel string) (string, bool) {
	starts := []string{}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}
	for _, start := range starts {
		dir := start
		for depth := 0; depth < 8; depth++ {
			candidate := filepath.Join(dir, rel)
			if fileExists(candidate) {
				return candidate, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

// PanelClientCertificate mirrors the readPanelClientCertificate return value.
type PanelClientCertificate struct {
	Certificate string `json:"certificate"`
	Fingerprint string `json:"fingerprint"`
}

// ReadPanelClientCertificate mirrors readPanelClientCertificate: read exactly
// one PEM certificate, refuse private key material, refuse a certificate that is
// not currently valid, and return it with the sha256 of its DER encoding.
func ReadPanelClientCertificate() (*PanelClientCertificate, error) {
	certPath, _ := PanelCertificatePaths()
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if len(text) > 65536 || !panelPEMCertificatePattern.MatchString(text) {
		return nil, errors.New("Expected one PEM public certificate without private key material")
	}

	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("Expected one PEM public certificate without private key material")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("Expected one PEM public certificate without private key material")
	}
	now := time.Now().UTC()
	if now.Before(parsed.NotBefore) || now.After(parsed.NotAfter) {
		return nil, errors.New("Panel client certificate is not currently valid")
	}

	// X509Certificate#toString() re-encodes the PEM with 64-character base64
	// lines, which is exactly what pem.EncodeToMemory produces.
	normalized := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: parsed.Raw})
	sum := sha256.Sum256(parsed.Raw)
	return &PanelClientCertificate{
		Certificate: string(normalized),
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}
