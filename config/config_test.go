package config

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"text/template"
)

func TestConfig(t *testing.T) {
	if err := os.Mkdir("testdata/tmp", os.ModePerm); err != nil {
		t.Fatalf("failed to created tmp dir: %s", err)
	}
	defer os.RemoveAll("testdata/tmp")

	c, err := Load("testdata/config", template.FuncMap{
		"decrypt": func(s string) (string, error) { return s, nil },
	})
	if err != nil {
		t.Fatalf("failed to load config: %s", err)
	}
	if err := c.Render("testdata/tmp", "/usr/bin/echo"); err != nil {
		t.Fatalf("failed to render config: %s", err)
	}
	if os.Getenv("UPDATE") != "" {
		if err := os.RemoveAll("testdata/generated"); err != nil {
			t.Fatalf("failed to remove generated: %s", err)
		}
		if err := os.Rename("testdata/tmp", "testdata/generated"); err != nil {
			t.Fatalf("failed to overwrite generated: %s", err)
		}
		bs, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatalf("failed to jsonify config: %s", err)
		} else if err := os.WriteFile("testdata/generated.json", bs, 0644); err != nil {
			t.Fatalf("failed to write generated.json: %s", err)
		}
		return
	}

	bs, err := os.ReadFile("testdata/generated.json")
	if err != nil {
		t.Fatalf("failed to read generated.json: %s", err)
	}
	c2 := &C{}
	if err := json.Unmarshal(bs, c2); err != nil {
		t.Fatalf("failed to unmarshal generated.json: %s", err)
	}
	if !reflect.DeepEqual(c, c2) {
		t.Fatal("actual config does not match generated.json")
	}
}
