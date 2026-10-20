package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MalformedCleanupReport deliberately excludes identities and source parameters.
type MalformedCleanupReport struct {
	Candidates int            `json:"candidates"`
	Preserved  int            `json:"preservedAlternateOrActive"`
	Reasons    map[string]int `json:"reasons"`
	Deleted    int            `json:"deleted"`
}

// malformedReason is deliberately narrower than ShareLinkProxy: unsupported
// optional/default parameters and unknown formats survive.
func malformedReason(n domain.Node) string {
	rawLink := strings.TrimSpace(n.RawLink)
	if len(n.RawConfig) == 0 && rawLink == "" {
		return "missing-full-config"
	}
	var protocol, uuid string
	var port any
	missingRequired := ""
	if len(n.RawConfig) > 0 {
		protocol, _ = n.RawConfig["type"].(string)
		if protocol == "" {
			protocol = strings.ToLower(n.Type)
		}
		uuid, _ = n.RawConfig["uuid"].(string)
		port = n.RawConfig["port"]
		if (protocol == "hysteria" || protocol == "hysteria2") && n.RawConfig["ports"] != nil {
			port = nil // An alternate port-hopping range can override this field.
		}
		if isSupportedConnectionType(protocol) {
			server, _ := n.RawConfig["server"].(string)
			missingPort := port == nil || fmt.Sprint(port) == ""
			if strings.TrimSpace(server) == "" || (missingPort && n.RawConfig["ports"] == nil) {
				return "missing-required-connection-field"
			}
		}
		if protocol == "tuic" && n.RawConfig["token"] != nil {
			uuid = "" // TUIC v4 uses token authentication.
		}
		if protocol == "vless" && strings.TrimSpace(uuid) == "" {
			missingRequired = "missing-required-authentication"
		}
		if protocol == "vless" {
			if reality, ok := n.RawConfig["reality-opts"].(map[string]any); ok && strings.TrimSpace(stringValue(reality["public-key"])) == "" {
				missingRequired = "missing-reality-public-key"
			}
		}
	} else if strings.HasPrefix(rawLink, "vmess://") {
		var raw []byte
		var err error
		for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
			raw, err = encoding.DecodeString(strings.TrimPrefix(rawLink, "vmess://"))
			if err == nil {
				break
			}
		}
		var v map[string]any
		if err != nil || json.Unmarshal(raw, &v) != nil {
			return ""
		}
		protocol, uuid, port = "vmess", stringValue(v["id"]), v["port"]
	} else if rawLink != "" {
		u, err := url.Parse(strings.ReplaceAll(rawLink, "&amp;", "&"))
		if err != nil {
			return ""
		}
		protocol = strings.ToLower(u.Scheme)
		port = u.Port()
		if protocol == "vless" {
			if u.User == nil || strings.TrimSpace(u.User.Username()) == "" {
				missingRequired = "missing-required-authentication"
			}
			if u.Query().Get("security") == "reality" && strings.TrimSpace(u.Query().Get("pbk")) == "" {
				missingRequired = "missing-reality-public-key"
			}
		}
		if (protocol == "hy2" || protocol == "hysteria2") && u.Query().Get("mport") != "" {
			port = nil
		}
		if protocol == "tuic" && u.User != nil {
			// A token-only URL can describe TUIC v4. The share-link parser
			// accepts a UUID token and an explicit username/password pair, so
			// the UUID evidence must track exactly that contract: a plain
			// user:pass pair is a valid credential shape and must never be
			// judged as a malformed UUID here.
			if password, present := u.User.Password(); present && password != "" {
				if tuicUUIDShape(u.User.Username()) {
					uuid = u.User.Username()
				} else {
					uuid = ""
				}
			} else {
				uuid = u.User.Username()
			}
		}
	} else {
		return "" // Unknown formats remain for review.
	}
	switch protocol {
	case "vmess", "tuic", "vless", "ss", "ssr", "trojan", "hysteria", "hysteria2", "hy2", "socks", "socks5", "http", "https":
	default:
		return ""
	}
	if missingRequired != "" {
		return missingRequired
	}
	if port != nil && fmt.Sprint(port) != "" {
		v, err := strconv.ParseFloat(fmt.Sprint(port), 64)
		if err != nil || v < 1 || v > 65535 || v != float64(int64(v)) {
			return "invalid-explicit-port"
		}
	}
	// Do not impose UUID rules on VLESS implementations that support custom IDs.
	if (protocol == "tuic" || protocol == "vmess") && uuid != "" {
		value := uuid
		if len(value) == 36 {
			if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
				return "invalid-" + protocol + "-uuid"
			}
			value = value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
		}
		if len(value) != 32 {
			return "invalid-" + protocol + "-uuid"
		}
		for _, c := range value {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return "invalid-" + protocol + "-uuid"
			}
		}
	}
	return ""
}

func isSupportedConnectionType(protocol string) bool {
	switch protocol {
	case "vmess", "tuic", "vless", "ss", "ssr", "trojan", "hysteria", "hysteria2", "hy2", "socks", "socks5", "http", "https":
		return true
	default:
		return false
	}
}

func stringValue(v any) string { s, _ := v.(string); return s }

// tuicUUIDShape mirrors domain.normalizeUUID's accepted shapes: a 36-char
// dashed hex form or a compact 32-char hex form. Keeping the check local
// avoids exporting a parser helper while staying byte-for-byte consistent
// with the share-link contract.
func tuicUUIDShape(v string) bool {
	if len(v) == 32 {
		return isHexValue(v)
	}
	if len(v) != 36 {
		return false
	}
	return isHexValue(v[:8]) && isHexValue(v[9:13]) && isHexValue(v[14:18]) && isHexValue(v[19:23]) && isHexValue(v[24:])
}

func isHexValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// CleanMalformedNodes is an explicit administrative operation, never scheduled.
// The caller must pause writers and take a database backup before apply.
func CleanMalformedNodes(ctx context.Context, pool *pgxpool.Pool, apply bool, backup string) (MalformedCleanupReport, error) {
	report := MalformedCleanupReport{Reasons: map[string]int{}}
	if apply && backup == "" {
		return report, fmt.Errorf("apply requires a backup reference")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='10s'`); err != nil {
		return report, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); err != nil {
		return report, err
	}
	if apply {
		if _, err = tx.Exec(ctx, `LOCK TABLE node_explorer_snapshots,node_explorer_ippure_results,node_explorer_node_registry,node_explorer_work IN EXCLUSIVE MODE`); err != nil {
			return report, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT n.identity,n.node,
 EXISTS(SELECT 1 FROM node_explorer_work w WHERE w.payload->>'identity'=n.identity AND w.state='running' AND w.lease_until>now())
	 FROM node_explorer_node_registry n WHERE
	 ((coalesce(n.node->>'subTag','')='Qilan' AND n.node->>'type' IN ('vless','ss','vmess','trojan')
	   AND btrim(coalesce(n.node->>'rawLink',''))=''
	   AND (jsonb_typeof(n.node->'rawConfig') IS DISTINCT FROM 'object' OR n.node->'rawConfig'='{}'::jsonb))
	 OR EXISTS(SELECT 1 FROM node_explorer_work w
	 WHERE w.payload->>'identity'=n.identity AND w.error LIKE 'node_config_rejected:%')) ORDER BY n.identity`)
	if err != nil {
		return report, err
	}
	selected := map[string]string{}
	for rows.Next() {
		var id string
		var raw []byte
		var active bool
		var node domain.Node
		if err = rows.Scan(&id, &raw, &active); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &node); err != nil {
			break
		}
		if reason := malformedReason(node); reason != "" {
			report.Candidates++
			if active {
				report.Preserved++
			} else {
				selected[id] = reason
			}
		}
	}
	rows.Close()
	if err != nil {
		return report, err
	}
	if rows.Err() != nil {
		return report, rows.Err()
	}
	// Any alternate source without independently proven damage protects the identity.
	rows, err = tx.Query(ctx, `SELECT payload FROM node_explorer_snapshots ORDER BY id`)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var raw []byte
		var snapshot struct {
			Nodes []domain.Node `json:"nodes"`
		}
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &snapshot); err != nil {
			break
		}
		for _, node := range snapshot.Nodes {
			id := domain.AggregateIdentity(node)
			if _, ok := selected[id]; ok && malformedReason(node) == "" {
				delete(selected, id)
				report.Preserved++
			}
		}
	}
	rows.Close()
	if err != nil {
		return report, err
	}
	if rows.Err() != nil {
		return report, rows.Err()
	}
	ids := make([]string, 0, len(selected))
	for id, reason := range selected {
		ids = append(ids, id)
		report.Reasons[reason]++
	}
	if !apply || len(ids) == 0 {
		return report, nil
	}
	if err = removeSnapshotIdentities(ctx, tx, ids); err != nil {
		return report, err
	}
	for _, table := range []string{"node_explorer_connectivity", "node_explorer_exits", "node_explorer_retired_nodes", "node_explorer_probe_results"} {
		if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE identity=ANY($1::text[])`, ids); err != nil {
			return report, err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_work WHERE payload->>'identity'=ANY($1::text[]) AND kind NOT IN ('attributes','risk')`, ids); err != nil {
		return report, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_node_registry WHERE identity=ANY($1::text[])`, ids); err != nil {
		return report, err
	}
	var legacy bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('node_explorer_current_nodes') IS NOT NULL`).Scan(&legacy); err != nil {
		return report, err
	}
	if legacy {
		if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_current_nodes WHERE identity=ANY($1::text[])`, ids); err != nil {
			return report, err
		}
	}
	for id, reason := range selected {
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_deleted_nodes(identity,deleted_at) VALUES($1,clock_timestamp()) ON CONFLICT(identity) DO UPDATE SET deleted_at=EXCLUDED.deleted_at`, id); err != nil {
			return report, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,'delete',false,$2)`, id, "explicit malformed-config cleanup: "+reason+"; backup "+backup); err != nil {
			return report, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return report, err
	}
	report.Deleted = len(ids)
	return report, nil
}
