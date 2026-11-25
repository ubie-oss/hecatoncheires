package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/adapter/policy"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/urfave/cli/v3"
	"golang.org/x/net/http/httpguts"
)

// MCP holds the configuration for the MCP (Model Context Protocol) server
// endpoint and its Rego-based authorization. The MCP endpoint is only exposed
// when enabled with either verified user identity or a Rego policy.
type MCP struct {
	enabled bool
	// policyPaths and envPassthrough are read from the cli.Command in
	// Configure, since urfave/cli/v3 StringSliceFlag does not support
	// Destination.
	policyPaths    []string
	envPassthrough []string
	identity       *MCPIdentitySection
}

// MCPIdentitySection configures Google user authentication in global TOML.
type MCPIdentitySection struct {
	IdentityHeader string `toml:"identity_header"`
	GoogleAudience string `toml:"google_audience"`
}

// Flags returns CLI flags for the MCP server and its policy source.
func (m *MCP) Flags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:        "mcp",
			Usage:       "Enable the MCP endpoint at /mcp (requires [mcp] identity settings or --policy)",
			Value:       false,
			Sources:     cli.EnvVars("HECATONCHEIRES_MCP"),
			Destination: &m.enabled,
		},
		&cli.StringSliceFlag{
			Name:     "policy",
			Usage:    "Paths to Rego policy files or directories used to authorize MCP requests (data.auth.mcp). Can be specified multiple times.",
			Sources:  cli.EnvVars("HECATONCHEIRES_POLICY"),
			Category: "MCP",
		},
		&cli.StringSliceFlag{
			Name:     "mcp-env",
			Usage:    "Names of environment variables to expose to the Rego policy as input.env (allow-list). Can be specified multiple times.",
			Sources:  cli.EnvVars("HECATONCHEIRES_MCP_ENV"),
			Category: "MCP",
		},
	}
}

// IsEnabled reports whether the MCP endpoint should be wired.
func (m *MCP) IsEnabled() bool {
	return m.enabled
}

// Identity returns the configured identity mode, or nil for policy-only MCP.
func (m *MCP) Identity() *MCPIdentitySection {
	return m.identity
}

// Configure loads the identity settings and builds the optional policy client.
func (m *MCP) Configure(c *cli.Command) (interfaces.PolicyClient, map[string]string, error) {
	if !m.enabled {
		return nil, nil, nil
	}

	m.policyPaths = c.StringSlice("policy")
	m.envPassthrough = c.StringSlice("mcp-env")

	identity, err := loadMCPIdentity(c.StringSlice("global-config"))
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to configure MCP identity")
	}
	m.identity = identity
	if len(m.policyPaths) == 0 {
		if identity == nil {
			return nil, nil, goerr.New("--mcp requires [mcp] identity settings or at least one --policy path")
		}
		return nil, nil, nil
	}

	pc, err := policy.New(m.policyPaths)
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to build policy client for MCP")
	}

	return pc, m.envSnapshot(), nil
}

func loadMCPIdentity(paths []string) (*MCPIdentitySection, error) {
	files, err := collectTOMLFiles(paths)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to collect global config files")
	}
	var identity *MCPIdentitySection
	for _, path := range files {
		cfg, err := loadGlobalConfigFile(path)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to load global config")
		}
		if cfg.MCP == nil {
			continue
		}
		if identity != nil {
			return nil, goerr.New("more than one global config file defines [mcp]", goerr.V(ConfigPathKey, path))
		}
		if !httpguts.ValidHeaderFieldName(cfg.MCP.IdentityHeader) {
			return nil, goerr.New("[mcp] identity_header must be a valid HTTP header name", goerr.V(ConfigPathKey, path))
		}
		if cfg.MCP.GoogleAudience == "" || strings.TrimSpace(cfg.MCP.GoogleAudience) != cfg.MCP.GoogleAudience {
			return nil, goerr.New("[mcp] google_audience is required and must have no surrounding whitespace", goerr.V(ConfigPathKey, path))
		}
		identity = cfg.MCP
	}
	return identity, nil
}

// envSnapshot reads the current values of the allow-listed environment
// variables. Names absent from the environment are omitted so the policy sees
// only the variables that are actually set.
func (m *MCP) envSnapshot() map[string]string {
	out := make(map[string]string, len(m.envPassthrough))
	for _, name := range m.envPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			out[name] = v
		}
	}
	return out
}

// LogAttrs returns log attributes describing the MCP configuration. Only the
// env variable names are logged, never their values.
func (m *MCP) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Bool("enabled", m.enabled),
		slog.Bool("verified_identity", m.identity != nil),
		slog.Int("policy_paths", len(m.policyPaths)),
		slog.Any("env_passthrough", m.envPassthrough),
	}
}
