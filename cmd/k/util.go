package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"text/template"
	"time"

	"github.com/niklasfasching/k/config"
	"github.com/niklasfasching/k/util"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
)

func remoteInstallBinary(c *ssh.Client, binPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return err
	}
	mt, _ := util.SSHExec(c, fmt.Sprintf("stat -c %%Y %q 2> /dev/null", binPath), true)
	if mt >= fmt.Sprintf("%d", fi.ModTime().Unix()) {
		return nil
	}
	if err := util.SCP(c, exe, binPath); err != nil {
		return err
	}
	_, err = util.SSHExec(c, fmt.Sprintf(`
      mkdir -p /usr/local/lib/systemd/system-generators/
      ln -sf %q /usr/local/lib/systemd/system-generators/k-generator`, binPath), true)
	return err
}

func getBuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "-"
	}
	revision, dirty := "", ""
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value[:6]
		} else if s.Key == "vcs.modified" && s.Value == "true" {
			dirty = fmt.Sprintf("-dirty-%d", time.Now().UnixNano())
		}
	}
	return fmt.Sprintf("%s%s", revision, dirty)
}

func loadConfig() (*config.C, error) {
	v, err := util.OpenVault(root.VaultKeyFile(), root.IsClient())
	if err != nil {
		return nil, err
	}
	c, err := config.Load(root.ConfigDir(), template.FuncMap{"decrypt": v.Decrypt})
	if err != nil {
		return nil, err
	}
	if os.Getenv("DEV") != "" {
		c.User, c.Host = "root", "localhost"
	}
	return c, nil
}

func getAppName(c *config.C, name string) (string, error) {
	if name != "" && c.Apps[name] == nil && name != filepath.Base(c.Dir) {
		return "", fmt.Errorf("unknown app %q", name)
	} else if name != "" {
		return name, nil
	} else if aDir, err := os.Getwd(); err != nil {
		return "", err
	} else if aRelDir, err := filepath.Rel(filepath.Dir(c.Dir), aDir); err != nil {
		return "", err
	} else {
		return filepath.Base(aRelDir), nil
	}
}

func sendTelegramMessage(botId, token, chatId, text string) error {
	body, err := json.Marshal(map[string]string{"chat_id": chatId, "text": text})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/%s:%s/sendMessage", botId, token)
	r, err := http.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	bs, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	v := struct{ Ok bool }{}
	if err := json.Unmarshal(bs, &v); err != nil || !v.Ok {
		return fmt.Errorf("error sending message: %s", string(bs))
	}
	return nil
}

func sync(sc *ssh.Client, srcDir, dstDir string) (int, error) {
	s, err := sc.NewSession()
	if err != nil {
		return 0, err
	}
	defer s.Close()
	r, err := s.StderrPipe()
	if err != nil {
		return 0, err
	}
	go func() { io.Copy(os.Stderr, r) }()
	rw, err := s.StdinPipe()
	if err != nil {
		return 0, err
	}
	rr, err := s.StdoutPipe()
	if err != nil {
		return 0, err
	}
	g, n, cancel := errgroup.Group{}, 0, func() { s.Close() }
	g.Go(func() (err error) {
		defer cancel()
		n, err = util.NewPipe(rr, rw).Send(srcDir, dstDir)
		return err
	})
	g.Go(func() (err error) {
		defer cancel()
		return s.Run(fmt.Sprintf("%s receive", serverBin))
	})
	return n, g.Wait()
}

func completeApps(args []string) []string {
	completions := []string{}
	c, err := loadConfig()
	if err != nil {
		return nil
	}
	for name := range c.Apps {
		completions = append(completions, name)
	}
	return completions
}

func renderConfig(c *config.C, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	} else if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	return c.Render(dir, serverBin)
}

func syncConfig(sc *ssh.Client, c *config.C) error {
	dir := filepath.Join(string(root), "tmp")
	defer func() { os.RemoveAll(dir) }()
	if err := renderConfig(c, dir); err != nil {
		return err
	} else if n, err := sync(sc, dir, serverRoot.ConfigDir()); err != nil {
		return err
	} else if n != 0 {
		cmd := `set -x; systemctl daemon-reload && systemctl restart k-http.target`
		_, err := util.SSHExec(sc, cmd, false)
		return err
	}
	return nil
}

func deployApp(sc *ssh.Client, c *config.C, name string) error {
	a, aDir := c.Apps[name], filepath.Join(c.Dir, "..", name)
	if a.Deploy != nil {
		return fmt.Errorf("TODO: reimplement a.Deploy")
	}
	for _, name := range a.Dependencies {
		if err := deployApp(sc, c, name); err != nil {
			return err
		}
	}
	if _, err := sync(sc, aDir, filepath.Join(string(serverRoot), name)); err != nil {
		return err
	}
	cmd := fmt.Sprintf("cd %q; set -x;\n", filepath.Join(string(serverRoot), name))
	if a.Build != nil {
		cmd += *a.Build + "\n"
	}
	cmd += fmt.Sprintf(`systemctl restart %s.target`, name)
	_, err := util.SSHExec(sc, cmd, false)
	return err
}
