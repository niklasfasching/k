package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	ex "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/niklasfasching/k/cli"
	"github.com/niklasfasching/k/server"
	"github.com/niklasfasching/k/util"
)

var api = cli.API{
	"ls":       {F: ls, Desc: "list apps"},
	"start":    {F: systemctl},
	"stop":     {F: systemctl},
	"reload":   {F: systemctl},
	"restart":  {F: systemctl},
	"status":   {F: systemctl, Desc: `show status of app - equivalent to systemctl status`},
	"logs":     {F: systemctl},
	"version":  {F: version},
	"init":     {F: initConfig},
	"deploy":   {F: deploy},
	"generate": {F: generate},
	"encrypt":  {F: encrypt},
	"decrypt":  {F: decrypt},
	"receive":  {F: receive, Doc: "-"},
	"update":   {F: update, Doc: "-"},
	"serve":    {F: serve, Doc: "-"},
}

var serverRoot = "/opt/k/"
var clientRoot = os.ExpandEnv("$HOME/.config/k/")
var root = clientRoot
var serverBin = filepath.Join(serverRoot, ".k")
var configDir = ".config"
var keyFile = ".key"

func init() {
	if isServer := os.Getenv("DISPLAY") == ""; isServer {
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
	v, err := util.OpenVault(filepath.Join(root, keyFile), true)
	if err != nil {
		return err
	}
	s, err := v.Decrypt(args.CipherText)
	if err != nil {
		return err
	}
	log.Println(s)
	return nil
}

func update(cmd string, x struct{ Ref, OldSHA, NewSHA string }) error {
	script := ""
	if dir, err := os.Getwd(); err != nil {
		return err
	} else if exe, err := os.Executable(); err != nil {
		return err
	} else if name := filepath.Base(filepath.Dir(dir)); name == configDir {
		script = fmt.Sprintf(`%s generate /run/k && systemctl daemon-reload`, exe)
	} else {
		c, err := loadConfig()
		if err != nil {
			return err
		}
		a, ok := c.Apps[name]
		if !ok {
			return fmt.Errorf("app '%s' does not exist", name)
		}
		script = fmt.Sprintf(`
          %s
          systemctl daemon-reload
          systemctl restart k-http.target %s.target`, a.Build, name)
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
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return c.Render(x.Dir)
}

func serve(cmd string, x struct{ ConfigPath string }) error {
	return server.Start(x.ConfigPath)
}

func deploy(cmd string,
	as struct {
		App string `cli:"::"`
	},
	fs struct {
		Dev bool
	}) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	name := as.App
	if as.App == "" {
		if name, err = getAppName(); err != nil {
			return err
		}
	}
	if app := c.Apps[name]; app == nil {
		return fmt.Errorf("'%s' is not a valid app", name)

	}
	if s, err := util.SSH(c.User, c.Host); err != nil {
		return err
	} else if err := remoteInstallBinary(s, serverBin); err != nil {
		return err
	} else if err := util.SCP(s,
		filepath.Join(clientRoot, keyFile),
		filepath.Join(serverRoot, keyFile)); err != nil {
		return err
	}
	cDir, err := filepath.EvalSymlinks(filepath.Join(root, configDir))
	if err != nil {
		return err
	}
	if err := gitPush(c, cDir, configDir); err != nil {
		return err
	}
	if err := gitPush(c, filepath.Join(cDir, "..", name), name); err != nil {
		return err
	}
	return nil
}

func systemctl(cmd string, x struct {
	App string `cli:"::defaults to $cwd app"`
}) error {
	c, err := loadConfig()
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
	if filepath.Ext(unit) == "" {
		unit += ".target"
	}
	script := fmt.Sprintf("systemctl %s %s", cmd, unit)
	if cmd == "logs" {
		script = fmt.Sprintf("journalctl -u %s", unit)
	} else if cmd == "status" {
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
	c, err := loadConfig()
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
		log.Println("\t- " + a)
	}
	return nil
}
