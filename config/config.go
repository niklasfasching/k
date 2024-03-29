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
	"github.com/niklasfasching/k/util"
)

type C struct {
	Dir        string
	Vars       map[string]interface{}
	User, Host string
	Server     server.Config
	Tunnel     Tunnel
	Apps       map[string]*App
}

type Tunnel struct {
	Pattern, Address string
}

type App struct {
	Units         Units
	Routes        []*server.Route
	Build, Deploy *string
	Env           map[string]string
	Dependencies  []string
}

type Units map[string]Unit
type Unit map[string]Section
type Section map[string]any

var kFile = "k.yaml"

func Load(dir string, fns template.FuncMap) (*C, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	c := &C{
		Dir:  dir,
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

func (c *C) Render(dir, exe string) error {
	for name, a := range c.Apps {
		if err := a.Units.render(dir, name); err != nil {
			return err
		}
		if err := c.renderEnvFile(dir, name, a.Env); err != nil {
			return err
		}
	}
	return c.renderInternals(dir, exe)
}

func (c *C) renderEnvFile(dir, appName string, env map[string]string) error {
	s, keys := &strings.Builder{}, []string{}
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(s, "%s=%s\n", k, env[k])
	}
	return writeFile(fmt.Sprintf("%s/k/%s.env", dir, appName), s.String(), 0600)
}

func (c *C) renderInternals(dir, exe string) error {
	sc, reqs := c.Server, []string{}
	for _, r := range c.Server.Routes {
		if r.LogFields == nil {
			r.LogFields = map[string]string{}
		}
		r.LogFields["K"] = "k-custom"
		r.LogFields["SYSLOG_IDENTIFIER"] = "k-custom"
	}
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
				"ExecStart": fmt.Sprintf(`%s serve ${K_CONFIG_DIR}/k/k-http.json`, exe),
				"Restart":   "always",
			},
		},
	}
	if err := httpServer.render(dir, "k-http"); err != nil {
		return err
	}
	if err := writeFile(fmt.Sprintf("%s/k/k-http.env", dir), "", 0600); err != nil {
		return err
	}
	serverConfigPath := filepath.Join(dir, "k", "k-http.json")
	sort.Slice(sc.Routes, func(i, j int) bool { return sc.Routes[i].Target < sc.Routes[j].Target })
	if bs, err := json.MarshalIndent(sc, "", "  "); err != nil {
		return err
	} else if err := writeFile(serverConfigPath, string(bs), 0644); err != nil {
		return err
	}
	sort.Strings(reqs)
	t := Unit{
		"Unit": {
			"After":    "network-online.target",
			"Requires": strings.Join(append(reqs, "k-http.target"), " "),
		},
	}
	if err := t.render(dir, "k.target"); err != nil {
		return err
	}
	return util.WriteSymlink(filepath.Join("..", "k.target"),
		filepath.Join(dir, "multi-user.target.wants", "k.target"))
}

// TODO: use %y for K_CONFIG_DIR in 251/ubuntu 24 https://github.com/systemd/systemd/pull/22195
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
					"LogExtraFields":   []any{"K=" + appName},
					"DynamicUser":      "true",
					"StateDirectory":   name,
					"CacheDirectory":   name,
					"Environment":      []any{"K_CONFIG_DIR=/opt/k/_"},
					"EnvironmentFile":  []any{fmt.Sprintf("/opt/k/_/k/%s.env", appName)},
					"Restart":          "always",
				},
			}, u)
		}
		if err := u.render(dir, name); err != nil {
			return err
		}
	}
	sort.Strings(reqs)
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
			case []any:
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
	return writeFile(filepath.Join(dir, name), s, 0644)
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
		a, err := parseApp(f, name, fns, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		as[name] = a
	}
	for name := range as {
		if err := checkDeps(name, as, map[string]int{name: 1}); err != nil {
			return nil, err
		}
	}
	return as, nil
}

func checkDeps(name string, as map[string]*App, deps map[string]int) error {
	for _, name := range as[name].Dependencies {
		if _, ok := as[name]; !ok {
			return fmt.Errorf("unknown dependency: %q", name)
		} else if deps[name] != 0 {
			return fmt.Errorf("recursive dependency: %q", name)
		}
		deps[name]++
		if err := checkDeps(name, as, deps); err != nil {
			return err
		}
	}
	return nil
}

func parseApp(f, name string, fns template.FuncMap, vars interface{}) (*App, error) {
	a := &App{}
	bs, err := readTemplate(f, fns, vars)
	if err != nil {
		return nil, err
	} else if err := jml.Unmarshal(bs, a); err != nil {
		return nil, err
	}
	if a.Build != nil && a.Deploy != nil {
		return nil, fmt.Errorf(".Build and .Deploy cannot be used in combination")
	}
	return a, nil
}

func mergeUnits(a, b Unit) Unit {
	for name, section := range b {
		if _, ok := a[name]; !ok {
			a[name] = section
		} else {
			for k, v := range section {
				if _, ok := a[name][k].([]any); ok {
					vs, ok := v.([]any)
					if !ok {
						vs = []any{v}
					}
					a[name][k] = append(a[name][k].([]any), vs...)
				} else {
					a[name][k] = v
				}
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

func writeFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}
