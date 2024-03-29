package util

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

type file struct {
	mode          os.FileMode
	path, content string
}

type path struct {
	path    string
	ignored bool
}

func TestGitIgnore(t *testing.T) {
	testGitIgnore(t, "rooted dir", []string{
		"/rooted/",
	}, []path{
		{"foo", false},
		{"bar/baz", false},
		{"rooted", false},
		{"rooted/", true},
		{"rooted/foo", true},
	})

	testGitIgnore(t, "simple single star", []string{
		"/*.foo",
		"*.bar",
	}, []path{
		{"a.foo", true},
		{"foo/b.foo", false},
		{"a.bar", true},
		{"foo/b.bar", true},
		{"foo/baz/b.bar", true},
	})

	testGitIgnore(t, "complex single star", []string{
		"*/baz",
		"/foo/*/foo",
	}, []path{
		{"baz", true}, // ?
		{"foo/baz", true},
		{"bar/baz", true},
		{"foo/bar/bam", false},
		{"foo/bar/foo", true},
		{"foo/baz/foo", true},
		{"foo/bar/bam/foo", false},
	})

	testGitIgnore(t, "dot", []string{
		"*.mkv",
	}, []path{
		{"foo.mkv", true}, // ?
		{"foo/bar.mkv", true},
		{".mkv", false},
		{"xyzmkv", false},
	})
}

func TestCopyReceive(t *testing.T) {
	sync(t, "create", []file{
		{0644, "foo", "bar"},
	}, nil, nil)

	sync(t, "modify,delete", []file{
		{0644, "foo", "bar"},
	}, []file{
		{0777, "foo", ""},
		{0644, "baz", "bam"},
	}, nil)

	sync(t, "gitignore", []file{
		{0644, "foo", "src"},
		{0644, "baz", "src"},
		{0644, ".gitignore", "baz"},
	}, []file{
		{0777, "foo", "dst"},
		{0644, "baz", "dst"},
	}, map[string]bool{
		"baz": true,
	})

	sync(t, "gitignore dir", []file{
		{0644, "foo/bar", "src"},
		{0644, "foo/baz", "src"},
		{0644, "foo/foo", "src"},
		{0644, ".gitignore", "/foo/"},
	}, []file{}, map[string]bool{
		"foo/bar": true,
		"foo/baz": true,
	})
}

func testGitIgnore(t *testing.T, name string, ignorePatterns []string, paths []path) {
	t.Run(name, func(t *testing.T) {
		g, err := NewGitIgnore("/test/", strings.Join(ignorePatterns, "\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if p.ignored != g.IsIgnored("/test/"+strings.TrimPrefix(p.path, "/")) {
				t.Fatalf("expected %q ignore to be %v", p.path, p.ignored)
			}
		}
	})
}

func sync(t *testing.T, name string, srcFs, dstFs []file, ignored map[string]bool) {
	t.Run(name, func(t *testing.T) {
		dir := t.TempDir()
		srcDir, dstDir := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		for _, f := range srcFs {
			path := filepath.Join(srcDir, f.path)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(path, []byte(f.content), f.mode); err != nil {
				t.Fatal(f.path, err)
			}
		}
		sr, sw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer sr.Close()
		defer sw.Close()
		rr, rw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer rr.Close()
		defer rw.Close()
		r := NewPipe(sr, rw)
		s := NewPipe(rr, sw)
		g, cancel := errgroup.Group{}, func() {
			time.Sleep(100 * time.Millisecond)
			_, _, _, _ = sw.Close(), sr.Close(), rw.Close(), rr.Close()
		}
		g.Go(func() error {
			defer cancel()
			if _, err := s.Send(srcDir, dstDir); err != nil {
				return fmt.Errorf("send: %w", err)
			}
			return nil
		})
		g.Go(func() error {
			defer cancel()
			if err := r.Receive(); err != nil {
				return fmt.Errorf("receive: %w", err)
			}
			return nil
		})
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		fs := []file{}
		err = filepath.Walk(dstDir, func(path string, fi os.FileInfo, err error) error {
			if fi.IsDir() {
				return nil
			}
			rpath, err := filepath.Rel(dstDir, path)
			if err != nil {
				return err
			}
			bs, err := os.ReadFile(path)
			fs = append(fs, file{fi.Mode(), rpath, string(bs)})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		expFs := []file{}
		for _, f := range srcFs {
			if !ignored[f.path] {
				expFs = append(expFs, f)
			}
		}
		if len(fs) != len(expFs) {
			t.Fatalf("file count mismatch: %v != %v", len(fs), len(expFs))
		}
		sort.Slice(fs, func(i, j int) bool { return fs[i].path < fs[j].path })
		sort.Slice(expFs, func(i, j int) bool { return expFs[i].path < expFs[j].path })
		for i := range fs {
			if fs[i] != expFs[i] {
				t.Fatalf("file mismatch: %v != %v", fs[i], expFs[i])
			}
		}
	})
}
