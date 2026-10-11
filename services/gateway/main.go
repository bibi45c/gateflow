// Command gateway is the model gateway service: it serves the native model APIs
// (/v1/messages, /v1/responses) to local coding tools, routes requests to approved
// channels and relays streamed output.
package main

import "github.com/bibi45c/gateflow/pkg/server"

func main() {
	server.Main("gateway", ":8080", nil)
}
