// Copyright 2020 Tetrate
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	rootcmd "github.com/tetratelabs/func-e/internal/cmd"
	"github.com/tetratelabs/func-e/internal/globals"
	"github.com/tetratelabs/func-e/internal/moreos"
	"github.com/tetratelabs/func-e/internal/test"
	"github.com/tetratelabs/func-e/internal/test/morerequire"
	"github.com/tetratelabs/func-e/internal/version"
)

// Runner allows us to not introduce dependency cycles on envoy.Runtime
type runner struct {
	c              *cli.App
	stdout, stderr *bytes.Buffer
}

func (r *runner) Run(ctx context.Context, args []string) error {
	return r.c.RunContext(ctx, args)
}

func (r *runner) String() string {
	return fmt.Sprintf("{stdout: %s, stderr: %s}", r.stdout, r.stderr)
}

// TestFuncERun executes envoy then cancels the context. This results in no stdout
func TestFuncERun(t *testing.T) {
	o := setupTest(t)

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	c := rootcmd.NewApp(o)

	args := []string{"func-e", "run", "-c", "envoy.yaml"}
	// tee the error stream so we can look for the "starting main dispatch loop" line without consuming it.
	errCopy := new(bytes.Buffer)
	c.ErrWriter = io.MultiWriter(stderr, errCopy)
	err := test.RequireRun(t, 3*time.Second, &runner{c, stdout, stderr}, errCopy, args...)

	require.NoError(t, err)
	require.Empty(t, stdout)

	// The runner is the current process, not a child (func-e run) as would be the case in reality and in e2e tests.
	// Normally, we would use Runtime.FakeInterrupt to fake a ctrl-c, but you can't get a Runtime from a cli.App.
	//
	// Passing shutdown=nil, test.RequireRun cancels the go context used by urfave. This gets the process to stop, but
	// unpredictably: For example, we will see "starting main dispatch loop", but may or may not see the line "exiting".
	//
	// To avoid test flakes around this, we only look for the line we know will be there.
	require.Contains(t, stderr.String(), "starting main dispatch loop")
}

func TestFuncERun_TeesConsoleToLogs(t *testing.T) {
	o := setupTest(t)

	c, stdout, stderr := newApp(o)
	o.Out = io.Discard         // stdout/stderr only includes what envoy writes, not our status messages
	o.DontArchiveRunDir = true // we need to read-back the log files
	runWithoutConfig(t, c)

	actual, err := os.ReadFile(filepath.Join(o.RunDir, "stdout.log"))
	require.NoError(t, err)
	require.NotEmpty(t, stdout.String())                 // sanity check
	require.Contains(t, stdout.String(), string(actual)) // stdout will be more than in the log as func-e writes to it.

	actual, err = os.ReadFile(filepath.Join(o.RunDir, "stderr.log"))
	require.NoError(t, err)
	require.NotEmpty(t, stderr.String()) // sanity check
	require.Equal(t, stderr.String(), string(actual))
}

func TestFuncERun_ReadsHomeVersionFile(t *testing.T) {
	o := setupTest(t)
	o.EnvoyVersion = "" // pretend this is an initial setup
	o.Out = new(bytes.Buffer)

	require.NoError(t, os.WriteFile(filepath.Join(o.HomeDir, "version"), []byte(version.LastKnownEnvoyMinor), 0o600))

	c, _, _ := newApp(o)
	runWithoutConfig(t, c)

	// No implicit lookup
	require.NotContains(t, o.Out.(*bytes.Buffer).String(), moreos.Sprintf("looking up latest version"))
	require.Equal(t, version.LastKnownEnvoy, o.EnvoyVersion)

	writtenVersion, err := os.ReadFile(filepath.Join(o.HomeDir, "version"))
	require.NoError(t, err)
	require.Equal(t, version.LastKnownEnvoyMinor.String(), string(writtenVersion))
}

func TestFuncERun_CreatesHomeVersionFile(t *testing.T) {
	o := setupTest(t)
	o.EnvoyVersion = "" // pretend this is an initial setup
	o.Out = new(bytes.Buffer)

	// make sure first run where the home doesn't exist yet, works!
	require.NoError(t, os.RemoveAll(o.HomeDir))

	c, _, _ := newApp(o)
	runWithoutConfig(t, c)

	// We logged the implicit lookup
	require.Contains(t, o.Out.(*bytes.Buffer).String(), moreos.Sprintf("looking up the latest Envoy version"))
	require.FileExists(t, filepath.Join(o.HomeDir, "version"))
	require.Equal(t, version.LastKnownEnvoy, o.EnvoyVersion)

	writtenVersion, err := os.ReadFile(filepath.Join(o.HomeDir, "version"))
	require.NoError(t, err)
	require.Equal(t, version.LastKnownEnvoyMinor.String(), string(writtenVersion))
}

// runWithoutConfig intentionally has envoy quit. This allows tests to not have to interrupt envoy to proceed.
func runWithoutConfig(t *testing.T, c *cli.App) {
	require.EqualError(t, c.Run([]string{"func-e", "run"}), "envoy exited with status: 1")
}

func TestFuncERun_ValidatesHomeVersion(t *testing.T) {
	o := setupTest(t)
	o.Out = new(bytes.Buffer)

	o.EnvoyVersion = ""
	require.NoError(t, os.WriteFile(filepath.Join(o.HomeDir, "version"), []byte("a.a.a"), 0o600))

	c, _, _ := newApp(o)
	err := c.Run([]string{"func-e", "run"})

	// Verify the command failed with the expected error
	expectedErr := fmt.Sprintf(`invalid version in "$FUNC_E_HOME/version": "a.a.a" should look like %q or %q`, version.LastKnownEnvoy, version.LastKnownEnvoyMinor)
	require.EqualError(t, err, moreos.ReplacePathSeparator(expectedErr))
}

// TestFuncERun_ValidatesWorkingVersion duplicates logic in version_test.go to ensure a non-home version validates.
func TestFuncERun_ValidatesWorkingVersion(t *testing.T) {
	o := setupTest(t)
	o.Out = new(bytes.Buffer)
	o.EnvoyVersion = ""

	revertWd := morerequire.RequireChdir(t, t.TempDir())
	defer revertWd()
	require.NoError(t, os.WriteFile(".envoy-version", []byte("b.b.b"), 0o600))

	c, _, _ := newApp(o)
	err := c.Run([]string{"func-e", "run"})

	// Verify the command failed with the expected error
	expectedErr := fmt.Sprintf(`invalid version in "$PWD/.envoy-version": "b.b.b" should look like %q or %q`, version.LastKnownEnvoy, version.LastKnownEnvoyMinor)
	require.EqualError(t, err, moreos.ReplacePathSeparator(expectedErr))
}

func TestFuncERun_ErrsWhenVersionsServerDown(t *testing.T) {
	tempDir := t.TempDir()

	o := &globals.GlobalOpts{
		EnvoyVersionsURL: "https://127.0.0.1:9999",
		HomeDir:          tempDir,
		Out:              new(bytes.Buffer),
	}
	c, _, _ := newApp(o)
	err := c.Run([]string{"func-e", "run"})

	require.Contains(t, o.Out.(*bytes.Buffer).String(), moreos.Sprintf("looking up the latest Envoy version"))
	require.Contains(t, err.Error(), fmt.Sprintf(`couldn't lookup the latest Envoy version from %s`, o.EnvoyVersionsURL))
}
