// Command metering is the quota and metering service: it owns wallets,
// reservations, settlements and the ledger, and answers usage queries.
package main

import "github.com/bibi45c/gateflow/pkg/server"

func main() {
	server.Main("metering", ":8082", nil)
}
