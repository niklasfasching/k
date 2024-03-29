package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/niklasfasching/k/cli"
	"github.com/niklasfasching/k/server"
	"github.com/niklasfasching/k/util"
)

var api = cli.API{
	"init":     {F: initConfig, Desc: "set up the provided config <dir>"},
	"ls":       {F: ls, Desc: "list all apps"},
	"deploy":   {F: deploy, Desc: "deploy  config & app", Complete: completeApps},
	"start":    {F: systemctl, Desc: "systemctl start", Complete: completeApps},
	"stop":     {F: systemctl, Desc: "systemctl stop", Complete: completeApps},
	"reload":   {F: systemctl, Desc: "systemctl reload", Complete: completeApps},
	"restart":  {F: systemctl, Desc: "systemctl restart", Complete: completeApps},
	"status":   {F: systemctl, Desc: `systemctl status`, Complete: completeApps},
	"logs":     {F: systemctl, Desc: "journalctl K=<app>", Complete: completeApps},
	"tunnel":   {F: tunnel, Desc: "tunnel <address>:<remote_address>"},
	"notify":   {F: notify, Desc: "send message to k.Vars.telegram $bot_id:$token:$chat_id"},
	"encrypt":  {F: encrypt, Desc: "encrypt the provided <value>"},
	"decrypt":  {F: decrypt, Desc: "decrypt the provided <value>"},
	"sign":     {F: sign, Desc: "sign the provided <file>"},
	"render":   {F: render, Desc: "render systemd config"},
	"version":  {F: version},
	"generate": {F: generate, Desc: "-"},
	"receive":  {F: receive, Desc: "-"},
	"serve":    {F: serve, Desc: "-"},
}

type Root string

var root = Root(clientRoot)
var serverRoot = Root("/opt/k/")
var clientRoot = Root(os.ExpandEnv("$HOME/.config/k/"))
var serverBin = filepath.Join(string(serverRoot), "_k_")

func (r Root) IsClient() bool       { return r != serverRoot }
func (r Root) ConfigDir() string    { return filepath.Join(string(r), "_") }
func (r Root) VaultKeyFile() string { return filepath.Join(string(r), "vault.key") }
func (r Root) SignKeyFile() string  { return filepath.Join(string(r), "sign.key") }

func init() {
	log.SetFlags(0)
	if v := os.Getenv("K_ROOT"); v != "" {
		log.SetOutput(os.Stdout)
		root = Root(v)
	} else if isServer := os.Getenv("DISPLAY") == ""; isServer {
		log.SetOutput(os.Stderr)
		root = Root(serverRoot)
	}
}

func main() {
	cmd, args := "", os.Args[1:]
	if strings.Contains(filepath.Base(os.Args[0]), "generator") {
		cmd = "generate"
	} else if len(args) >= 1 {
		cmd, args = args[0], args[1:]
	}
	if err := api.Run(cmd, args); err != nil {
		log.Fatal(err)
	}
}

func encrypt(cmd string, args struct{ PlainText string }) error {
	v, err := util.OpenVault(root.VaultKeyFile(), true)
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
	m := regexp.MustCompile(`([\w/=+]{24,})`).FindStringSubmatch(args.CipherText)
	if m == nil {
		return fmt.Errorf("arg does not contain an encrypted value")
	}
	v, err := util.OpenVault(root.VaultKeyFile(), true)
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
	kbs, err := os.ReadFile(root.SignKeyFile())
	if err != nil {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		if err := os.WriteFile(root.SignKeyFile(), k, 0600); err != nil {
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
	x struct {
		App string `cli:"::"`
	}) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	name, err := getAppName(c, x.App)
	if err != nil {
		return err
	}
	sc, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer sc.Close()
	if err := remoteInstallBinary(sc, serverBin); err != nil {
		return err
	} else if err := syncConfig(sc, c); err != nil {
		return err
	}
	if name == filepath.Base(c.Dir) {
		return nil
	}
	return deployApp(sc, c, name)
}

func systemctl(cmd string, x struct {
	App string
}) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	unit, err := getAppName(c, x.App)
	if err != nil {
		return err
	}
	s, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer s.Close()
	script := fmt.Sprintf("systemctl %s %s", cmd, unit)
	if cmd == "logs" {
		script = fmt.Sprintf("journalctl K=%s -f", unit)
	} else if cmd == "status" {
		if filepath.Ext(unit) == "" {
			script += ".target"
		}
		script += " --with-dependencies --lines 100"
	}
	_, err = util.SSHExec(s, script, false, "SYSTEMD_COLORS", "1")
	return err
}

func initConfig(cmd string, x struct{ Dir string }) error {
	if _, err := os.Stat(filepath.Join(x.Dir, "k.yaml")); err != nil {
		return fmt.Errorf("k config dir requires k.yaml: %w", err)
	} else if dir, err := filepath.Abs(x.Dir); err != nil {
		return err
	} else if err := os.MkdirAll(clientRoot.ConfigDir(), 0755); err != nil {
		return err
	} else if err := os.Symlink(dir, clientRoot.ConfigDir()); err != nil {
		return err
	}
	_, err := util.OpenVault(root.VaultKeyFile(), true)
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
		log.Println("  - " + a)
	}
	return nil
}

func receive(cmd string) error {
	return util.NewPipe(os.Stdin, os.Stdout).Receive()
}

func render(cmd string, x struct {
	Dir string
}) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return renderConfig(c, x.Dir)
}

func generate(cmd string, x struct {
	Dir, EarlyDir, LateDir string
}) error {
	if root.IsClient() {
		return fmt.Errorf("systemd internal command")
	}
	src, dst := root.ConfigDir(), x.Dir
	return filepath.Walk(src, func(path string, fi fs.FileInfo, err error) error {
		rpath, err := filepath.Rel(src, path)
		if err != nil || rpath == "" {
			return err
		}
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rpath), 0755)
		}
		bs, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return ioutil.WriteFile(filepath.Join(dst, rpath), bs, fi.Mode())
	})
}

func notify(cmd string, a struct {
	Message string `cli:"::"`
}, f struct {
	App string
}) error {
	c, err := loadConfig()
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

func serve(cmd string, x struct{ ConfigPath string }) error {
	return server.Start(x.ConfigPath)
}

func tunnel(cmd string, x struct{ LocalAddress string }) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if c.Tunnel.Pattern == "" {
		return fmt.Errorf("Tunnel.Pattern not configured")
	}
	sc, err := util.SSH(c.User, c.Host)
	if err != nil {
		return err
	}
	defer sc.Close()
	if err := remoteInstallBinary(sc, serverBin); err != nil {
		return err
	} else if err := syncConfig(sc, c); err != nil {
		return err
	}
	for {
		log.Printf("opening tunnel: 'http://%s' -> %s", c.Tunnel.Pattern, x.LocalAddress)
		log.Println("tunnel exited with: ", util.ReverseTunnel(sc, x.LocalAddress, c.Tunnel.Address))
		sc.Close()
		sc, err = util.SSH(c.User, c.Host)
		if err != nil {
			return err
		}
	}
}
