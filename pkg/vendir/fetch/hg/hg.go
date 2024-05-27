// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

package hg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	ctlconf "carvel.dev/vendir/pkg/vendir/config"
	ctlfetch "carvel.dev/vendir/pkg/vendir/fetch"
)

type hg struct {
	opts       ctlconf.DirectoryContentsHg
	infoLog    io.Writer
	refFetcher ctlfetch.RefFetcher
	authDir    string
	env        []string
	cacheID    string
}

func newHg(opts ctlconf.DirectoryContentsHg,
	infoLog io.Writer, refFetcher ctlfetch.RefFetcher,
	tempArea ctlfetch.TempArea,
) (*hg, error) {
	t := hg{opts, infoLog, refFetcher, "", nil, ""}
	if err := t.setup(tempArea); err != nil {
		return nil, err
	}
	return &t, nil
}

// getCacheID returns a cache id for the repository
// It doesn't include the ref because we want to reuse a cache when only the ref
// is changed
// Basically we combine all data used to write the hgrc file
func (t *hg) getCacheID() string {
	return t.cacheID
}

//nolint:revive
type hgInfo struct {
	SHA            string
	ChangeSetTitle string
}

// cloneHasTargetRef returns true if the given clone contains the target
// ref, and this ref is a revision id (not a tag or a branch)
func (t *hg) cloneHasTargetRef(dstPath string) bool {
	out, _, err := t.run([]string{"id", "--id", "-r", t.opts.Ref}, dstPath)
	if err != nil {
		return false
	}
	out = strings.TrimSpace(out)
	if strings.HasPrefix(t.opts.Ref, out) {
		return true
	}
	return false
}

func (t *hg) clone(dstPath string) error {
	if err := t.initClone(dstPath); err != nil {
		return err
	}
	return t.syncClone(dstPath)
}

func (t *hg) syncClone(dstPath string) error {
	if _, _, err := t.run([]string{"pull"}, dstPath); err != nil {
		return err
	}
	return nil
}

func (t *hg) checkout(dstPath string) (hgInfo, error) {
	if _, _, err := t.run([]string{"checkout", t.opts.Ref}, dstPath); err != nil {
		return hgInfo{}, err
	}

	info := hgInfo{}

	// use hg log to retrieve full cset sha
	out, _, err := t.run([]string{"log", "-r", ".", "-T", "{node}"}, dstPath)
	if err != nil {
		return hgInfo{}, err
	}

	info.SHA = strings.TrimSpace(out)

	out, _, err = t.run([]string{"log", "-l", "1", "-T", "{desc|firstline|strip}", "-r", info.SHA}, dstPath)
	if err != nil {
		return hgInfo{}, err
	}

	info.ChangeSetTitle = strings.TrimSpace(out)

	return info, nil
}

func (t *hg) Close() {
	if t.authDir != "" {
		os.RemoveAll(t.authDir)
		t.authDir = ""
	}
}

func (t *hg) setup(tempArea ctlfetch.TempArea) error {
	if len(t.opts.URL) == 0 {
		return fmt.Errorf("Expected non-empty URL")
	}

	cacheID := t.opts.URL

	authOpts, err := t.getAuthOpts()
	if err != nil {
		return err
	}

	authDir, err := tempArea.NewTempDir("hg-auth")
	if err != nil {
		return err
	}

	t.authDir = authDir

	t.env = os.Environ()

	hgURL := t.opts.URL

	var hgRc string

	if t.opts.Evolve {
		hgRc = fmt.Sprintf("%s", "[extensions]\nevolve =\ntopic =")
	}

	if authOpts.Username != nil && authOpts.Password != nil {
		if !strings.HasPrefix(hgURL, "https://") {
			return fmt.Errorf("Username/password authentication is only supported for https remotes")
		}
		hgCredsURL, err := url.Parse(hgURL)
		if err != nil {
			return fmt.Errorf("Parsing hg remote url: %s", err)
		}

		hgRc = fmt.Sprintf(`%s
[auth]
hgauth.prefix = https://%s
hgauth.username = %s
hgauth.password = %s
`, hgRc, hgCredsURL.Host, *authOpts.Username, *authOpts.Password)

	}

	if authOpts.IsPresent() {
		sshCmd := []string{"ssh", "-o", "ServerAliveInterval=30", "-o", "ForwardAgent=no", "-F", "/dev/null"}

		if authOpts.PrivateKey != nil {
			path := filepath.Join(authDir, "private-key")

			err = os.WriteFile(path, []byte(*authOpts.PrivateKey), 0600)
			if err != nil {
				return fmt.Errorf("Writing private key: %s", err)
			}

			sshCmd = append(sshCmd, "-i", path, "-o", "IdentitiesOnly=yes")
		}

		if authOpts.KnownHosts != nil {
			path := filepath.Join(authDir, "known-hosts")

			err = os.WriteFile(path, []byte(*authOpts.KnownHosts), 0600)
			if err != nil {
				return fmt.Errorf("Writing known hosts: %s", err)
			}

			sshCmd = append(sshCmd, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+path)
		} else {
			sshCmd = append(sshCmd, "-o", "StrictHostKeyChecking=no")
		}

		hgRc = fmt.Sprintf("%s\n[ui]\nssh = %s\n", hgRc, strings.Join(sshCmd, " "))
	}

	if len(hgRc) > 0 {
		hgRcPath := filepath.Join(authDir, "hgrc")
		err = os.WriteFile(hgRcPath, []byte(hgRc), 0600)
		if err != nil {
			return fmt.Errorf("Writing %s: %s", hgRcPath, err)
		}
		t.env = append(t.env, "HGRCPATH="+hgRcPath)
	}

	sha := sha256.Sum256([]byte(cacheID))
	t.cacheID = hex.EncodeToString(sha[:])

	return nil
}

func (t *hg) initClone(dstPath string) error {
	hgURL := t.opts.URL

	if _, _, err := t.run([]string{"init"}, dstPath); err != nil {
		return err
	}

	repoHgRcPath := filepath.Join(dstPath, ".hg", "hgrc")

	repoHgRc := fmt.Sprintf("[paths]\ndefault = %s\n", hgURL)

	if err := os.WriteFile(repoHgRcPath, []byte(repoHgRc), 0600); err != nil {
		return fmt.Errorf("Writing %s: %s", repoHgRcPath, err)
	}

	return nil
}

func (t *hg) run(args []string, dstPath string) (string, string, error) {
	var stdoutBs, stderrBs bytes.Buffer

	cmd := exec.Command("hg", args...)
	cmd.Env = t.env
	cmd.Dir = dstPath
	cmd.Stdout = io.MultiWriter(t.infoLog, &stdoutBs)
	cmd.Stderr = io.MultiWriter(t.infoLog, &stderrBs)

	t.infoLog.Write([]byte(fmt.Sprintf("--> hg %s\n", strings.Join(args, " "))))

	err := cmd.Run()
	if err != nil {
		return "", "", fmt.Errorf("Hg %s: %s (stderr: %s)", args, err, stderrBs.String())
	}

	return stdoutBs.String(), stderrBs.String(), nil
}

type hgAuthOpts struct {
	PrivateKey *string
	KnownHosts *string
	Username   *string
	Password   *string
}

func (o hgAuthOpts) IsPresent() bool {
	return o.PrivateKey != nil || o.KnownHosts != nil || o.Username != nil || o.Password != nil
}

func (t *hg) getAuthOpts() (hgAuthOpts, error) {
	var opts hgAuthOpts

	if t.opts.SecretRef != nil {
		secret, err := t.refFetcher.GetSecret(t.opts.SecretRef.Name)
		if err != nil {
			return opts, err
		}

		for name, val := range secret.Data {
			switch name {
			case ctlconf.SecretK8sCoreV1SSHAuthPrivateKey:
				key := string(val)
				opts.PrivateKey = &key
			case ctlconf.SecretSSHAuthKnownHosts:
				hosts := string(val)
				opts.KnownHosts = &hosts
			case ctlconf.SecretK8sCorev1BasicAuthUsernameKey:
				username := string(val)
				opts.Username = &username
			case ctlconf.SecretK8sCorev1BasicAuthPasswordKey:
				password := string(val)
				opts.Password = &password
			default:
				return opts, fmt.Errorf("Unknown secret field '%s' in secret '%s'", name, t.opts.SecretRef.Name)
			}
		}
	}

	return opts, nil
}
