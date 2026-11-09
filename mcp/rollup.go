package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/fdefilippo/resman/internal/metricrollup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GetMetricRollupArgs selects compacted hourly buckets rather than raw samples.
type GetMetricRollupArgs struct {
	Scope     metricrollup.Scope `json:"scope"`
	UID       *int               `json:"uid,omitempty"`
	StartTime string             `json:"start_time,omitempty"`
	EndTime   string             `json:"end_time,omitempty"`
	Limit     int                `json:"limit,omitempty"`
}

// GetMetricRollupResult makes compacted-only, sample-weighted coverage explicit.
type GetMetricRollupResult struct {
	Entries            []metricrollup.Entry `json:"entries"`
	StartTime          string               `json:"start_time"`
	EndTime            string               `json:"end_time"`
	RawSamplesIncluded bool                 `json:"raw_samples_included"`
	Averaging          string               `json:"averaging"`
}

func (s *Server) registerMetricRollupTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{Name: "get_metrics_rollups", Description: "Get compacted UTC-hour observation statistics with known/unknown sample counts; raw samples are not mixed in"}, s.handleGetMetricRollups)
}
func (s *Server) handleGetMetricRollups(ctx context.Context, _ *mcp.CallToolRequest, args GetMetricRollupArgs) (*mcp.CallToolResult, GetMetricRollupResult, error) {
	if s.dbManager == nil {
		return nil, GetMetricRollupResult{}, fmt.Errorf("metrics database is not enabled")
	}
	now := time.Now().UTC()
	start, end := now.Add(-24*time.Hour), now
	var err error
	if args.StartTime != "" {
		start, err = time.Parse(time.RFC3339, args.StartTime)
		if err != nil {
			return nil, GetMetricRollupResult{}, fmt.Errorf("invalid start_time: %w", err)
		}
	}
	if args.EndTime != "" {
		end, err = time.Parse(time.RFC3339, args.EndTime)
		if err != nil {
			return nil, GetMetricRollupResult{}, fmt.Errorf("invalid end_time: %w", err)
		}
	}
	entries, err := s.dbManager.GetMetricRollups(ctx, metricrollup.Query{Scope: args.Scope, UID: args.UID, Start: start, End: end, Limit: args.Limit})
	if err != nil {
		return nil, GetMetricRollupResult{}, err
	}
	result := GetMetricRollupResult{Entries: entries, StartTime: start.UTC().Format(time.RFC3339), EndTime: end.UTC().Format(time.RFC3339), Averaging: "sample_weighted"}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: toJSON(result)}}, StructuredContent: result}, result, nil
}
