package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	ex "os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/niklasfasching/k/cli"
	"github.com/niklasfasching/k/server"
	"github.com/niklasfasching/k/util"
)

var api = cli.API{
	"ls":       {F: ls, Desc: "list all apps"},
	"start":    {F: systemctl, Desc: "systemctl start", Complete: completeApps},
	"stop":     {F: systemctl, Desc: "systemctl stop", Complete: completeApps},
	"reload":   {F: systemctl, Desc: "systemctl reload", Complete: completeApps},
	"tunnel":   {F: tunnel, Desc: "tunnel <address>:<remote_address>"},
	"restart":  {F: systemctl, Desc: "systemctl restart", Complete: completeApps},
	"status":   {F: systemctl, Desc: `show status of app - equivalent to systemctl status`, Complete: completeApps},
	"logs":     {F: systemctl, Desc: "journalctl K=<app>", Complete: completeApps},
	"notify":   {F: notify, Desc: "send message to k.Vars.telegram $bot_id:$token:$chat_id"},
	"version":  {F: version},
	"init":     {F: initConfig, Desc: "set up the provided config <dir>"},
	"deploy":   {F: deploy, Desc: "git push config & app repo's", Complete: completeApps},
	"encrypt":  {F: encrypt, Desc: "encrypt the provided <value>"},
	"decrypt":  {F: decrypt, Desc: "decrypt the provided <value>"},
	"sign":     {F: sign, Desc: "sign the provided <file>"},
	"generate": {F: generate, Desc: "-"},
	"receive":  {F: receive, Desc: "-"},
	"update":   {F: update, Desc: "-"},
	"serve":    {F: serve, Desc: "-"},
}

var serverRoot = "/opt/k/"
var clientRoot = os.ExpandEnv("$HOME/.config/k/")
var root = clientRoot
var serverBin = filepath.Join(serverRoot, ".k")
var configDir = ".config"
var keyFile = ".key"
var signKeyFile = ".signKey"

func init() {
	if kRoot := os.Getenv("K_ROOT"); kRoot != "" {
		root = kRoot
	} else if isServer := os.Getenv("DISPLAY") == ""; isServer {
		root = serverRoot
	}
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	cmd, args := "", os.Args[1:]
	if dir, file := filepath.Split(os.Args[0]); strings.Contains(file, "generator") {
		cmd = "generate"
	} else if filepath.Base(dir) == "hooks" && file == "update" {
		cmd = "update"
	} else if len(args) == 1 && strings.HasPrefix(args[0], "/receive/") {
		cmd, args[0] = "receive", strings.TrimPrefix(args[0], "/receive")
	} else if len(args) >= 1 {
		cmd, args = args[0], args[1:]
	}
	if err := api.Run(cmd, args); err != nil {
		log.Fatal(err)
	}
}

func encrypt(cmd string, args struct{ PlainText string }) error {
	v, err := util.OpenVault(filepath.Join(root, keyFile), true)
	if err != nil {
		return err
	}
	s, err := v.Encrypt(args.PlainText)
	if err != nil {
		return err
	}
	log.Printf(`{{ decrypt "%s" }}`, s)
	return nil
}

func decrypt(cmd string, args struct{ CipherText string }) error {
	m := regexp.MustCompile(`([\w/=]{24,})`).FindStringSubmatch(args.CipherText)
	if m == nil {
		return fmt.Errorf("arg does not contain an encrypted value")
	}
	v, err := util.OpenVault(filepath.Join(root, keyFile), true)
	if err != nil {
		return err
	}
	s, err := v.Decrypt(m[1])
	if err != nil {
		return err
	}
	log.Printf(s)
	return nil
}

func sign(cmd string, args struct{ File, SigFile string }) error {
	keyFile := filepath.Join(root, signKeyFile)
	kbs, err := os.ReadFile(keyFile)
	if err != nil {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		if err := os.WriteFile(keyFile, k, 0600); err != nil {
			return fmt.Errorf("write key: %w", err)
		}
		kbs = k
	}
	k := ed25519.PrivateKey(kbs)
	bs, err := os.ReadFile(args.File)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	sig := ed25519.Sign(k, bs)
	log.Printf("signature: %x\npublic key: %x\n  (verify with ed25519.Verify)", sig, k.Public())
	return os.WriteFile(args.SigFile, sig, 0644)
}

func deploy(cmd string,
	as struct {
		App string `cli:"::"`
	}) error {
	c, cDir, err := loadConfig()
	if err != nil {
		return err
	}
	name := as.App
	if as.App == "" {
		if name, err = getAppName(); err != nil {
			return err
		}
	}
	if name != filepath.Base(cDir) && c.Apps[name] == nil {
		return fmt.Errorf("'%s' is not a valid app", name)
	}
	s, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := remoteInstallBinary(s, serverBin); err != nil {
		return err
	} else if err := util.SCP(s,
		filepath.Join(clientRoot, keyFile),
		filepath.Join(serverRoot, keyFile)); err != nil {
		return err
	}
	if err := gitPush(c, cDir, configDir); err != nil {
		return err
	}
	if name == filepath.Base(cDir) {
		return nil
	}
	if a := c.Apps[name]; a.Deploy != nil {
		remoteDir := filepath.Join(serverRoot, name)
		if _, err := util.SSHExec(s, fmt.Sprintf(`mkdir -p %s`, remoteDir), nil, false); err != nil {
			return err
		} else if _, err := util.Exec(`set -x;`+*a.Deploy, nil, false); err != nil {
			return err
		}
		_, err := util.SSHExec(s, fmt.Sprintf(`systemctl restart %s.target`, name), nil, false)
		return err
	}
	return gitPush(c, filepath.Join(cDir, "..", name), name)
}

func systemctl(cmd string, x struct {
	App string `cli:"::defaults to $cwd app"`
}) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	unit := x.App
	if unit == "" {
		if unit, err = getAppName(); err != nil {
			return err
		}
	}
	s, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer s.Close()
	script := fmt.Sprintf("systemctl %s %s", cmd, unit)
	if cmd == "logs" {
		script = fmt.Sprintf("journalctl K=%s", unit)
	} else if cmd == "status" {
		if filepath.Ext(unit) == "" {
			script += ".target"
		}
		script += " --with-dependencies --lines 100"
	}
	_, err = util.SSHExec(s, "SYSTEMD_COLORS=1 "+script, nil, false)
	return err
}

func initConfig(cmd string, x struct{ Dir string }) error {
	dir, err := filepath.Abs(x.Dir)
	if err != nil {
		return err
	} else if xs, err := os.ReadDir(dir); !os.IsNotExist(err) && err != nil {
		return err
	} else if len(xs) != 0 {
		log.Printf("Init k in %s even though it is not empty? (y/n)", dir)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "y" {
			return fmt.Errorf("aborted")
		}
	}
	_, err = util.Exec(`
      git init --quiet "$dir"
      mkdir -p $client_root
      ln -sfn "$dir" "$client_root/$config_dir"
      echo "Inited k in $dir"
    `, map[string]string{"dir": dir, "client_root": clientRoot, "config_dir": configDir}, false)
	if err != nil {
		return err
	}
	_, err = util.OpenVault(filepath.Join(root, keyFile), true)
	return err
}

func version(cmd string) error {
	log.Println(getBuildVersion())
	return nil
}

func ls(cmd string) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	as := []string{}
	for name := range c.Apps {
		as = append(as, name)
	}
	sort.Strings(as)
	log.Println("Apps:")
	for _, a := range as {
		log.Println("  - " + a)
	}
	return nil
}

func update(cmd string, x struct{ Ref, OldSHA, NewSHA string }) error {
	script := ""
	if dir, err := os.Getwd(); err != nil {
		return err
	} else if exe, err := os.Executable(); err != nil {
		return err
	} else if name := filepath.Base(filepath.Dir(dir)); name == configDir {
		script = fmt.Sprintf(`
          %s generate /run/k
          systemctl daemon-reload
          systemctl restart k-http.target`, exe)
	} else {
		c, _, err := loadConfig()
		if err != nil {
			return err
		}
		a, ok := c.Apps[name]
		if !ok {
			return fmt.Errorf("app '%s' does not exist", name)
		}
		if a.Build != nil {
			script += *a.Build + "\n"
		}
		script += fmt.Sprintf(`systemctl restart %s.target`, name)
	}
	// hook executes inside .git/ and hardcodes 'GIT_DIR=.'; we have to unset it when cd'ing
	// git hooks don't source /etc/environment by themselves
	script = fmt.Sprintf(`
      unset GIT_DIR
      . /etc/environment
      cd ..
      set -x
      git switch --detach $id
      %s`, script)
	if _, err := util.Exec(script, map[string]string{"id": x.NewSHA}, false); err == nil {
		return nil
	} else if err != nil && x.OldSHA == "0000000000000000000000000000000000000000" {
		return err
	}
	_, err := util.Exec(script, map[string]string{"id": x.OldSHA}, false)
	return err
}

func receive(cmd string, x struct{ Dir string }) error {
	if _, err := util.Exec(`git init --quiet "$dir"
                            cd "$dir"
                            git config receive.denyCurrentBranch updateInstead
	                        ln --symbolic --force "$k" "$dir/.git/hooks/update"`,
		map[string]string{"dir": x.Dir, "k": os.Args[0]}, false); err != nil {
		return err
	}
	exe, err := ex.LookPath("git-receive-pack")
	if err != nil {
		return err
	}
	return syscall.Exec(exe, []string{exe, x.Dir}, os.Environ())
}

func generate(cmd string, x struct {
	Dir               string
	EarlyDir, LateDir string `cli:"::"`
}) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	return c.Render(x.Dir)
}

func notify(cmd string, a struct {
	Message string `cli:"::"`
}, f struct {
	App string
}) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	s, _ := c.Vars["telegram"].(string)
	xs := strings.Split(s, ":")
	if len(xs) != 3 {
		return fmt.Errorf(".Vars.telegram must be in the format <bot_id>:<token>:<chat_id>")
	}
	msg := a.Message
	if f.App != "" {
		msg = strings.TrimSpace(msg + "\n" + f.App)
	}
	return sendTelegramMessage(xs[0], xs[1], xs[2], msg)
}

// reset failed not working with target?
func serve(cmd string, x struct{ ConfigPath string }) error {
	return server.Start(x.ConfigPath)
}

func tunnel(cmd string, x struct{ LocalAddress string }) error {
	c, cDir, err := loadConfig()
	if err != nil {
		return err
	}
	if c.Tunnel.Pattern == "" {
		return fmt.Errorf("Tunnel.Pattern not configured")
	}
	r, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := remoteInstallBinary(r, serverBin); err != nil {
		return err
	} else if _, err := util.SSHExec(r, "systemctl restart k-http", nil, false); err != nil {
		return err
	}
	if err := gitPush(c, cDir, configDir); err != nil {
		return err
	}
	for {
		log.Printf("opening tunnel: 'http://%s' -> %s", c.Tunnel.Pattern, x.LocalAddress)
		log.Println("tunnel exited with: ", util.ReverseTunnel(r, x.LocalAddress, c.Tunnel.Address))
		r.Close()
		r, err = util.SSH(c.User, c.Host)
		if err != nil {
			return err
		}
	}
}

func completeApps(args []string) []string {
	completions := []string{}
	c, _, err := loadConfig()
	if err != nil {
		return nil
	}
	for name := range c.Apps {
		completions = append(completions, name)
	}
	return completions
}
