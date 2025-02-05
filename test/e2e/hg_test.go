// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type HgAssetInfo struct {
	InitialChangeset string `json:"initial-changeset"`
	ExtraBundle      string `json:"extra-bundle"`
	ExtraChangeset   string `json:"extra-changeset"`
}

func loadHgAssetInfo(t *testing.T, filename string) HgAssetInfo {
	t.Helper()
	f, err := os.Open(filename)
	require.NoError(t, err, "could not open file %s", filename)
	var info HgAssetInfo
	require.NoError(t, json.NewDecoder(f).Decode(&info), "could not parse file %s", filename)
	return info
}

func TestHgCache(t *testing.T) {
	env := BuildEnv(t)
	logger := Logger{}
	vendir := Vendir{t, env.BinaryPath, logger}

	_ = Vendir{t, env.BinaryPath, logger}

	hgSrcPath, err := os.MkdirTemp("", "vendir-e2e-hg-cache")
	require.NoError(t, err)
	defer os.RemoveAll(hgSrcPath)

	out, err := exec.Command("tar", "xzvf", "assets/hg-repos/asset.tgz", "-C", hgSrcPath).CombinedOutput()
	require.NoError(t, err, "Unpacking hg-repos asset, output: '%s'", out)

	info := loadHgAssetInfo(t, path.Join(hgSrcPath, "info.json"))

	yamlConfig := func(ref string) io.Reader {
		return strings.NewReader(fmt.Sprintf(`
apiVersion: vendir.k14s.io/v1alpha1
kind: Config
directories:
- path: vendor
  contents:
  - path: test
    hg:
      url: "%s/repo"
      ref: "%s"
`, hgSrcPath, ref))
	}

	cachePath, err := os.MkdirTemp("", "vendir-e2e-hg-cache-vendir-cache")
	require.NoError(t, err)
	defer os.RemoveAll(cachePath)

	dstPath, err := os.MkdirTemp("", "vendir-e2e-hg-cache-dst")
	require.NoError(t, err)
	defer os.RemoveAll(dstPath)

	var stdout bytes.Buffer
	stdoutDec := json.NewDecoder(&stdout)

	logger.Section("initial clone", func() {
		stdout.Truncate(0)
		vendir.RunWithOpts(
			[]string{"sync", "-f", "-", "--json"},
			RunOpts{
				Dir:          dstPath,
				StdinReader:  yamlConfig(info.InitialChangeset),
				StdoutWriter: &stdout,
				Env:          []string{"VENDIR_CACHE_DIR=" + cachePath},
			})

		var out VendirOutput
		require.NoError(t, stdoutDec.Decode(&out))

		assert.Contains(t, out.Lines[1], "init")
		assert.Contains(t, out.Lines[3], "pull")

		b, err := os.ReadFile(filepath.Join(dstPath, "vendor", "test", "file1.txt"))
		assert.NoError(t, err)
		assert.Equal(t, "content1\n", string(b))
	})

	logger.Section("sync from cache only", func() {
		stdout.Truncate(0)
		vendir.RunWithOpts(
			[]string{"sync", "-f", "-", "--json"},
			RunOpts{
				Dir:          dstPath,
				StdinReader:  yamlConfig(info.InitialChangeset),
				StdoutWriter: &stdout,
				Env:          []string{"VENDIR_CACHE_DIR=" + cachePath},
			})

		var out VendirOutput
		require.NoError(t, stdoutDec.Decode(&out))

		for _, line := range out.Lines {
			assert.NotContains(t, line, "init")
			assert.NotContains(t, line, "pull")
		}

		b, err := os.ReadFile(filepath.Join(dstPath, "vendor", "test", "file1.txt"))
		assert.NoError(t, err)
		assert.Equal(t, "content1\n", string(b))
	})

	out, err = exec.Command(
		"hg", "unbundle",
		"--repository", filepath.Join(hgSrcPath, "/repo"),
		filepath.Join(hgSrcPath, info.ExtraBundle),
	).CombinedOutput()
	require.NoError(t, err, "Unpacking hg-repos asset, output: '%s'", out)

	logger.Section("sync from cache + pull", func() {
		stdout.Truncate(0)
		vendir.RunWithOpts(
			[]string{"sync", "-f", "-", "--json"},
			RunOpts{
				Dir:          dstPath,
				StdinReader:  yamlConfig(info.ExtraChangeset),
				StdoutWriter: &stdout,
				Env:          []string{"VENDIR_CACHE_DIR=" + cachePath},
			})

		var out VendirOutput
		require.NoError(t, stdoutDec.Decode(&out))

		assert.Contains(t, out.Lines[3], "pull")
		for _, line := range out.Lines {
			assert.NotContains(t, line, "init")
		}

		b, err := os.ReadFile(filepath.Join(dstPath, "vendor", "test", "file1.txt"))
		assert.NoError(t, err)
		assert.Equal(t, "content2\n", string(b))
	})
}
