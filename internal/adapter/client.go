package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FeedClient reads the board communication feed without exposing its token.
type FeedClient interface {
	Read(context.Context, string) (FeedPage, error)
}

// MCPFeedClient reads board_get(view:"messages") over authenticated MCP HTTP.
type MCPFeedClient struct {
	Endpoint string
	Token    string
	Project  string
}

// Read reads after cursor. The token stays in this process and is never written
// to State, task metadata, or a project message.
func (c MCPFeedClient) Read(ctx context.Context, after string) (FeedPage, error) {
	endpoint := strings.TrimRight(c.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/mcp") {
		endpoint += "/mcp"
	}
	hc := &http.Client{Transport: bearerTransport{token: c.Token, base: http.DefaultTransport}}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "kanban-adapter", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &gomcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return FeedPage{}, fmt.Errorf("connect to board MCP: %w", err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: "board_get", Arguments: map[string]any{"project": c.Project, "view": "messages", "after": after, "limit": 100}})
	if err != nil {
		return FeedPage{}, fmt.Errorf("read board feed: %w", err)
	}
	if result.IsError {
		return FeedPage{}, fmt.Errorf("board_get returned an error")
	}
	if result.StructuredContent != nil {
		page, err := decodeFeed(result.StructuredContent)
		if err == nil {
			return page, nil
		}
	}
	for _, content := range result.Content {
		text, ok := content.(*gomcp.TextContent)
		if !ok {
			continue
		}
		var payload any
		if json.Unmarshal([]byte(text.Text), &payload) == nil {
			if page, err := decodeFeed(payload); err == nil {
				return page, nil
			}
		}
	}
	return FeedPage{}, fmt.Errorf("board_get response did not include structured messages")
}

func decodeFeed(value any) (FeedPage, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return FeedPage{}, err
	}
	var payload struct {
		Messages FeedPage `json:"messages"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return FeedPage{}, err
	}
	if payload.Messages.Messages == nil && payload.Messages.NextCursor == "" {
		return FeedPage{}, fmt.Errorf("messages payload absent")
	}
	return payload.Messages, nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
