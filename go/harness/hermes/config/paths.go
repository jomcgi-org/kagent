package config

import "path/filepath"

// HermesHome is HERMES_HOME under the Actor's durable directory.
func HermesHome(dataDir string) string {
	return filepath.Join(dataDir, "hermes")
}
