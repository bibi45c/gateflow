// Command access is the access and channel management service: it owns tenants,
// gateway keys, channels and price versions, and verifies keys for the gateway.
package main

import "github.com/bibi45c/gateflow/pkg/server"

func main() {
	server.Main("access", ":8081", nil)
}
