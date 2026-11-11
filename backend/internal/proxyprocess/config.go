// Package proxyprocess adapts stored nodes and runs private proxy processes.
package proxyprocess

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/cytusc/node_list/backend/internal/domain"
)

type Proxy = map[string]any

func text(p Proxy, key string) string {
	if v, ok := p[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Decode only adapts the stored node format. Execution never uses the old
// transport, sweeper, Runner or manager implementation.
func Decode(raw []byte) (Proxy, error) {
	var n domain.Node
	if json.Unmarshal(raw, &n) != nil {
		return nil, errors.New("invalid_node")
	}
	var p Proxy
	if len(n.RawConfig) > 0 {
		p = Proxy(n.RawConfig)
	} else {
		link := n.RawLink
		if link == "" {
			link = n.GeneratedLink
		}
		parsed, err := domain.ShareLinkProxy(link, "probe")
		if err != nil {
			var detail *domain.ShareLinkError
			if errors.As(err, &detail) {
				return nil, errors.New("share_link_" + detail.Code)
			}
			return nil, errors.New("invalid_config")
		}
		p = Proxy(parsed)
	}
	port, err := strconv.Atoi(text(p, "port"))
	if err != nil || port < 1 || port > 65535 || text(p, "server") == "" {
		return nil, errors.New("invalid_endpoint")
	}
	p["port"] = port
	p["name"] = "probe"
	return p, nil
}
