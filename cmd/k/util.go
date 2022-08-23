package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/template"
	"time"

	"github.com/niklasfasching/k/config"
	"github.com/niklasfasching/k/util"
	"golang.org/x/crypto/ssh"
)

func remoteInstallBinary(c *ssh.Client, binPath string) error {
	if rv, err := util.SSHExec(c, fmt.Sprintf("%s version || true", binPath), nil, true); err != nil {
		return err
	} else if v := getBuildVersion(); rv != v || v == "" {
		if !strings.Contains(rv, "not found") {
			log.Printf("k version mismatch: client='%s', server='%s'", v, rv)
		}
		log.Println("Copying k binary to server...")
		if exe, err := os.Executable(); err != nil {
			return err
		} else if err := util.SCP(c, exe, binPath); err != nil {
			return err
		}
	}
	_, err := util.SSHExec(c, `
      mkdir -p /etc/systemd/system-generators
      ln -drsf $k /etc/systemd/system-generators/k-generator`,
		map[string]string{"k": binPath}, false)
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

func loadConfig() (*config.C, string, error) {
	dir := filepath.Join(root, configDir)
	if v := os.Getenv("K_DIR"); v != "" {
		dir = v
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, "", fmt.Errorf("config: %w", err)
	}
	v := util.Vault(nil)
	c, err := config.Load(dir, template.FuncMap{
		"decrypt": func(s string) (string, error) {
			if err := error(nil); v == nil {
				v, err = util.OpenVault(filepath.Join(root, ".key"), root == clientRoot)
				if err != nil {
					return "", err
				}
			}
			return v.Decrypt(s)
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("config: %w", err)
	}
	if os.Getenv("DEV") != "" {
		c.User, c.Host = "root", "localhost"
	}
	return c, dir, nil
}

func gitPush(c *config.C, dir, remote string) error {
	if err := assertGitClean(dir); err != nil {
		return err
	}
	log.Printf("Pushing %s:", filepath.Base(dir))
	url := fmt.Sprintf("ssh://%s@%s:/receive/%s", c.User, c.Host, filepath.Join(serverRoot, remote))
	// push to non-existant remote to ensure git hooks run even when nothing changed
	_, err := util.Exec(`cd $dir && git push "$url" --receive-pack="$exe" master:$(date +%s) --force`,
		map[string]string{"dir": dir, "url": url, "exe": serverBin}, false)
	return err
}

func assertGitClean(dir string) error {
	if fs, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fs.IsDir() {
		return fmt.Errorf("%s is not a git repository", filepath.Base(dir))
	} else if out, err := util.Exec(`[[ -z $(cd $dir && git status --porcelain) ]] && echo clean`,
		map[string]string{"dir": dir}, true); err != nil || out != "clean" {
		return fmt.Errorf("%s has uncommitted changes", filepath.Base(dir))
	}
	return nil
}

func getAppName() (string, error) {
	if cDir, err := filepath.EvalSymlinks(filepath.Join(root, configDir)); err != nil {
		return "", err
	} else if aDir, err := os.Getwd(); err != nil {
		return "", err
	} else if aRelDir, err := filepath.Rel(filepath.Dir(cDir), aDir); err != nil {
		return "", err
	} else {
		return strings.Split(aRelDir, string(os.PathSeparator))[0], nil
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
