// Command linkdiag replays archived raw links through the current share-link
// parser and reports, per node, what the parser can still recover.
//
// It answers one question for a snapshot that looks incomplete: was a field
// lost while decoding the link, or absent from the link itself? Export the
// nodes of a snapshot as [{"stored": {...}}, ...] and diff the output against
// what the database holds: a field empty in the database but present in
// parsed.json was dropped by an importer, while a field empty in both was
// never in the link.
//
//	go run ./cmd/linkdiag nodes.json > parsed.json
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/cytusc/node_list/backend/internal/domain"
)

type input struct {
	Stored map[string]any `json:"stored"`
}

type result struct {
	Name    string         `json:"name"`
	Link    string         `json:"link"`
	OK      bool           `json:"ok"`
	Code    string         `json:"code,omitempty"`
	Proxy   map[string]any `json:"proxy,omitempty"`
	Summary domain.Node    `json:"summary,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: linkdiag <nodes.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var nodes []input
	if err = json.Unmarshal(raw, &nodes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var results []result
	for _, n := range nodes {
		link, _ := n.Stored["rawLink"].(string)
		name, _ := n.Stored["name"].(string)
		r := result{Name: name, Link: link}
		if link == "" {
			r.Code = "no-link"
			results = append(results, r)
			continue
		}
		proxy, parseErr := domain.ShareLinkProxy(link, name)
		if parseErr != nil {
			r.Code = "parse-error"
			var detail *domain.ShareLinkError
			if errors.As(parseErr, &detail) {
				r.Code = detail.Code
			}
			results = append(results, r)
			continue
		}
		r.OK = true
		r.Proxy = map[string]any(proxy)
		r.Summary = domain.SummarizeProxy(proxy)
		results = append(results, r)
	}
	out, err := json.MarshalIndent(results, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
