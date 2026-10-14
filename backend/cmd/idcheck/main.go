// Command idcheck verifies that the Go identity hashes reproduce the identities
// already persisted by the Node.js service.
//
// Identities are the primary key of enrichment and retirement. A mismatch would
// orphan every stored IPPure result, revive retired nodes and re-queue the
// whole fleet, so this check gates the cutover.
//
// Usage:
//
//	idcheck              -- read the live node_explorer_current_nodes table (PG* env)
//	idcheck -file dump   -- read a local dump (JSON array or JSON lines of
//	                        {"identity": "...", "payload": "<node json>"})
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cytusc/node_list/backend/internal/domain"
	pgstore "github.com/cytusc/node_list/backend/internal/store/postgres"
)

type row struct {
	Identity string `json:"identity"`
	Payload  string `json:"payload"`
}

func main() {
	dump := flag.String("file", "", "local dump file instead of PG")
	conn := flag.String("conn", "", "write the Go hashing input per row to this file")
	flag.Parse()
	if err := run(*dump, *conn); err != nil {
		fmt.Fprintf(os.Stderr, "idcheck: %v\n", err)
		os.Exit(1)
	}
}

func run(dump, connOut string) error {
	var rows []row
	var err error
	if dump != "" {
		rows, err = readDump(dump)
	} else {
		rows, err = readPG()
	}
	if err != nil {
		return err
	}
	var connFile *os.File
	if connOut != "" {
		connFile, err = os.Create(connOut)
		if err != nil {
			return err
		}
		defer connFile.Close()
	}
	checked, mismatched := 0, 0
	var samples []string
	byType := map[string]int{}
	badByType := map[string]int{}
	for _, r := range rows {
		var node domain.Node
		if err := json.Unmarshal([]byte(r.Payload), &node); err != nil {
			return fmt.Errorf("decode node: %w", err)
		}
		identity := r.Identity
		computed := domain.AggregateIdentity(node)
		checked++
		byType[node.Type]++
		if connFile != nil {
			connection := connectionOf(node)
			fmt.Fprintf(connFile, "%s\t%s\t%s\n",
				identity, computed, strings.ReplaceAll(connection, "\t", " "))
		}
		if computed == identity {
			continue
		}
		mismatched++
		badByType[node.Type]++
		if len(samples) < 5 {
			samples = append(samples, fmt.Sprintf(
				"  type=%s server=%s name=%q\n    stored=%s\n    go    =%s",
				node.Type, node.Server, node.Name, identity, computed))
		}
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)
	fmt.Printf("identity parity: checked=%d mismatched=%d (%.4f%%)\n",
		checked, mismatched, percent(mismatched, checked))
	for _, t := range types {
		fmt.Printf("  %-12s total=%-6d mismatched=%d\n", t, byType[t], badByType[t])
	}
	for _, sample := range samples {
		fmt.Println(sample)
	}
	if mismatched > 0 {
		return fmt.Errorf("identity parity failed: %d/%d mismatched", mismatched, checked)
	}
	fmt.Println("identity parity OK")
	return nil
}

// connectionOf returns the exact string the identity hash is taken over.
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
	hash, err := canonicalHashFor(node.RawConfig)
	if err != nil {
		return ""
	}
	id, _ := domain.NodeIdentity(node)
	return id + hash
}

func canonicalHashFor(proxy domain.ProxyConfig) (string, error) {
	config := domain.ConnectionConfig(proxy)
	b, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func readPG() ([]row, error) {
	cfg, err := pgstore.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	adapter, err := pgstore.NewAdapter(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	defer adapter.Close()

	rows, err := adapter.Pool().Query(context.Background(),
		`SELECT identity, payload FROM node_explorer_current_nodes ORDER BY identity`)
	if err != nil {
		return nil, fmt.Errorf("read projection: %w", err)
	}
	defer rows.Close()
	out := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Identity, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readDump(path string) ([]row, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		out := []row{}
		if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	out := []row{}
	scanner := bufio.NewScanner(strings.NewReader(trimmed))
	scanner.Buffer(make([]byte, 1<<20), 1<<26)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", len(out)+1, err)
		}
		out = append(out, r)
	}
	return out, scanner.Err()
}

func percent(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}
