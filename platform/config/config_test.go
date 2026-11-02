package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateProduction(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	base := Config{Env: "prod", JWTSecret: strings.Repeat("x", 32), StoreDriver: "postgres",
		KEKKeys: map[int][]byte{1: key}, KEKCurrentVersion: 1}
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
		{"missing KEK", func(c *Config) { c.KEKKeys = nil }},
		{"missing current KEK version", func(c *Config) { c.KEKCurrentVersion = 2 }},
		{"invalid KEK config", func(c *Config) { c.KEKConfigError = "invalid" }},
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

func TestParseKEKKeyring(t *testing.T) {
	key1 := []byte(strings.Repeat("a", 32))
	key2 := []byte(strings.Repeat("b", 32))
	raw := "1:" + base64.StdEncoding.EncodeToString(key1) + ",2:" + base64.StdEncoding.EncodeToString(key2)
	keys, current, parseErr := parseKEKKeyring(raw, 2)
	if parseErr != "" || current != 2 || len(keys) != 2 {
		t.Fatalf("valid keyring rejected: keys=%d current=%d err=%q", len(keys), current, parseErr)
	}
	if string(keys[1]) != string(key1) || string(keys[2]) != string(key2) {
		t.Fatal("keyring contents changed")
	}
	for _, invalid := range []string{"bad-entry", "1:not-base64", "1:" + base64.StdEncoding.EncodeToString([]byte("short")), "1:" + base64.StdEncoding.EncodeToString(key1) + ",1:" + base64.StdEncoding.EncodeToString(key2)} {
		if _, _, err := parseKEKKeyring(invalid, 1); err == "" {
			t.Fatalf("invalid keyring accepted: %q", invalid)
		}
	}
	if _, _, err := parseKEKKeyring("1:"+base64.StdEncoding.EncodeToString(key1), 2); err == "" {
		t.Fatal("missing current key version should be rejected")
	}
}

func TestLoadRejectsMalformedKEKCurrentVersion(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	t.Setenv("ZINIAO_ENV", "prod")
	t.Setenv("ZINIAO_JWT_SECRET", strings.Repeat("j", 32))
	t.Setenv("ZINIAO_STORE_DRIVER", "postgres")
	t.Setenv("ZINIAO_DEMO_SITE", "false")
	t.Setenv("ZINIAO_KEK_KEYS", "1:"+key)
	t.Setenv("ZINIAO_KEK_CURRENT_VERSION", "not-a-number")
	if err := Load().Validate(); err == nil {
		t.Fatal("malformed current KEK version must fail closed")
	}
}
