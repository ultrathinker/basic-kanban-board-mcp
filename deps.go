//go:build tools

// This file exists so `go mod tidy` keeps every dependency the project will use
// pinned in go.mod before the packages that import them are written. It is
// excluded from every real build by the `tools` tag.
//
// Implementing agents must never edit go.mod or go.sum: the versions here are
// the frozen contract, and four agents build against them in parallel.
package tools

import (
	_ "github.com/google/uuid"
	_ "github.com/microcosm-cc/bluemonday"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/yuin/goldmark"
	_ "modernc.org/sqlite"
)
