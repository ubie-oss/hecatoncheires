package config_test

import (
	"context"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/urfave/cli/v3"
)

// runMCPConfigure parses args into an MCP config, invokes Configure inside the
// command Action (where the parsed cli.Command is available), and returns the
// results.
func runMCPConfigure(t *testing.T, args []string) (*config.MCP, interfaces.PolicyClient, map[string]string, error) {
	t.Helper()
	var m config.MCP
	var pc interfaces.PolicyClient
	var env map[string]string
	var cfgErr error
	cmd := &cli.Command{
		Name:  "test",
		Flags: append(m.Flags(), (&config.AppConfig{}).GlobalConfigFlags()...),
		Action: func(_ context.Context, c *cli.Command) error {
			pc, env, cfgErr = m.Configure(c)
			return nil
		},
	}
	gt.NoError(t, cmd.Run(context.Background(), append([]string{"test"}, args...))).Required()
	return &m, pc, env, cfgErr
}

func TestMCP_DisabledByDefault(t *testing.T) {
	m, pc, env, err := runMCPConfigure(t, nil)
	gt.NoError(t, err)
	gt.Bool(t, m.IsEnabled()).False()
	gt.Value(t, pc).Nil()
	gt.Value(t, env).Nil()
}

func TestMCP_EnabledWithoutPolicyIsError(t *testing.T) {
	_, _, _, err := runMCPConfigure(t, []string{"--mcp"})
	gt.Error(t, err)
}

func TestMCP_EnabledWithPolicyBuildsClient(t *testing.T) {
	m, pc, env, err := runMCPConfigure(t, []string{"--mcp", "--policy", "testdata/policy"})
	gt.NoError(t, err).Required()
	gt.Bool(t, m.IsEnabled()).True()
	gt.Value(t, pc).NotNil()
	gt.Value(t, env).NotNil()
}

func TestMCP_VerifiedIdentityConfig(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `[mcp]
identity_header = "x-user-assertion"
google_audience = "example.apps.googleusercontent.com"
`)
	m, pc, _, err := runMCPConfigure(t, []string{"--mcp", "--global-config", path})
	gt.NoError(t, err).Required()
	gt.Value(t, pc).Nil()
	gt.Value(t, m.Identity()).NotNil().Required()
	gt.String(t, m.Identity().IdentityHeader).Equal("x-user-assertion")
	gt.String(t, m.Identity().GoogleAudience).Equal("example.apps.googleusercontent.com")

	_, pc, _, err = runMCPConfigure(t, []string{"--mcp", "--global-config", path, "--policy", "testdata/policy"})
	gt.NoError(t, err).Required()
	gt.Value(t, pc).NotNil()

	other := writeGlobalConfig(t, "other.toml", `[mcp]
identity_header = "Authorization"
google_audience = "other.apps.googleusercontent.com"
`)
	_, _, _, err = runMCPConfigure(t, []string{"--mcp", "--global-config", path, "--global-config", other})
	gt.Error(t, err).Is(config.ErrDuplicateMCPConfig)
}

func TestMCP_InvalidIdentityDoesNotFallBackToPolicy(t *testing.T) {
	for name, contents := range map[string]string{
		"empty section":    "[mcp]\n",
		"missing audience": "[mcp]\nidentity_header = 'X-Identity'\n",
		"invalid header":   "[mcp]\nidentity_header = 'Invalid Header'\ngoogle_audience = 'client-id'\n",
		"padded audience":  "[mcp]\nidentity_header = 'X-Identity'\ngoogle_audience = ' client-id '\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeGlobalConfig(t, "global.toml", contents)
			_, _, _, err := runMCPConfigure(t, []string{"--mcp", "--global-config", path, "--policy", "testdata/policy"})
			gt.Error(t, err)
		})
	}
}

func TestMCP_EnvPassthroughSnapshot(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "abc123")
	_, _, env, err := runMCPConfigure(t, []string{
		"--mcp",
		"--policy", "testdata/policy",
		"--mcp-env", "MCP_TEST_TOKEN",
		"--mcp-env", "MCP_TEST_ABSENT",
	})
	gt.NoError(t, err).Required()
	gt.Value(t, env["MCP_TEST_TOKEN"]).Equal("abc123")
	// Absent env vars are omitted, not stored as empty strings.
	_, present := env["MCP_TEST_ABSENT"]
	gt.Bool(t, present).False()
}
