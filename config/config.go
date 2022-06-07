package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/niklasfasching/k/jml"
	"github.com/niklasfasching/k/server"
)

type C struct {
	Vars       map[string]interface{}
	User, Host string
	Server     server.Config
	Tunnel     Tunnel
	Apps       map[string]*App `json:"-"`
}

type Tunnel struct {
	Pattern, Address string
}

type App struct {
	Units  Units
	Routes []*server.Route
	Build  string
}

type Units map[string]Unit
type Unit map[string]Section
type Section map[string]interface{}

var kFile = "k.yaml"

func Load(dir string, fns template.FuncMap) (*C, error) {
	c := &C{
		User: "root",
		Tunnel: Tunnel{
			Address: "localhost:9999",
		},
		Server: server.Config{
			HTTP:                 80,
			HTTPS:                443,
			LetsEncryptCachePath: "/var/cache/k-http/autocert-cache",
		},
	}
	bs, err := readTemplate(filepath.Join(dir, kFile), fns, nil)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", kFile, err)
	} else if err := jml.Unmarshal(bs, c); err != nil {
		return nil, err
	}
	c.Apps, err = parseApps(dir, fns, c.Vars)
	if err != nil {
		return nil, err
	}
	return c, err
}

func (c *C) Render(dir string) error {
	for name, a := range c.Apps {
		if err := a.Units.render(dir, name); err != nil {
			return err
		}
	}
	return c.renderInternals(dir)
}

func (c *C) renderInternals(dir string) error {
	sc, reqs := c.Server, []string{}
	for name, a := range c.Apps {
		reqs = append(reqs, name+".target")
		for _, r := range a.Routes {
			if r.LogFields == nil {
				r.LogFields = map[string]string{}
			}
			r.LogFields["K"] = name
			r.LogFields["SYSLOG_IDENTIFIER"] = "k-http"
			sc.Routes = append(sc.Routes, r)
		}
	}
	if c.Tunnel.Pattern != "" {
		sc.Routes = append(sc.Routes, &server.Route{
			Target:    "http://" + c.Tunnel.Address,
			Patterns:  []string{c.Tunnel.Pattern},
			LogFields: map[string]string{"SYSLOG_IDENTIFIER": "k-http"},
		})
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	notifyService := Unit{
		"Service": {
			"ExecStart": fmt.Sprintf("-%s notify --app %%i", exe),
		},
	}
	if err := notifyService.render(dir, "k-notify@.service"); err != nil {
		return err
	}
	httpServer := Units{
		"k-http.socket": {
			"Socket": {
				"ListenStream":       strconv.Itoa(c.Server.HTTP),
				"FileDescriptorName": "http",
				"Service":            "k-http.service",
			},
		},
		"k-https.socket": {
			"Socket": {
				"ListenStream":       strconv.Itoa(c.Server.HTTPS),
				"FileDescriptorName": "https",
				"Service":            "k-http.service",
			},
		},
		"k-http.service": {
			"Service": {
				"ExecStart": fmt.Sprintf(`%s serve ${K_RUN_DIR}/server.json`, exe),
				"Restart":   "always",
			},
		},
	}
	if err := httpServer.render(dir, "k-http"); err != nil {
		return err
	}
	serverConfigPath := filepath.Join(dir, "k", "server.json")
	if bs, err := json.MarshalIndent(sc, "", "  "); err != nil {
		return err
	} else if err := writeFile(serverConfigPath, string(bs)); err != nil {
		return err
	}
	t := Unit{
		"Unit": {
			"After":    "network-online.target",
			"Requires": strings.Join(append(reqs, "k-http.target"), " "),
		},
	}
	if err := t.render(dir, "k.target"); err != nil {
		return err
	}
	return writeSymlink(filepath.Join(dir, "k.target"),
		filepath.Join(dir, "multi-user.target.wants", "k.target"))
}

func (us Units) render(dir, appName string) error {
	target, reqs := appName+".target", []string{}
	for name, u := range us {
		reqs = append(reqs, name)
		u = mergeUnits(Unit{"Unit": {"PartOf": target + " " + "k.target"}}, u)
		if filepath.Ext(name) == ".service" {
			name := strings.TrimSuffix(name, ".service")
			u = mergeUnits(Unit{
				"Service": {
					"SyslogIdentifier": name,
					"LogExtraFields":   "K=" + appName,
					"DynamicUser":      "true",
					"StateDirectory":   name,
					"CacheDirectory":   name,
					"Environment":      fmt.Sprintf("K_RUN_DIR=%s/k", dir),
					"Restart":          "always",
				},
			}, u)
		}
		if err := u.render(dir, name); err != nil {
			return err
		}
	}
	t := Unit{
		"Unit": {
			"Requires":  strings.Join(reqs, " "),
			"OnFailure": "k-notify@%N.service",
		},
	}
	return t.render(dir, target)
}

func (u Unit) render(dir, name string) error {
	um, sections := map[string]string{}, []string{}
	for section, v := range u {
		sections = append(sections, section)
		sm, keys := map[string]string{}, []string{}
		for k, v := range v {
			keys = append(keys, k)
			switch v := v.(type) {
			case nil:
				sm[k] += fmt.Sprintf("%s=\n", k)
			case string, bool, int, float64:
				sm[k] += fmt.Sprintf("%s=%v\n", k, v)
			case []interface{}:
				for _, v := range v {
					switch v := v.(type) {
					case nil:
						continue
					case string, bool, int, float64:
						sm[k] += fmt.Sprintf("%s=%s\n", k, v)
					default:
						return fmt.Errorf("[%s] %s is not string but %T", section, k, v)
					}
				}
			default:
				return fmt.Errorf("[%s] %s is not string but %T", section, k, v)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			um[section] += sm[k]
		}
	}
	sort.Strings(sections)
	s := "# generated by k\n"
	for _, section := range sections {
		s += fmt.Sprintf("[%s]\n", section)
		s += um[section] + "\n"
	}
	return writeFile(filepath.Join(dir, name), s)
}

func parseApps(dir string, fns template.FuncMap, vars interface{}) (map[string]*App, error) {
	fs, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	as := map[string]*App{}
	for _, f := range fs {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if name == "k" {
			continue
		}
		a := &App{}
		bs, err := readTemplate(f, fns, vars)
		if err != nil {
			return nil, err
		} else if err := jml.Unmarshal(bs, a); err != nil {
			return nil, err
		}
		as[name] = a
	}
	return as, nil
}

func mergeUnits(a, b Unit) Unit {
	for name, section := range b {
		if _, ok := a[name]; !ok {
			a[name] = section
		} else {
			for k, v := range section {
				a[name][k] = v
			}
		}
	}
	return a
}

func readTemplate(path string, fns template.FuncMap, v interface{}) ([]byte, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := template.New(path).Funcs(fns).Parse(string(bs))
	if err != nil {
		return nil, err
	}
	w := &bytes.Buffer{}
	err = t.Execute(w, v)
	return w.Bytes(), err
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func writeSymlink(oldname, newname string) error {
	newname, err := filepath.Abs(newname)
	if err != nil {
		return err
	}
	oldname, err = filepath.Abs(oldname)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(newname), os.ModePerm); err != nil {
		return err
	} else if err := os.Remove(newname); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(oldname, newname)
}
