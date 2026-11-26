package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/safe"
	"github.com/urfave/cli/v3"
)

const harnessCommandName = "harness"

func cmdHarness() *cli.Command {
	return &cli.Command{
		Name:  harnessCommandName,
		Usage: "Answer external evaluation requests from the shell",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "dir",
				Usage:   "Evaluation's session directory",
				Sources: cli.EnvVars("HECATONCHEIRES_EVAL_HARNESS_DIR"),
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "next",
				Usage: "Take a request or report that the session is idle or done",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "wait", Usage: "Seconds to wait for a request (0-55)", Value: 20},
					&cli.StringFlag{Name: "role", Usage: "Only take requests with this role"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					return runHarnessTool(ctx, c, harness.SessionToolNext, map[string]any{
						"wait_seconds": c.Int("wait"), "role": c.String("role"),
					})
				},
			},
			{
				Name:      "show",
				Usage:     "Show a request's instructions, task, tools, and output schema",
				ArgsUsage: "<request-id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "Print a JSON briefing"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() != 1 {
						return cli.Exit("usage: harness show [--json] <request-id>", 2)
					}
					return runHarnessTool(ctx, c, harness.SessionToolGet, map[string]any{
						"id": c.Args().First(), "json": c.Bool("json"), "cli": harnessCommandPrefix(c.String("dir")),
					})
				},
			},
			{
				Name:      "call",
				Usage:     "Execute a tool for a request",
				ArgsUsage: "<request-id> <tool>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "args", Usage: "Tool arguments as a JSON object"},
					&cli.StringFlag{Name: "args-file", Usage: "File with JSON arguments (- for stdin)"},
					&cli.BoolFlag{Name: "async", Usage: "Print the call ID and return before the call completes"},
				},
				Action: callHarnessTool,
			},
			{
				Name:      "poll",
				Usage:     "Read a pending tool call's status or result",
				ArgsUsage: "<request-id> <call-id>",
				Flags:     []cli.Flag{&cli.IntFlag{Name: "wait", Usage: "Seconds to wait for a result (0-55)"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() != 2 {
						return cli.Exit("usage: harness poll [--wait seconds] <request-id> <call-id>", 2)
					}
					return runHarnessTool(ctx, c, harness.SessionToolPoll, map[string]any{
						"id": c.Args().Get(0), "call_id": c.Args().Get(1), "wait_seconds": c.Int("wait"),
					})
				},
			},
			{
				Name:      "submit",
				Usage:     "Submit the final answer for a request",
				ArgsUsage: "<request-id>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "file", Usage: "File with the answer (- for stdin)", Value: "-"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() != 1 {
						return cli.Exit("usage: harness submit [--file <path>] <request-id>", 2)
					}
					answer, err := readHarnessInput(c, c.String("file"))
					if err != nil {
						return cli.Exit(err.Error(), 2)
					}
					return runHarnessTool(ctx, c, harness.SessionToolSubmit, map[string]any{"id": c.Args().First(), "answer": answer})
				},
			},
			{
				Name:  "status",
				Usage: "Show waiting, in-progress, and finished request counts",
				Action: func(ctx context.Context, c *cli.Command) error {
					return runHarnessTool(ctx, c, harness.SessionToolStatus, nil)
				},
			},
		},
	}
}

func callHarnessTool(ctx context.Context, c *cli.Command) error {
	if c.NArg() != 2 || (c.IsSet("args") && c.IsSet("args-file")) {
		return cli.Exit("usage: harness call [--async] [--args '<json>' | --args-file <path>] <request-id> <tool>", 2)
	}
	raw := c.String("args")
	if c.IsSet("args-file") {
		var err error
		raw, err = readHarnessInput(c, c.String("args-file"))
		if err != nil {
			return cli.Exit(err.Error(), 2)
		}
	}
	args := make(map[string]any)
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
			return cli.Exit("the arguments must be a JSON object", 2)
		}
	}

	id := c.Args().Get(0)
	client, err := connectHarness(ctx, c)
	if err != nil {
		return err
	}
	defer safe.Close(ctx, client)

	text, failed, err := client.Call(ctx, harness.SessionToolCall, map[string]any{"id": id, "tool": c.Args().Get(1), "arguments": args})
	if err != nil || failed || c.Bool("async") {
		return printHarnessResult(c, text, failed, err)
	}
	var operation struct {
		CallID string `json:"call_id"`
		Done   bool   `json:"done"`
		Failed bool   `json:"failed"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(text), &operation); err != nil || operation.CallID == "" {
		return cli.Exit("invalid response from the session after starting a tool call", 1)
	}
	pollArgs := map[string]any{"id": id, "call_id": operation.CallID, "wait_seconds": 20}
	for {
		text, failed, err := client.Call(ctx, harness.SessionToolPoll, pollArgs)
		if err != nil || failed {
			return printHarnessResult(c, text, failed, err)
		}
		if err := json.Unmarshal([]byte(text), &operation); err != nil {
			return cli.Exit("invalid response from the session after polling a tool call", 1)
		}
		if operation.Done {
			return printHarnessResult(c, operation.Text, operation.Failed, nil)
		}
	}
}

func connectHarness(ctx context.Context, c *cli.Command) (*harness.SessionClient, error) {
	dir := c.String("dir")
	if dir == "" {
		return nil, cli.Exit("--dir or HECATONCHEIRES_EVAL_HARNESS_DIR is required", 2)
	}
	client, err := harness.DialSession(ctx, dir)
	if err != nil && !errors.Is(err, harness.ErrSessionDone) {
		return nil, cli.Exit(err.Error(), 1)
	}
	return client, err
}

func runHarnessTool(ctx context.Context, c *cli.Command, tool string, args map[string]any) error {
	client, err := connectHarness(ctx, c)
	if err == nil {
		defer safe.Close(ctx, client)
		var text string
		var failed bool
		text, failed, err = client.Call(ctx, tool, args)
		if err == nil {
			return printHarnessResult(c, text, failed, nil)
		}
		err = cli.Exit(err.Error(), 1)
	}
	// Shutdown can interrupt an HTTP exchange that started before the
	// completion record was published. Only next treats that record as done.
	if tool == harness.SessionToolNext && (errors.Is(err, harness.ErrSessionDone) || harness.SessionFinished(ctx, c.String("dir"))) {
		return printHarnessResult(c, `{"done":true}`, false, nil)
	}
	return err
}

func printHarnessResult(c *cli.Command, text string, failed bool, err error) error {
	if err != nil {
		return cli.Exit(err.Error(), 1)
	}
	if _, err := fmt.Fprintln(c.Writer, text); err != nil {
		return goerr.Wrap(err, "write harness result")
	}
	if failed {
		return cli.Exit("", 1)
	}
	return nil
}

func readHarnessInput(c *cli.Command, path string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(c.Reader)
	} else {
		raw, err = os.ReadFile(path) // #nosec G304 -- operator-selected tool argument or answer file
	}
	if err != nil {
		return "", goerr.Wrap(err, "read harness input")
	}
	return string(raw), nil
}

func harnessCommandPrefix(dir string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return shellQuote(exe) + " " + harnessCommandName + " --dir " + shellQuote(abs)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
