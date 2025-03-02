//go:build linux || darwin

package terraform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/slogtest"

	"github.com/coder/coder/provisioner/terraform"
	"github.com/coder/coder/provisionersdk"
	"github.com/coder/coder/provisionersdk/proto"
)

type provisionerServeOptions struct {
	binaryPath  string
	exitTimeout time.Duration
}

func setupProvisioner(t *testing.T, opts *provisionerServeOptions) (context.Context, proto.DRPCProvisionerClient) {
	if opts == nil {
		opts = &provisionerServeOptions{}
	}
	cachePath := t.TempDir()
	client, server := provisionersdk.TransportPipe()
	ctx, cancelFunc := context.WithCancel(context.Background())
	serverErr := make(chan error, 1)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		cancelFunc()
		err := <-serverErr
		assert.NoError(t, err)
	})
	go func() {
		serverErr <- terraform.Serve(ctx, &terraform.ServeOptions{
			ServeOptions: &provisionersdk.ServeOptions{
				Listener: server,
			},
			BinaryPath:  opts.binaryPath,
			CachePath:   cachePath,
			Logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
			ExitTimeout: opts.exitTimeout,
		})
	}()
	api := proto.NewDRPCProvisionerClient(provisionersdk.Conn(client))
	return ctx, api
}

func TestProvision_Cancel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("This test uses interrupts and is not supported on Windows")
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)
	fakeBin := filepath.Join(cwd, "testdata", "bin", "terraform_fake_cancel.sh")

	tests := []struct {
		name          string
		mode          string
		startSequence []string
		wantLog       []string
	}{
		{
			name:          "Cancel init",
			mode:          "init",
			startSequence: []string{"init_start"},
			wantLog:       []string{"interrupt", "exit"},
		},
		{
			name:          "Cancel apply",
			mode:          "apply",
			startSequence: []string{"init", "apply_start"},
			wantLog:       []string{"interrupt", "exit"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			binPath := filepath.Join(dir, "terraform")

			// Example: exec /path/to/terrafork_fake_cancel.sh 1.2.1 apply "$@"
			content := fmt.Sprintf("#!/bin/sh\nexec %q %s %s \"$@\"\n", fakeBin, terraform.TerraformVersion.String(), tt.mode)
			err := os.WriteFile(binPath, []byte(content), 0o755) //#nosec
			require.NoError(t, err)

			ctx, api := setupProvisioner(t, &provisionerServeOptions{
				binaryPath:  binPath,
				exitTimeout: time.Nanosecond,
			})

			response, err := api.Provision(ctx)
			require.NoError(t, err)
			err = response.Send(&proto.Provision_Request{
				Type: &proto.Provision_Request_Start{
					Start: &proto.Provision_Start{
						Directory: dir,
						DryRun:    false,
						ParameterValues: []*proto.ParameterValue{{
							DestinationScheme: proto.ParameterDestination_PROVISIONER_VARIABLE,
							Name:              "A",
							Value:             "example",
						}},
						Metadata: &proto.Provision_Metadata{},
					},
				},
			})
			require.NoError(t, err)

			for _, line := range tt.startSequence {
			LoopStart:
				msg, err := response.Recv()
				require.NoError(t, err)

				t.Log(msg.Type)

				log := msg.GetLog()
				if log == nil {
					goto LoopStart
				}
				require.Equal(t, line, log.Output)
			}

			err = response.Send(&proto.Provision_Request{
				Type: &proto.Provision_Request_Cancel{
					Cancel: &proto.Provision_Cancel{},
				},
			})
			require.NoError(t, err)

			var gotLog []string
			for {
				msg, err := response.Recv()
				require.NoError(t, err)

				if log := msg.GetLog(); log != nil {
					gotLog = append(gotLog, log.Output)
				}
				if c := msg.GetComplete(); c != nil {
					require.Contains(t, c.Error, "exit status 1")
					break
				}
			}
			require.Equal(t, tt.wantLog, gotLog)
		})
	}
}

func TestProvision(t *testing.T) {
	t.Parallel()

	ctx, api := setupProvisioner(t, nil)

	testCases := []struct {
		Name    string
		Files   map[string]string
		Request *proto.Provision_Request
		// Response may be nil to not check the response.
		Response *proto.Provision_Response
		// If ErrorContains is not empty, then response.Recv() should return an
		// error containing this string before a Complete response is returned.
		ErrorContains string
		// If ExpectLogContains is not empty, then the logs should contain it.
		ExpectLogContains string
		DryRun            bool
	}{
		{
			Name: "single-variable",
			Files: map[string]string{
				"main.tf": `variable "A" {
				description = "Testing!"
			}`,
			},
			Request: &proto.Provision_Request{
				Type: &proto.Provision_Request_Start{
					Start: &proto.Provision_Start{
						ParameterValues: []*proto.ParameterValue{{
							DestinationScheme: proto.ParameterDestination_PROVISIONER_VARIABLE,
							Name:              "A",
							Value:             "example",
						}},
					},
				},
			},
			Response: &proto.Provision_Response{
				Type: &proto.Provision_Response_Complete{
					Complete: &proto.Provision_Complete{},
				},
			},
		},
		{
			Name: "missing-variable",
			Files: map[string]string{
				"main.tf": `variable "A" {
			}`,
			},
			Response: &proto.Provision_Response{
				Type: &proto.Provision_Response_Complete{
					Complete: &proto.Provision_Complete{
						Error: "terraform apply: exit status 1",
					},
				},
			},
			ExpectLogContains: "No value for required variable",
		},
		{
			Name: "missing-variable-dry-run",
			Files: map[string]string{
				"main.tf": `variable "A" {
			}`,
			},
			ErrorContains:     "terraform plan:",
			ExpectLogContains: "No value for required variable",
			DryRun:            true,
		},
		{
			Name: "single-resource-dry-run",
			Files: map[string]string{
				"main.tf": `resource "null_resource" "A" {}`,
			},
			Response: &proto.Provision_Response{
				Type: &proto.Provision_Response_Complete{
					Complete: &proto.Provision_Complete{
						Resources: []*proto.Resource{{
							Name: "A",
							Type: "null_resource",
						}},
					},
				},
			},
			DryRun: true,
		},
		{
			Name: "single-resource",
			Files: map[string]string{
				"main.tf": `resource "null_resource" "A" {}`,
			},
			Response: &proto.Provision_Response{
				Type: &proto.Provision_Response_Complete{
					Complete: &proto.Provision_Complete{
						Resources: []*proto.Resource{{
							Name: "A",
							Type: "null_resource",
						}},
					},
				},
			},
		},
		{
			Name: "bad-syntax-1",
			Files: map[string]string{
				"main.tf": `a`,
			},
			ErrorContains:     "initialize terraform",
			ExpectLogContains: "Argument or block definition required",
		},
		{
			Name: "bad-syntax-2",
			Files: map[string]string{
				"main.tf": `;asdf;`,
			},
			ErrorContains:     "initialize terraform",
			ExpectLogContains: `The ";" character is not valid.`,
		},
		{
			Name: "destroy-no-state",
			Files: map[string]string{
				"main.tf": `resource "null_resource" "A" {}`,
			},
			Request: &proto.Provision_Request{
				Type: &proto.Provision_Request_Start{
					Start: &proto.Provision_Start{
						State: nil,
						Metadata: &proto.Provision_Metadata{
							WorkspaceTransition: proto.WorkspaceTransition_DESTROY,
						},
					},
				},
			},
			ExpectLogContains: "nothing to do",
		},
		{
			Name: "unsupported-parameter-scheme",
			Files: map[string]string{
				"main.tf": "",
			},
			Request: &proto.Provision_Request{
				Type: &proto.Provision_Request_Start{
					Start: &proto.Provision_Start{
						ParameterValues: []*proto.ParameterValue{
							{
								DestinationScheme: 88,
								Name:              "UNSUPPORTED",
								Value:             "sadface",
							},
						},
					},
				},
			},
			ErrorContains: "unsupported parameter type",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			for path, content := range testCase.Files {
				err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600)
				require.NoError(t, err)
			}

			request := &proto.Provision_Request{
				Type: &proto.Provision_Request_Start{
					Start: &proto.Provision_Start{
						Directory: directory,
						DryRun:    testCase.DryRun,
					},
				},
			}
			if testCase.Request != nil {
				request.GetStart().ParameterValues = testCase.Request.GetStart().ParameterValues
				request.GetStart().State = testCase.Request.GetStart().State
				request.GetStart().DryRun = testCase.Request.GetStart().DryRun
				request.GetStart().Metadata = testCase.Request.GetStart().Metadata
			}
			if request.GetStart().Metadata == nil {
				request.GetStart().Metadata = &proto.Provision_Metadata{}
			}

			response, err := api.Provision(ctx)
			require.NoError(t, err)
			err = response.Send(request)
			require.NoError(t, err)

			gotExpectedLog := testCase.ExpectLogContains == ""
			for {
				msg, err := response.Recv()
				if msg != nil && msg.GetLog() != nil {
					if testCase.ExpectLogContains != "" && strings.Contains(msg.GetLog().Output, testCase.ExpectLogContains) {
						gotExpectedLog = true
					}

					t.Logf("log: [%s] %s", msg.GetLog().Level, msg.GetLog().Output)
					continue
				}
				if testCase.ErrorContains != "" {
					require.ErrorContains(t, err, testCase.ErrorContains)
					break
				}
				require.NoError(t, err)

				if msg.GetComplete() == nil {
					continue
				}

				require.NoError(t, err)

				// Remove randomly generated data.
				for _, resource := range msg.GetComplete().Resources {
					sort.Slice(resource.Agents, func(i, j int) bool {
						return resource.Agents[i].Name < resource.Agents[j].Name
					})

					for _, agent := range resource.Agents {
						agent.Id = ""
						if agent.GetToken() == "" {
							continue
						}
						agent.Auth = &proto.Agent_Token{}
					}
				}

				if testCase.Response != nil {
					resourcesGot, err := json.Marshal(msg.GetComplete().Resources)
					require.NoError(t, err)

					resourcesWant, err := json.Marshal(testCase.Response.GetComplete().Resources)
					require.NoError(t, err)

					require.Equal(t, testCase.Response.GetComplete().Error, msg.GetComplete().Error)

					require.Equal(t, string(resourcesWant), string(resourcesGot))
				}
				break
			}

			if !gotExpectedLog {
				t.Fatalf("expected log string %q but never saw it", testCase.ExpectLogContains)
			}
		})
	}
}

// nolint:paralleltest
func TestProvision_ExtraEnv(t *testing.T) {
	// #nosec
	secretValue := "oinae3uinxase"
	t.Setenv("TF_LOG", "INFO")
	t.Setenv("TF_SUPERSECRET", secretValue)

	ctx, api := setupProvisioner(t, nil)

	directory := t.TempDir()
	path := filepath.Join(directory, "main.tf")
	err := os.WriteFile(path, []byte(`resource "null_resource" "A" {}`), 0o600)
	require.NoError(t, err)

	request := &proto.Provision_Request{
		Type: &proto.Provision_Request_Start{
			Start: &proto.Provision_Start{
				Directory: directory,
				Metadata: &proto.Provision_Metadata{
					WorkspaceTransition: proto.WorkspaceTransition_START,
				},
			},
		},
	}
	response, err := api.Provision(ctx)
	require.NoError(t, err)
	err = response.Send(request)
	require.NoError(t, err)
	found := false
	for {
		msg, err := response.Recv()
		require.NoError(t, err)

		if log := msg.GetLog(); log != nil {
			t.Log(log.Level.String(), log.Output)
			if strings.Contains(log.Output, "TF_LOG") {
				found = true
			}
			require.NotContains(t, log.Output, secretValue)
		}
		if c := msg.GetComplete(); c != nil {
			require.Empty(t, c.Error)
			break
		}
	}
	require.True(t, found)
}
