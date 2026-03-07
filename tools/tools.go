//go:build tools

// Package tools pins tool dependencies so that go mod tidy keeps their
// transitive requirements in go.sum and `go run` works without extra steps.
package tools

import (
	_ "github.com/swaggo/swag/cmd/swag"
)
