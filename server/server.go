package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/niklasfasching/k/util"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	HTTP, HTTPS          int
	LetsEncryptEmail     string
	LetsEncryptCachePath string
	Routes               []*Route
}

type Route struct {
	Patterns  []string
	Target    string
	BasicAuth BasicAuth
	LogFormat string
	LogFields map[string]string
	ErrPaths  map[int]string
}

func Start(configPath string) error {
	c, err := readConfig(configPath)
	if err != nil {
		return err
	}
	return c.Start()
}

func (c *Config) Start() error {
	handler, hostnames, err := c.getHandlerAndHostnames()
	if err != nil {
		return err
	}
	if isHTTPS := c.LetsEncryptEmail != ""; !isHTTPS {
		log.Println("LetsEncryptEmail not set - only listening for http")
		log.Printf("Listening on :%d", c.HTTP)
		return c.serve(&http.Server{Handler: handler})
	}
	g := errgroup.Group{}
	m := autocert.Manager{
		Prompt:     func(string) bool { return c.LetsEncryptEmail != "" },
		Email:      c.LetsEncryptEmail,
		Cache:      autocert.DirCache(c.LetsEncryptCachePath),
		HostPolicy: autocert.HostWhitelist(hostnames...),
	}
	g.Go(func() error {
		autocertHandler := m.HTTPHandler(nil)
		return c.serve(&http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") {
					handler.ServeHTTP(w, r)
					return
				}
				autocertHandler.ServeHTTP(w, r)
			}),
		})
	})
	g.Go(func() error {
		return c.serve(&http.Server{Handler: handler, TLSConfig: m.TLSConfig()})
	})
	log.Printf("Listening on :%d and :%d", c.HTTP, c.HTTPS)
	return g.Wait()
}

func (c *Config) serve(s *http.Server) error {
	ls, err := listenFDs()
	if err != nil {
		return err
	}
	if s.TLSConfig != nil {
		l := ls["https"]
		if l == nil {
			l, err = net.Listen("tcp", fmt.Sprintf(":%d", c.HTTPS))
			if err != nil {
				return err
			}
		}
		return s.ServeTLS(l, "", "")
	}
	l := ls["http"]
	if l == nil {
		l, err = net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP))
		if err != nil {
			return err
		}
	}
	return s.Serve(l)
}

func (c *Config) getHandlerAndHostnames() (http.Handler, []string, error) {
	mux, hostnames := http.NewServeMux(), []string{}
	for _, r := range c.Routes {
		h, err := r.Handler()
		if err != nil {
			util.JournalLog(fmt.Sprintf("bad route [%v]: %s", r.Patterns, err), "1", r.LogFields)
			continue
		}
		for _, pattern := range r.Patterns {
			parts := strings.SplitN(pattern, "/", 2)
			if len(parts) < 2 {
				return nil, nil, fmt.Errorf("pattern must be either {hostname}/... or /...: %s", pattern)
			}
			if hostname := parts[0]; hostname != "" {
				hostnames = append(hostnames, hostname)
			}
			mux.Handle(pattern, http.StripPrefix("/"+parts[1], h))
		}
	}
	return mux, hostnames, nil
}

func (r *Route) Handler() (http.Handler, error) {
	h, err := http.Handler(nil), error(nil)
	if strings.HasPrefix(r.Target, "/") {
		h, err = StaticHandler(r.Target)
	} else {
		h, err = ProxyHandler(r.Target)
	}
	if err != nil {
		return nil, err
	}
	h, err = LogHandler(h, r.LogFormat, r.LogFields, r.ErrPaths)
	if err != nil {
		return nil, err
	}
	if r.BasicAuth != (BasicAuth{}) {
		h = r.BasicAuth.Handler(h)
	}
	return h, err
}

func readConfig(path string) (*Config, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	return c, json.Unmarshal(bs, c)
}

// https://www.freedesktop.org/software/systemd/man/sd_listen_fds.html
func listenFDs() (map[string]net.Listener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n == 0 {
		return nil, nil
	}
	ls, names := map[string]net.Listener{}, strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	for i := 0; i < n; i++ {
		fd := 3 + i
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), names[i])
		if f == nil {
			continue
		}
		l, err := net.FileListener(f)
		if err != nil {
			return nil, err
		}
		ls[names[i]] = l
	}
	return ls, nil
}
