package tool

import (
	"context"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// NewControl declares a host-defined control tool whose call the strategy must
// intercept. Calling it through the ordinary business-tool dispatcher fails
// without producing an effect.
func NewControl(spec gollem.ToolSpec) gollem.Tool {
	return controlTool{spec: spec}
}

type controlTool struct{ spec gollem.ToolSpec }

func (t controlTool) Spec() gollem.ToolSpec { return t.spec }

func (controlTool) Run(context.Context, map[string]any) (map[string]any, error) {
	return nil, goerr.New("control tool requests must be handled by the strategy")
}
