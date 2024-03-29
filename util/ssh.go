package util

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sync/errgroup"
)

func SSH(user, host string) (*ssh.Client, error) {
	socket, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, err
	}
	agent := agent.NewClient(socket)
	signers, err := agent.Signers()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	cfg.SetDefaults()
	return ssh.Dial("tcp", host+":22", cfg)
}

func SCP(c *ssh.Client, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	g := errgroup.Group{}
	g.Go(func() error {
		w, err := s.StdinPipe()
		if err != nil {
			return err
		}
		defer w.Close()
		// since scp does not allow writing to busy files we'll scp + mv
		fmt.Fprintf(w, "C%#o %d %s\n", fi.Mode(), fi.Size(), filepath.Base(remotePath)+".tmp")
		_, err = io.Copy(w, f)
		fmt.Fprint(w, "\x00")
		return err
	})
	g.Go(func() error {
		return s.Run(fmt.Sprintf(`mkdir -p '%[1]s' && scp -t '%[1]s' && mv '%[2]s.tmp' '%[2]s'`,
			filepath.Dir(remotePath), remotePath))
	})
	return g.Wait()
}

func SSHExec(c *ssh.Client, script string, capture bool, env ...string) (string, error) {
	script = shellPreamble(env) + script
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	if capture {
		bs, err := s.CombinedOutput(script)
		return strings.TrimSpace(string(bs)), err
	}
	s.Stdout, s.Stderr = os.Stdout, os.Stderr
	return "", s.Run(script)
}

func ReverseTunnel(sc *ssh.Client, localAddr, remoteAddr string) error {
	rl, err := sc.Listen("tcp", remoteAddr)
	if err != nil {
		return err
	}
	defer rl.Close()
	for {
		rc, err := rl.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer rc.Close()
			lc, err := net.Dial("tcp", localAddr)
			if err != nil {
				log.Println(err)
				return
			}
			defer lc.Close()
			go io.Copy(rc, lc)
			io.Copy(lc, rc)
		}()
	}
}

func shellPreamble(kvs []string) string {
	preamble := "set -euo pipefail;\n"
	for i := 0; i < len(kvs); i += 2 {
		preamble += fmt.Sprintf("%s=\"%s\"\n", kvs[i], kvs[i+1])
	}
	return preamble
}
