package main

import (
	"fmt"
	"os"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func main() {
	for _, link := range os.Args[1:] {
		identity := domain.AggregateIdentity(domain.Node{RawLink: link})
		fmt.Printf("%s\n  -> %s\n", link, identity)
	}
}
