// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	ctlconf "carvel.dev/vendir/pkg/vendir/config"
	ctlfetch "carvel.dev/vendir/pkg/vendir/fetch"
	ctlver "carvel.dev/vendir/pkg/vendir/versions"
)

type Git struct {
	opts         ctlconf.DirectoryContentsGit
	contentPaths ctlconf.ContentPaths
	infoLog      io.Writer
	refFetcher   ctlfetch.RefFetcher
	cmdRunner    CommandRunner
}

func NewGit(opts ctlconf.DirectoryContentsGit, contentPaths ctlconf.ContentPaths,
	infoLog io.Writer, refFetcher ctlfetch.RefFetcher,
) *Git {
	return &Git{opts, contentPaths, infoLog, refFetcher, &runner{infoLog}}
}

// NewGitWithRunner creates a Git retriever with a provided runner
func NewGitWithRunner(opts ctlconf.DirectoryContentsGit, contentPaths ctlconf.ContentPaths,
	infoLog io.Writer, refFetcher ctlfetch.RefFetcher, cmdRunner CommandRunner,
) *Git {
	return &Git{opts, contentPaths, infoLog, refFetcher, cmdRunner}
}

//nolint:revive
type GitInfo struct {
	SHA         string
	Tags        []string
	CommitTitle string
}

func (t *Git) Retrieve(dstPath string, tempArea ctlfetch.TempArea, bundle string) (GitInfo, error) {
	if len(t.opts.URL) == 0 {
		return GitInfo{}, fmt.Errorf("Expected non-empty URL")
	}

	err := t.fetch(dstPath, tempArea, bundle)
	if err != nil {
		return GitInfo{}, err
	}

	info := GitInfo{}

	out, _, err := t.cmdRunner.Run([]string{"rev-parse", "HEAD"}, nil, dstPath)
	if err != nil {
		return GitInfo{}, err
	}

	info.SHA = strings.TrimSpace(out)

	out, _, err = t.cmdRunner.Run([]string{"describe", "--tags", info.SHA}, nil, dstPath)
	if err == nil {
		info.Tags = strings.Split(strings.TrimSpace(out), "\n")
	}

	out, _, err = t.cmdRunner.Run([]string{"log", "-n", "1", "--pretty=%B", info.SHA}, nil, dstPath)
	if err != nil {
		return GitInfo{}, err
	}

	info.CommitTitle = strings.TrimSpace(out)

	return info, nil
}

func (t *Git) fetch(dstPath string, tempArea ctlfetch.TempArea, bundle string) error {
	authOpts, err := t.getAuthOpts()
	if err != nil {
		return err
	}

	authDir, err := tempArea.NewTempDir("git-auth")
	if err != nil {
		return err
	}

	defer os.RemoveAll(authDir)

	env := os.Environ()

	if authOpts.IsPresent() {
		sshCmd := []string{"ssh", "-o", "ServerAliveInterval=30", "-o", "ForwardAgent=no", "-F", "/dev/null"}

		if authOpts.PrivateKey != nil {
			path := filepath.Join(authDir, "private-key")

			// Ensure the private key ends with a newline character, as git requires it to work. (https://github.com/carvel-dev/vendir/issues/350)
			err = os.WriteFile(path, []byte(*authOpts.PrivateKey+"\n"), 0600)
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

		env = append(env, "GIT_SSH_COMMAND="+strings.Join(sshCmd, " "))
	}

	if t.opts.LFSSkipSmudge {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	if t.opts.DangerousSkipTLSVerify {
		env = append(env, "GIT_SSL_NO_VERIFY=true")
	}
	gitURL := t.opts.URL
	gitCredsPath := filepath.Join(authDir, ".git-credentials")

	argss := [][]string{
		{"init"},
		{"config", "credential.helper", "store --file " + gitCredsPath},
		{"remote", "add", "origin", gitURL},
	}

	if t.opts.SparseCheckout {
		if len(t.contentPaths.IncludePaths) > 0 || len(t.contentPaths.ExcludePaths) > 0 {
			sparseCheckoutArgs := []string{"sparse-checkout", "set", "--no-cone"}
			for _, include := range t.contentPaths.IncludePaths {
				sparseCheckoutArgs = append(sparseCheckoutArgs, include)
			}
			for _, exclude := range t.contentPaths.ExcludePaths {
				sparseCheckoutArgs = append(sparseCheckoutArgs, fmt.Sprintf("!%s", exclude))
			}
			argss = append(argss, sparseCheckoutArgs)
		}
	}

	if authOpts.Username != nil && authOpts.Password != nil {
		if !strings.HasPrefix(gitURL, "https://") {
			return fmt.Errorf("Username/password authentication is only supported for https remotes")
		}

		if t.opts.ForceHTTPBasicAuth {
			encodedAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", *authOpts.Username, *authOpts.Password)))
			argss = append(argss, []string{"config", "--add", "http.extraHeader", fmt.Sprintf("Authorization: Basic %s", encodedAuth)})
		} else {
			gitCredsURL, err := url.Parse(gitURL)
			if err != nil {
				return fmt.Errorf("Parsing git remote url: %s", err)
			}

			gitCredsURL.User = url.UserPassword(*authOpts.Username, *authOpts.Password)
			gitCredsURL.Path = ""

			err = os.WriteFile(gitCredsPath, []byte(gitCredsURL.String()+"\n"), 0600)
			if err != nil {
				return fmt.Errorf("Writing %s: %s", gitCredsPath, err)
			}
		}
	}

	argss = append(argss, []string{"config", "remote.origin.tagOpt", "--tags"})

	if bundle != "" {
		argss = append(argss, []string{"bundle", "unbundle", bundle})
	}

	{
		fetchArgs := []string{"fetch", "origin"}
		if strings.HasPrefix(t.opts.Ref, "origin/") {
			// only fetch the exact ref we're seeking
			fetchArgs = append(fetchArgs, t.opts.Ref[7:])
		}
		if t.opts.Depth > 0 {
			fetchArgs = append(fetchArgs, "--depth", strconv.Itoa(t.opts.Depth))
		}
		argss = append(argss, fetchArgs)
	}

	err = t.cmdRunner.RunMultiple(argss, env, dstPath)
	if err != nil {
		return err
	}

	ref, err := t.resolveRef(dstPath)
	if err != nil {
		return err
	}

	if t.opts.Verification != nil {
		err := Verification{dstPath, *t.opts.Verification, t.refFetcher}.Verify(ref)
		if err != nil {
			return err
		}
	}

	_, _, err = t.cmdRunner.Run([]string{"-c", "advice.detachedHead=false", "checkout", ref}, env, dstPath)
	if err != nil {
		return err
	}

	if !t.opts.SkipInitSubmodules {
		_, _, err = t.cmdRunner.Run([]string{"submodule", "update", "--init", "--recursive"}, env, dstPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *Git) resolveRef(dstPath string) (string, error) {
	switch {
	case len(t.opts.Ref) > 0:
		if strings.HasPrefix(t.opts.Ref, "origin/") {
			return t.opts.Ref[7:], nil
		}
		return t.opts.Ref, nil

	case t.opts.RefSelection != nil:
		tags, err := t.tags(dstPath)
		if err != nil {
			return "", err
		}
		return ctlver.HighestConstrainedVersion(tags, *t.opts.RefSelection)

	default:
		return "", fmt.Errorf("Expected either ref or ref selection to be specified")
	}
}

func (t *Git) tags(dstPath string) ([]string, error) {
	out, _, err := t.cmdRunner.Run([]string{"tag", "-l"}, nil, dstPath)
	if err != nil {
		return nil, err
	}

	return strings.Split(out, "\n"), nil
}

type CommandRunner interface {
	RunMultiple(argss [][]string, env []string, dstPath string) error
	Run(args []string, env []string, dstPath string) (string, string, error)
}

type runner struct {
	infoLog io.Writer
}

func (r *runner) RunMultiple(argss [][]string, env []string, dstPath string) error {
	for _, args := range argss {
		_, _, err := r.Run(args, env, dstPath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) Run(args []string, env []string, dstPath string) (string, string, error) {
	var stdoutBs, stderrBs bytes.Buffer

	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Dir = dstPath
	cmd.Stdout = io.MultiWriter(r.infoLog, &stdoutBs)
	cmd.Stderr = io.MultiWriter(r.infoLog, &stderrBs)

	r.infoLog.Write([]byte(fmt.Sprintf("--> git %s\n", strings.Join(args, " "))))

	err := cmd.Run()
	if err != nil {
		return "", "", fmt.Errorf("Git %s: %s (stderr: %s)", args, err, stderrBs.String())
	}

	return stdoutBs.String(), stderrBs.String(), nil
}

type gitAuthOpts struct {
	PrivateKey *string
	KnownHosts *string
	Username   *string
	Password   *string
}

func (o gitAuthOpts) IsPresent() bool {
	return o.PrivateKey != nil || o.KnownHosts != nil || o.Username != nil || o.Password != nil
}

func (t *Git) getAuthOpts() (gitAuthOpts, error) {
	var opts gitAuthOpts

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
