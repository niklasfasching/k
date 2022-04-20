package util

import (
	"fmt"
	"io"
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
	fs, err := f.Stat()
	if err != nil {
		return err
	}
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	// scp does not allow writing busy files; mv to the rescue
	g := errgroup.Group{}
	g.Go(func() error {
		w, err := s.StdinPipe()
		if err != nil {
			return err
		}
		defer w.Close()
		defer w.Close()
		fmt.Fprintf(w, "C%#o %d %s\n", fs.Mode(), fs.Size(), filepath.Base(remotePath)+".tmp")
		_, err = io.Copy(w, f)
		fmt.Fprint(w, "\x00")
		return err
	})
	g.Go(func() error {
		return s.Run(fmt.Sprintf(`mkdir -p '%[1]s' && scp -t '%[1]s'`, filepath.Dir(remotePath)))
	})
	if err := g.Wait(); err != nil {
		return err
	}
	s, err = c.NewSession()
	if err != nil {
		return err
	}
	return s.Run(fmt.Sprintf(`mv '%[1]s.tmp' '%[1]s'`, remotePath))
}

func SSHExec(c *ssh.Client, script string, env map[string]string, capture bool) (string, error) {
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
