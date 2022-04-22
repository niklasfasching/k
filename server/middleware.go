package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

type BasicAuth struct {
	User     string
	Password string
	Realm    string
}

type fs struct{ http.FileSystem }

type responseWriter struct {
	status   int
	count    int
	errPaths map[int]string
	req      *http.Request
	http.ResponseWriter
}

var journalSocket = &net.UnixAddr{Name: "/run/systemd/journal/socket", Net: "unixgram"}
var journalConnection *net.UnixConn

var ipv4Mask = net.CIDRMask(16, 32)  // 255.255.0.0
var ipv6Mask = net.CIDRMask(56, 128) // ffff:ffff:ffff:ff00::
var commonLogFormat = `{{ .remote }} - {{ .userAgent }} [{{ .timestamp }}] "{{ .method }} {{ .host }}{{ .url }} {{ .proto }}" {{ .status }} {{ .size }}`

func LogHandler(next http.Handler, format string,
	fields map[string]string, errPaths map[int]string) (http.Handler, error) {
	fmt, err := newLogFormatter(format)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw, timestamp := &responseWriter{ResponseWriter: w, req: r}, time.Now()
		next.ServeHTTP(rw, r)
		journalLog(fmt(map[string]interface{}{
			"remote":    maskIP(r.RemoteAddr),
			"userAgent": r.UserAgent(),
			"timestamp": timestamp.Format(time.RFC3339),
			"proto":     r.Proto,
			"method":    r.Method,
			"host":      r.Host,
			"url":       r.URL,
			"status":    rw.status,
			"size":      rw.count,
		}), "6", fields)
	}), nil
}

func StaticHandler(root string) (http.Handler, error) {
	if f, err := os.Stat(root); err != nil || !f.IsDir() {
		return nil, fmt.Errorf("root must be a directory: %s", err)
	}
	fs := &fs{http.FileSystem(http.Dir(root))}
	return http.FileServer(fs), nil
}

func ProxyHandler(uri string) (http.Handler, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	return httputil.NewSingleHostReverseProxy(u), nil
}

func (ba *BasicAuth) Handler(next http.Handler) http.Handler {
	eq := func(s1, s2 string) bool { return subtle.ConstantTimeCompare([]byte(s1), []byte(s2)) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || !eq(user, ba.User) || !eq(password, ba.Password) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, ba.Realm))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (fs *fs) Open(name string) (http.File, error) {
	f, err := fs.FileSystem.Open(name)
	if err != nil {
		return nil, err
	} else if s, err := f.Stat(); err != nil {
		return nil, err
	} else if !s.IsDir() {
		return f, nil
	} else if f2, err := fs.FileSystem.Open(filepath.Join(name, "index.html")); err != nil {
		return nil, err
	} else if err := f2.Close(); err != nil {
		return nil, err
	}
	return f, nil
}

func (r *responseWriter) Write(bytes []byte) (count int, err error) {
	count, err = r.ResponseWriter.Write(bytes)
	r.count += count
	return count, err
}

func (r *responseWriter) WriteHeader(status int) {
	errPath, ok := r.errPaths[status]
	if !ok {
		errPath = r.errPaths[0]
	}
	if errPath == "" {
		r.ResponseWriter.WriteHeader(status)
	} else {
		http.Redirect(r.ResponseWriter, r.req, errPath, http.StatusTemporaryRedirect)
	}
	r.status = status
}

func newLogFormatter(format string) (func(interface{}) string, error) {
	if format == "" {
		format = commonLogFormat
	}
	logTemplate, err := template.New("logFormat").Parse(format)
	return func(data interface{}) string {
		s := &strings.Builder{}
		if err := logTemplate.Execute(s, data); err != nil {
			panic(err)
		}
		return s.String()
	}, err
}

func maskIP(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return "-"
	}
	ip := net.ParseIP(host)
	if ip.To4() != nil {
		return ip.Mask(ipv4Mask).String()
	}
	return ip.Mask(ipv6Mask).String()
}

// https://systemd.io/JOURNAL_NATIVE_PROTOCOL/
// http://www.freedesktop.org/software/systemd/man/systemd.journal-fields.html
func journalLog(msg, priority string, kvs map[string]string) error {
	if journalConnection == nil {
		c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram"})
		if err != nil {
			return err
		}
		journalConnection = c
	}
	w := &bytes.Buffer{}
	writeJournalKV(w, "MESSAGE", msg)
	writeJournalKV(w, "PRIORITY", priority)
	for k, v := range kvs {
		writeJournalKV(w, k, v)
	}
	_, _, err := journalConnection.WriteMsgUnix(w.Bytes(), nil, journalSocket)
	return err
}

func writeJournalKV(w io.Writer, k, v string) {
	if !strings.ContainsRune(v, '\n') {
		fmt.Fprintf(w, "%s=%s\n", k, v)
	} else {
		fmt.Fprint(w, k)
		binary.Write(w, binary.LittleEndian, uint64(len(v)))
		fmt.Fprint(w, v)
	}
}
