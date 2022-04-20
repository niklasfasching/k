package util

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	ex "os/exec"
	"strings"
	"syscall"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/term"
)

type Vault []byte

func Exec(script string, env map[string]string, capture bool) (string, error) {
	script = shellPreamble(env) + script
	cmd := ex.Command("bash", "-c", script)
	if capture {
		bs, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(bs)), err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return "", cmd.Run()
}

func OpenVault(path string, createIfMissing bool) (Vault, error) {
	bs, err := os.ReadFile(path)
	if err == nil {
		return bs, nil
	} else if !createIfMissing {
		return nil, err
	}
	log.Println("Please enter a password:")
	pass, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return nil, err
	} else if len(pass) == 0 {
		return nil, fmt.Errorf("password must not be empty")
	}
	log.Println("Enter password again:")
	if pass2, err := term.ReadPassword(int(syscall.Stdin)); err != nil {
		return nil, err
	} else if string(pass) != string(pass2) {
		return nil, fmt.Errorf("passwords did not match")
	}
	/* https://en.wikipedia.org/wiki/Salt_(cryptography)#Salt_re-use
	   To get a key that can be recreated with just the passphrase the salt must not
	   be dynamic. We don't care about collisions (we explicitly want them - the key should
	   be recreatable using the passphrase). To make lookup tables a little harder we salt
	   the passphrase anyways; just using a static salt. */
	salt := []byte{47, 239, 236, 171, 92, 171, 148, 211}
	k := pbkdf2.Key(pass, salt, 4096, 32, sha1.New) // from docstring
	return k, os.WriteFile(path, k, 0600)
}

func (v Vault) Encrypt(plaintext string) (string, error) {
	k, nonce := [32]byte{}, [24]byte{}
	copy(k[:], v)
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return "", err
	}
	ciphertext := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &k)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (v Vault) Decrypt(ciphertext string) (string, error) {
	bs, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	k, nonce := [32]byte{}, [24]byte{}
	copy(k[:], v)
	copy(nonce[:], bs[:24])
	plaintext, ok := secretbox.Open(nil, bs[24:], &nonce, &k)
	if !ok {
		return "", fmt.Errorf("failed to decrypt '%s'", ciphertext)
	}
	return string(plaintext), nil
}

func shellPreamble(env map[string]string) string {
	preamble := "set -euo pipefail;\n"
	for k, v := range env {
		preamble += fmt.Sprintf("%s=\"%s\"\n", k, v)
	}
	return preamble
}
