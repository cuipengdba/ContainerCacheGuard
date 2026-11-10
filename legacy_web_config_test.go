package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewConfigurationOmitsWebSettings(t *testing.T) {
	b, err := json.Marshal(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"web_listen", "web_token_file", "web_history_samples"} {
		if _, ok := fields[key]; ok {
			t.Fatal("new configuration still advertises Web service", key)
		}
	}
}

func TestLegacyWebSettingsAreAcceptedAndIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(p, []byte(`{"web_listen":"invalid-address","web_token_file":"unreadable-relative-token","web_history_samples":-1}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	c.StateFile = filepath.Join(t.TempDir(), "status.json")
	if err = c.validate(); err != nil {
		t.Fatal("inactive Web values blocked upgrade", err)
	}
	if !reflect.DeepEqual(c.deprecatedWebFields(), []string{"web_listen", "web_token_file", "web_history_samples"}) {
		t.Fatal(c.deprecatedWebFields())
	}
}
