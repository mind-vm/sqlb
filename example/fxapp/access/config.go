package access

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/example/fxapp/config"
)

// Config is the set of spaces this installation serves and the key each one
// answers to.
type Config struct {
	// Keys maps a space slug to its secret. Never logged.
	Keys map[string]string
}

// Slugs returns the configured slugs in a stable order. The spaces module
// provisions exactly these at boot.
func (c Config) Slugs() []string {
	slugs := make([]string, 0, len(c.Keys))
	for slug := range c.Keys {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// NewConfig reads FXAPP_SPACE_KEYS, a comma-separated list of slug=secret:
//
//	export FXAPP_SPACE_KEYS="acme=$(head -c 24 /dev/urandom | base64),globex=..."
//
// There is no default and no generated fallback. A server that invents a key
// at startup hands out access that stops working on restart, and the failure
// looks like a bug in the client.
func NewConfig() (Config, error) {
	raw, err := config.Require("SPACE_KEYS")
	if err != nil {
		return Config{}, fmt.Errorf("access: %w (a comma-separated list of slug=secret)", err)
	}

	keys := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		slug, secret, ok := strings.Cut(pair, "=")
		slug, secret = strings.TrimSpace(slug), strings.TrimSpace(secret)
		if !ok || slug == "" || secret == "" {
			return Config{}, fmt.Errorf("access: FXAPP_SPACE_KEYS: %q is not slug=secret", pair)
		}
		if len(secret) < 16 {
			// Refused rather than warned about. A 4-character key in a demo is
			// how a 4-character key reaches a deployment.
			return Config{}, fmt.Errorf("access: the key for %q is shorter than 16 characters", slug)
		}
		if _, dup := keys[slug]; dup {
			return Config{}, fmt.Errorf("access: FXAPP_SPACE_KEYS names %q twice", slug)
		}
		keys[slug] = secret
	}
	if len(keys) == 0 {
		return Config{}, fmt.Errorf("access: FXAPP_SPACE_KEYS is empty")
	}
	return Config{Keys: keys}, nil
}
