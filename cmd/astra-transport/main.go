package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"local.sub2api/openai-health/internal/adapter"
)

func main() {
	child, err := adapter.StartBundledChild(context.Background())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	server := adapter.New(child)
	defer server.Close()
	pluginv1.Serve(server)
}
