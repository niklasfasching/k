package jml

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJML(t *testing.T) {
	fs, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if filepath.Ext(f.Name()) != ".yaml" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			bs, err := os.ReadFile(filepath.Join("testdata", f.Name()))
			if err != nil {
				t.Fatal(err)
			}
			v, s := interface{}(nil), ""
			err = Unmarshal(bs, &v)
			if err != nil {
				t.Log("unmarshal", err)
				s = "unmarshal: " + err.Error()
			} else if bs, err := json.MarshalIndent(v, "", "  "); err != nil {
				t.Fatal(err)
			} else {
				s = string(bs)
			}
			compare(t, filepath.Join("testdata", f.Name()+".json"), s)
			if err == nil {
				bs, err := Marshal(v)
				s = string(bs)
				if err != nil {
					t.Log("marshal", err)
					s = "marshal: " + err.Error()
				}
			}
			compare(t, filepath.Join("testdata", f.Name()+".jml"), s)
		})
	}
}

func compare(t *testing.T, path, actual string) {
	if os.Getenv("UPDATE") != "" {
		t.Log("updating", path)
		if err := os.WriteFile(path, []byte(actual), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		expectedBS, _ := os.ReadFile(path)
		if actual != string(expectedBS) {
			t.Fatal("actual and expected output do not match")
		}
	}
}
