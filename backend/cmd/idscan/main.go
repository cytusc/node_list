// Command idscan computes an aggregate identity for every node on stdin and
// prints "<rawLink>\t<identity>\t<connection>". Each input line is a JSON node,
// which is what a snapshot payload element looks like.
//
// It exists to cross-check the Go identity against the JavaScript identity over
// every link the service has ever seen, not just the ones currently projected.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<26)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	total, failed := 0, 0
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var node domain.Node
		if err := json.Unmarshal([]byte(line), &node); err != nil {
			fmt.Fprintf(os.Stderr, "decode: %v\n", err)
			os.Exit(1)
		}
		total++
		identity := domain.AggregateIdentity(node)
		connection := connectionOf(node)
		if identity == "" {
			failed++
		}
		out.WriteString(strings.ReplaceAll(node.RawLink, "\t", " "))
		out.WriteByte('\t')
		out.WriteString(identity)
		out.WriteByte('\t')
		out.WriteString(strings.ReplaceAll(connection, "\t", " "))
		out.WriteByte('\n')
	}
	if err := in.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "idscan: total=%d empty=%d\n", total, failed)
}

func connectionOf(node domain.Node) string {
	if len(node.RawConfig) == 0 && node.RawLink != "" {
		if connection, ok := domain.ShareLinkConnection(node.RawLink); ok {
			return "share-link:" + connection
		}
	}
	if len(node.RawConfig) == 0 {
		id, _ := domain.NodeIdentity(node)
		return id
	}
	id, _ := domain.NodeIdentity(node)
	return id + "|rawConfig"
}
