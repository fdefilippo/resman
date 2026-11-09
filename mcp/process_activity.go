package mcp

import (
	"context"
	"fmt"

	"github.com/fdefilippo/resman/internal/processactivity"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GetProcessActivityArgs selects actual process identities, not per-user aggregates.
type GetProcessActivityArgs = processactivity.Query

func (s *Server) registerProcessActivityTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{Name: "get_process_activity", Description: "Query sampled process lifetimes and covered CPU/RAM activity; does not collect or change policy"}, s.handleGetProcessActivity)
}
func (s *Server) handleGetProcessActivity(ctx context.Context, _ *mcp.CallToolRequest, args GetProcessActivityArgs) (*mcp.CallToolResult, processactivity.Result, error) {
	if s.metricsCollector == nil {
		return nil, processactivity.Result{}, fmt.Errorf("process activity collector unavailable")
	}
	result, err := s.metricsCollector.QueryProcessActivity(ctx, args)
	if err != nil {
		return nil, processactivity.Result{}, fmt.Errorf("query process activity: %w", err)
	}
	return nil, result, nil
}
