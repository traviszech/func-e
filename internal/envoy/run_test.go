// Copyright 2019 Tetrate
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

package envoy

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
	"github.com/stretchr/testify/require"

	"github.com/tetratelabs/func-e/internal/globals"
	"github.com/tetratelabs/func-e/internal/moreos"
	"github.com/tetratelabs/func-e/internal/test"
	"github.com/tetratelabs/func-e/internal/test/fakebinary"
)

func TestRuntime_Run(t *testing.T) {
	tempDir := t.TempDir()

	runsDir := filepath.Join(tempDir, "runs")
	runDir := filepath.Join(runsDir, "1619574747231823000") // fake a realistic value

	fakeEnvoy := filepath.Join(tempDir, "envoy"+moreos.Exe)
	fakebinary.RequireFakeEnvoy(t, fakeEnvoy)

	tests := []struct {
		name             string
		args             []string
		shutdown         bool
		timeout          time.Duration
		expectedStderr   string
		expectedErr      string
		wantShutdownHook bool
	}{
		{
			name:    "func-e Ctrl+C",
			args:    []string{"-c", "envoy.yaml"},
			timeout: time.Second,
			// Don't warn the user when they exited the process
			expectedStderr:   moreos.Sprintf("initializing epoch 0\nstarting main dispatch loop\ncaught SIGINT\nexiting\n"),
			wantShutdownHook: true,
		},
		// We don't test envoy dying from an external signal as it isn't reported back to the func-e process and
		// Envoy returns exit status zero on anything except kill -9. We can't test kill -9 with a fake shell script.
		{
			name:           "Envoy exited with error",
			args:           []string{}, // no config file!
			expectedStderr: moreos.Sprintf("initializing epoch 0\nexiting\nAt least one of --config-path or --config-yaml or Options::configProto() should be non-empty\n"),
			expectedErr:    "envoy exited with status: 1",
		},
	}

	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			o := &globals.RunOpts{EnvoyPath: fakeEnvoy, RunDir: runDir}
			require.NoError(t, os.MkdirAll(runDir, 0o750))

			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)

			r := NewRuntime(o)
			r.Out = stdout
			r.Err = stderr
			var haveShutdownHook bool
			r.RegisterShutdownHook(func(_ context.Context) error {
				pid := requireEnvoyPid(t, r)

				// Validate envoy.pid was written
				pidText, err := os.ReadFile(r.pidPath)
				require.NoError(t, err)
				require.Equal(t, strconv.Itoa(pid), string(pidText))
				require.Greater(t, pid, 1)

				// Ensure the process can still be looked up (ex it didn't die from accidental signal propagation)
				_, err = process.NewProcess(int32(pid)) // because os.FindProcess is no-op in Linux!
				require.NoError(t, err, "shutdownHook called after process shutdown")

				haveShutdownHook = true
				return nil
			})

			// tee the error stream so we can look for the "starting main dispatch loop" line without consuming it.
			errCopy := new(bytes.Buffer)
			r.Err = io.MultiWriter(r.Err, errCopy)
			err := test.RequireRun(t, tc.timeout, r, errCopy, tc.args...)

			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedErr)
			}

			// Ensure envoy was run with the expected environment
			require.Empty(t, r.cmd.Dir) // envoy runs in the same directory as func-e
			expectedArgs := append([]string{fakeEnvoy}, tc.args...)
			expectedArgs = append(expectedArgs, "--admin-address-path", filepath.Join(runDir, "admin-address.txt"))
			require.Equal(t, expectedArgs, r.cmd.Args)

			// Assert appropriate hooks are called
			require.Equal(t, tc.wantShutdownHook, haveShutdownHook)

			// Validate we ran what we thought we did
			require.Contains(t, stdout.String(), moreos.Sprintf("starting: %s", fakeEnvoy))
			require.Contains(t, stderr.String(), tc.expectedStderr)

			// Ensure the working directory was deleted, and the "run" directory only contains the archive
			files, err := os.ReadDir(runsDir)
			require.NoError(t, err)
			require.Equal(t, 1, len(files))
			archive := filepath.Join(runsDir, files[0].Name())
			require.Equal(t, runDir+".tar.gz", archive)

			// Cleanup for the next run
			require.NoError(t, os.Remove(archive))
		})
	}
}

func requireEnvoyPid(t *testing.T, r *Runtime) int {
	if r.cmd == nil || r.cmd.Process == nil {
		t.Fatal("envoy process not yet started")
	}
	return r.cmd.Process.Pid
}
