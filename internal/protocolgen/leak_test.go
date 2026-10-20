package main

import (
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/leakcheck"
)

func TestMain(m *testing.M) { leakcheck.Main(m) }
