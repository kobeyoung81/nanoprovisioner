// Package migrations holds the provisioner's declarative MongoDB manifests.
package migrations

import "embed"

//go:embed *.json
var FS embed.FS

// Load returns all manifests as a name→content map.
func Load() (map[string]string, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := FS.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		out[entry.Name()] = string(data)
	}
	return out, nil
}
