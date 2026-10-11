// Command mock-upstream is a controllable fake model provider for load and failure
// tests. It will serve the same native protocols as real providers.
package main

import "github.com/bibi45c/gateflow/pkg/server"

func main() {
	server.Main("mock-upstream", ":8090", nil)
}
