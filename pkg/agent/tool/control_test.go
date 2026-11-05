package tool_test

import (
	"context"
	"testing"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
)

func TestControlPreservesItsSchemaAndRefusesOrdinaryDispatch(t *testing.T) {
	spec := gollem.ToolSpec{
		Name:        "request_transfer",
		Description: "Transfer control to the host",
		Parameters: map[string]*gollem.Parameter{
			"reason": {Type: gollem.TypeString, Required: true, Enum: []string{"conflict", "uncertain"}},
		},
	}
	control := tool.NewControl(spec)
	gt.Value(t, control.Spec()).Equal(spec)
	result, err := control.Run(context.Background(), map[string]any{"reason": "conflict"})
	gt.Value(t, err).NotNil()
	gt.Value(t, result).Nil()
}
