// official_machine.go reads the machine identity the official Qoder client
// persists on this host (SharedClientCache/cache/machine_token.json and
// cache/id). Cockpit-tools (github.com/jlcodes99/cockpit-tools) uses the same
// cache to present a recognised client identity; the CN gateway rejects some
// accounts' model-catalog calls when the COSY machine identity does not match
// a real installation. Reading the cache is best-effort: when it is absent we
// keep the plugin-generated identity.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// officialMachineIdentity is the subset of the official client cache we use.
type officialMachineIdentity struct {
	MachineID    string
	MachineToken string
	MachineType  string
	MachineOS    string
	CosyVersion  string
}

// empty reports whether nothing usable was found in the cache.
func (m officialMachineIdentity) empty() bool {
	return m.MachineID == "" && m.MachineToken == "" && m.CosyVersion == ""
}

// qoderUserDataDirsOverride lets tests point the probe at a fixture tree.
var qoderUserDataDirsOverride []string

// qoderUserDataDirs lists the official client user-data roots in probe order.
func qoderUserDataDirs() []string {
	if len(qoderUserDataDirsOverride) > 0 {
		return qoderUserDataDirsOverride
	}
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		switch {
		case fileExists(filepath.Join(home, "Library", "Application Support", "Qoder")):
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "Qoder"))
		}
		dirs = append(dirs,
			filepath.Join(home, ".config", "Qoder"),
			filepath.Join(home, ".qoder"),
		)
	}
	for _, env := range []string{"APPDATA", "LOCALAPPDATA"} {
		if root := strings.TrimSpace(os.Getenv(env)); root != "" {
			dirs = append(dirs, filepath.Join(root, "Qoder"))
		}
	}
	return dirs
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// readOfficialMachineIdentity loads the cached identity, returning an empty
// value when the client was never installed or the cache is unreadable.
func readOfficialMachineIdentity() officialMachineIdentity {
	var out officialMachineIdentity
	for _, root := range qoderUserDataDirs() {
		for _, base := range []string{
			filepath.Join(root, "SharedClientCache", "cache"),
			filepath.Join(root, "shared_client", "cache"),
			filepath.Join(root, "cache"),
		} {
			if out.MachineID == "" {
				if raw, err := os.ReadFile(filepath.Join(base, "id")); err == nil {
					out.MachineID = strings.TrimSpace(string(raw))
				}
			}
			if raw, err := os.ReadFile(filepath.Join(base, "machine_token.json")); err == nil {
				var cached struct {
					Token       string `json:"token"`
					Type        string `json:"type"`
					ID          string `json:"id"`
					OS          string `json:"os"`
					Version     string `json:"version"`
					MachineType string `json:"machine_type"`
					MachineOS   string `json:"machine_os"`
					CosyVersion string `json:"cosy_version"`
				}
				if json.Unmarshal(raw, &cached) == nil {
					if v := strings.TrimSpace(cached.Token); v != "" {
						out.MachineToken = v
					}
					if v := firstNonEmpty(cached.Type, cached.MachineType); v != "" {
						out.MachineType = v
					}
					if v := firstNonEmpty(cached.OS, cached.MachineOS); v != "" {
						out.MachineOS = v
					}
					if v := strings.TrimSpace(cached.CosyVersion); v != "" {
						out.CosyVersion = v
					}
					if v := strings.TrimSpace(cached.Version); v != "" && out.CosyVersion == "" {
						out.CosyVersion = v
					}
					if v := strings.TrimSpace(cached.ID); v != "" && out.MachineID == "" {
						out.MachineID = v
					}
				}
			}
			if out.CosyVersion == "" {
				if raw, err := os.ReadFile(filepath.Join(base, "version.txt")); err == nil {
					out.CosyVersion = strings.TrimSpace(string(raw))
				}
			}
		}
		if !out.empty() {
			break
		}
	}
	return out
}
