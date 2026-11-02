package config

import (
	"strings"
	"testing"
)

func TestValidateProduction(t *testing.T) {
	base := Config{Env: "prod", JWTSecret: strings.Repeat("x", 32), StoreDriver: "postgres"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}

	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"default jwt secret", func(c *Config) { c.JWTSecret = "dev-only-secret-change-me" }},
		{"short jwt secret", func(c *Config) { c.JWTSecret = "short" }},
		{"memory store", func(c *Config) { c.StoreDriver = "memory" }},
		{"demo site enabled", func(c *Config) { c.DemoSite = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe production config should be rejected")
			}
		})
	}
}

func TestValidateDevelopmentKeepsDefaults(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development defaults should remain usable: %v", err)
	}
}
