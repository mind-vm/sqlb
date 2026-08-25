package studio

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/mind-vm/sqlb/schema"
)

// LoadManifest reads a schema.Manifest from a sqlb.json file at path.
func LoadManifest(path string) (*schema.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("studio: reading manifest: %w", err)
	}
	var m schema.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("studio: parsing manifest %s: %w", path, err)
	}
	return &m, nil
}
