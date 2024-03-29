package util

import (
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Pipe struct {
	io.Reader
	io.Writer
	*gob.Decoder
	*gob.Encoder
}

type Message struct {
	Path string
	File
	Files map[string]File
}

type File struct {
	Mode os.FileMode
	Size int64
	SHA  string
}

type GitIgnore []*Pattern
type Pattern struct {
	Re      *regexp.Regexp
	Negated bool
}

var patternRep = strings.NewReplacer(
	".", "[.]", "*", "[^/]+", "**", ".*",
	"///", "/", "//", "/",
)

func NewPipe(r io.Reader, w io.Writer) *Pipe {
	return &Pipe{r, w, gob.NewDecoder(r), gob.NewEncoder(w)}
}

func (p *Pipe) Receive() error {
	dir, n := "", 0
	if err := p.Decode(&dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	lm, err := p.Walk(dir)
	if err != nil {
		return err
	}
	rm, missing := map[string]File{}, []string{}
	if err := p.Decode(&rm); err != nil {
		return err
	}
	for path, fr := range rm {
		if fl, ok := lm[path]; (!ok || fr.SHA != fl.SHA) && fr.Mode&fs.ModeSymlink != 0 {
			n++
			apath := filepath.Join(dir, path)
			if err := os.RemoveAll(apath); err != nil {
				return err
			} else if err := os.MkdirAll(filepath.Dir(apath), 0755); err != nil {
				return err
			} else if err := os.Symlink(fr.SHA, apath); err != nil {
				return err
			}
		} else if !ok || fr.SHA != fl.SHA {
			n++
			missing = append(missing, path)
		} else if ok && fr.Mode != fl.Mode {
			n++
			if err := os.Chmod(filepath.Join(dir, path), fr.Mode); err != nil {
				return err
			}
		}
	}
	if err := p.Encode(missing); err != nil {
		return err
	}
	for _, path := range missing {
		f, apath := rm[path], filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(apath), 0755); err != nil {
			return err
		} else if err := p.receiveFile(apath, f.Mode, f.Size); err != nil {
			return err
		}
	}
	for path := range lm {
		if _, ok := rm[path]; !ok {
			n++
			if err := os.Remove(filepath.Join(dir, path)); err != nil {
				return err
			}
		}
	}
	return p.Encode(n)
}

func (p *Pipe) Send(localDir, remoteDir string) (int, error) {
	if err := p.Encode(remoteDir); err != nil {
		return 0, err
	}
	m, err := p.Walk(localDir)
	if err != nil {
		return 0, err
	} else if err := p.Encode(m); err != nil {
		return 0, err
	}
	missing, n := []string{}, 0
	if err := p.Decode(&missing); err != nil {
		return 0, err
	}
	for _, path := range missing {
		start := time.Now()
		if err := p.sendFile(filepath.Join(localDir, path), m[path].Size); err != nil {
			return 0, err
		}
		log.Println("SendFile", filepath.Join(localDir, path), remoteDir, time.Now().Sub(start))
	}
	return n, p.Decode(&n)
}

// TODO: could use modtime rather than sha for comparison
func (p *Pipe) Walk(dir string) (map[string]File, error) {
	m, g, h := map[string]File{}, GitIgnore{}, sha256.New()
	err := g.Walk(dir, func(p string, e os.DirEntry) error {
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rp, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		mode, size, sha := fi.Mode(), fi.Size(), ""
		if mode&fs.ModeSymlink == 0 {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			h.Reset()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
			sha = string(h.Sum(nil))
		} else {
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			sha = l
		}
		m[rp] = File{mode, size, sha}
		return nil
	})
	return m, err
}

func (p *Pipe) receiveFile(path string, m os.FileMode, n int64) error {
	if m&fs.ModeSymlink != 0 {
		log.Println("YOLOOOOO", path)
		w := &strings.Builder{}
		i, err := io.CopyN(w, p, n)
		log.Println(w.String(), i, err)
		log.Println("YOLOOOOO", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, m)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := io.CopyN(f, p, n)
	if err != nil {
		return err
	} else if i != n {
		return fmt.Errorf("bad file copy: %q %v != %v", path, i, n)
	}
	return nil
}

func (p *Pipe) sendFile(path string, n int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := io.Copy(p, f)
	if err != nil {
		return err
	} else if i != n {
		return fmt.Errorf("bad file copy: %q %v != %v", path, i, n)
	}
	return nil
}

func NewGitIgnore(dir, content string) (g GitIgnore, err error) {
	for _, l := range strings.Split(content, "\n") {
		l, isNegated := strings.TrimSpace(l), false
		if strings.Contains(l, `\`) || strings.Contains(l, "**") {
			return nil, fmt.Errorf("only simple patterns are supported: %q", l)
		} else if l == "" || strings.HasPrefix(l, `#`) {
			continue
		}
		if l[0] == '!' {
			isNegated, l = true, l[1:]
		}
		if l[0] == '/' {
			l = "/" + dir + "/" + l
		}
		r, err := regexp.Compile(patternRep.Replace(l))
		if err != nil {
			return nil, err
		}
		g = append(g, &Pattern{r, isNegated})
	}
	return g, nil
}

func (g *GitIgnore) IsIgnored(path string) bool {
	path, ignore := strings.ReplaceAll(path, string(os.PathSeparator), "/"), false
	if filepath.Base(path) == ".git" {
		return true
	} else if g == nil {
		return false
	}
	for _, p := range *g {
		if p.Re.MatchString(path) {
			ignore = !p.Negated
		}
	}
	return ignore
}

func (g GitIgnore) Walk(dir string, f func(string, os.DirEntry) error) error {
	fis, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		if fi.Name() == ".gitignore" {
			bs, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
			if err != nil {
				return err
			}
			_g, err := NewGitIgnore(dir, string(bs))
			if err != nil {
				return err
			}
			g = append(g, _g...)
			break
		}
	}
	for _, fi := range fis {
		path := filepath.Join(dir, fi.Name())
		if g.IsIgnored(path) {
			continue
		} else if fi.IsDir() {
			g.Walk(path, f)
		} else {
			f(path, fi)
		}
	}
	return nil
}
