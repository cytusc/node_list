// Command github-patrol is a migration notice for the retired monolithic worker.
package main

import "log"

func main() {
	log.Fatal("github-patrol has been replaced: run node-worker -role connectivity, -role residential and -role lifecycle; see MODULAR_SERVICES.md")
}
