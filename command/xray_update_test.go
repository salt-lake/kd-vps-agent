//go:build xray

package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestMergeXrayConfig_AddsNewField(t *testing.T) {
	p := writeTempConfig(t, `{"log":{"level":"info"}}`)
	if err := mergeXrayConfig(p, `{"inbounds":[{"port":443}]}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(p)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	if _, ok := got["log"]; !ok {
		t.Error("expected existing 'log' field preserved")
	}
	if _, ok := got["inbounds"]; !ok {
		t.Error("expected new 'inbounds' field added")
	}
}

func TestMergeXrayConfig_OverwritesExistingField(t *testing.T) {
	p := writeTempConfig(t, `{"log":{"level":"info"},"inbounds":["old"]}`)
	if err := mergeXrayConfig(p, `{"inbounds":[{"port":443}]}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(p)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(data, &got)

	var inbounds []map[string]int
	if err := json.Unmarshal(got["inbounds"], &inbounds); err != nil {
		t.Fatalf("expected inbounds to be patched array, got %s", got["inbounds"])
	}
	if len(inbounds) != 1 || inbounds[0]["port"] != 443 {
		t.Errorf("expected [{port:443}], got %v", inbounds)
	}
}

func TestMergeXrayConfig_PreservesUntouchedFields(t *testing.T) {
	original := `{"log":{"level":"info"},"dns":{"servers":["1.1.1.1"]},"outbounds":[{"protocol":"freedom"}]}`
	p := writeTempConfig(t, original)
	if err := mergeXrayConfig(p, `{"inbounds":[{"port":443}]}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(p)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(data, &got)

	for _, k := range []string{"log", "dns", "outbounds", "inbounds"} {
		if _, ok := got[k]; !ok {
			t.Errorf("expected key %q preserved/added", k)
		}
	}
}

func TestMergeXrayConfig_MissingFile(t *testing.T) {
	if err := mergeXrayConfig("/nonexistent/path/config.json", `{}`); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestMergeXrayConfig_InvalidExistingJSON(t *testing.T) {
	p := writeTempConfig(t, `not json at all`)
	if err := mergeXrayConfig(p, `{}`); err == nil {
		t.Fatal("expected error for invalid existing config")
	}
}

func TestMergeXrayConfig_InvalidPatchJSON(t *testing.T) {
	p := writeTempConfig(t, `{"log":{}}`)
	if err := mergeXrayConfig(p, `not json`); err == nil {
		t.Fatal("expected error for invalid patch")
	}
}
