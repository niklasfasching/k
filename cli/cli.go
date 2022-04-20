package cli

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type API map[string]CMD

type CMD struct {
	F         interface{}
	Desc, Doc string
}

var kebabCaseRegexp = regexp.MustCompile(`([a-z]+)([A-Z]+)`)

func (a API) Run(cmd string, args []string) error {
	if c, ok := a[cmd]; ok {
		return c.call(cmd, args)
	}
	return a.usage(cmd)
}

func (a API) Complete() {
	for name, c := range a {
		if c.Doc != "-" && c.Desc != "-" {
			log.Println(name)
		}
	}
}

func (a API) usage(cmd string) error {
	exe, cmds, s := filepath.Base(os.Args[0]), []string{}, ""
	if cmd != "" {
		s += "Unknown command: " + cmd + "\n"
	}
	s += fmt.Sprintf("\nUsage: %s [Command] [Flags] [Args]\n\n", exe)
	for c := range a {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	s += "Commands:\n"
	for _, cmd := range cmds {
		if desc := a[cmd].Desc; desc != "-" {
			s += fmt.Sprintf("\n\t%s\n", cmd)
			if desc != "" {
				s += fmt.Sprintf("\t\t%s\n", desc)
			}
		}
	}
	return fmt.Errorf("%s", s)
}

func (c CMD) call(cmd string, args []string) error {
	ft, cv := reflect.TypeOf(c.F), reflect.ValueOf(cmd)
	av, fv := reflect.ValueOf(struct{}{}), reflect.ValueOf(struct{}{})
	switch ft.NumIn() {
	case 3:
		av, fv = reflect.New(ft.In(1)).Elem(), reflect.New(ft.In(2)).Elem()
	case 2:
		av = reflect.New(ft.In(1)).Elem()
	case 1:
	default:
		return fmt.Errorf("f must be of type func(cmd, ?args, ?flags)")
	}
	args, err := c.parseFlags(fv, args)
	if err != nil {
		return c.usage(cmd, err)
	}
	if err := c.parseArgs(av, args); err != nil {
		return c.usage(cmd, err)
	}
	vs := []reflect.Value{cv, av, fv}
	v := reflect.ValueOf(c.F).Call(vs[:ft.NumIn()])[0].Interface()
	if v == nil {
		return nil
	}
	return v.(error)
}

func (c CMD) parseFlags(fv reflect.Value, args []string) ([]string, error) {
	fs, ft := flag.NewFlagSet("", 0), fv.Type()
	fs.SetOutput(io.Discard)
	n := ft.NumField()
	for i := 0; i < n; i++ {
		ft, v := ft.Field(i), fv.Field(i).Addr().Interface()
		name := kebabCase(ft.Name)
		parts := splitTag(ft)
		usage, fallback := parts[0], ""
		if len(parts) == 2 {
			fallback = parts[1]
		}
		switch ft.Type.Kind() {
		case reflect.String:
			fs.StringVar(v.(*string), name, fallback, usage)
		case reflect.Int:
			i, err := strconv.Atoi(fallback)
			if err != nil {
				return nil, fmt.Errorf("could not parse fallback '%v' as int", fallback)
			}
			fs.IntVar(v.(*int), name, i, usage)
		case reflect.Bool:
			fs.BoolVar(v.(*bool), name, fallback == "true", usage)
		default:
			return nil, fmt.Errorf("%T flags are not supported", fv.Field(i).Interface())
		}
	}
	err := fs.Parse(args)
	return fs.Args(), err
}

func (c CMD) parseArgs(va reflect.Value, args []string) error {
	at := va.Type()
	n := at.NumField()
	if m := len(args); m > n {
		return fmt.Errorf("expected %d arguments but got %d", n, m)
	}
	for i := 0; i < n; i++ {
		ft := at.Field(i)
		tvs := splitTag(ft)
		if i < len(args) {
			va.Field(i).SetString(args[i])
		} else if isOptional := len(tvs) == 2; isOptional {
			va.Field(i).SetString(tvs[0])
		} else {
			return fmt.Errorf("missing required argument <%s>", ft.Name)
		}
	}
	return nil
}

func (c CMD) usage(cmd string, err error) error {
	exe := filepath.Base(os.Args[0])
	s, ft := "", reflect.TypeOf(c.F)
	sx := fmt.Sprintf("Usage: %s %s ", exe, cmd)
	if err != nil {
		sx = fmt.Sprintf("Error:\t%s\n", err) + sx
	}
	if ft.NumIn() == 3 {
		t := ft.In(2)
		s += "\tFlags:"
		sx += "[Flags] "
		for i, n := 0, t.NumField(); i < n; i++ {
			s += fieldUsage(t.Field(i), func(name string, tag []string) string {
				return "--" + kebabCase(name)
			})
		}
	}
	if ft.NumIn() >= 2 {
		t := ft.In(1)
		s += "\tArgs:"
		for i, n := 0, t.NumField(); i < n; i++ {
			s += fieldUsage(t.Field(i), func(name string, tag []string) string {
				if len(tag) == 2 {
					sx += "<?" + name + "> "
				} else {
					sx += "<" + name + "> "
				}
				return name
			})
		}
	}
	if c.Doc != "" {
		s += "Docs:\n\t" + strings.ReplaceAll(strings.TrimSpace(c.Doc), "\n", "\n\t")
	}
	return fmt.Errorf("%s\n\n%s", sx, s)
}

func fieldUsage(f reflect.StructField, nameify func(string, []string) string) string {
	tag := splitTag(f)
	s := fmt.Sprintf("\n\t\t%s", nameify(f.Name, tag))
	if len(tag) == 2 {
		s += fmt.Sprintf("\t(%s\t:: %#v)\n", f.Type, tag[0])
	} else {
		s += fmt.Sprintf("\t(%s)\n", f.Type)
	}
	if len(tag) >= 1 {
		s += fmt.Sprintf("\t\t\t%s\n", tag[0])
	}
	return s
}

func splitTag(t reflect.StructField) []string {
	vs := strings.SplitN(t.Tag.Get("cli"), "::", 2)
	for i, v := range vs {
		vs[i] = strings.TrimSpace(v)
	}
	sort.Sort(sort.StringSlice(vs))
	return vs
}

func kebabCase(s string) string {
	return strings.ToLower(kebabCaseRegexp.ReplaceAllString(s, "${1}-${2}"))
}
