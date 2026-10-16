package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed live.sql
var liveSchema string

// LiveReader reads current facts without loading archives or maintaining a projection.
type LiveReader struct{ pool *pgxpool.Pool }

func NewLiveReader(pool *pgxpool.Pool) *LiveReader { return &LiveReader{pool: pool} }

// Only filter, facet and sort fields enter the shared request-local CTE.
// Complete public node documents are rendered after the page is selected.
const liveRows = `WITH nodes AS MATERIALIZED (
 SELECT identity,snapshot_id,node_index,country,
 coalesce(nullif(country,''),'unknown') AS region,
 node->>'name' AS name,node->>'baseName' AS base_name,node->>'server' AS server,node->>'sni' AS sni,
 node->>'type' AS node_type,
 coalesce(nullif(node->>'type',''),'unknown') AS protocol,
 coalesce(nullif(node->>'network',''),'unknown') AS network,
 coalesce((node->>'speed')::double precision,0) AS speed,
 coalesce((node->>'tls')::boolean,false) AS tls,
 coalesce((node->>'reality')::boolean,false) AS reality,
 coalesce((node->>'udp')::boolean,false) AS udp,
 (node->>'speedTest')::boolean AS speed_test,(node->>'mediaCheck')::boolean AS media_check,
 node->>'checkedAt' AS checked_at,
 attributes->>'ip' AS attributes_ip,ip_status,attributes->>'source' AS ip_source,attributes->>'asnOrganization' AS asn_organization,
 (attributes->>'score')::double precision AS ip_score,(attributes->>'humanPercent')::double precision AS human_percent,
 attributes->'classification' AS classification,
 ARRAY(SELECT jsonb_array_elements_text(coalesce(nullif(attributes->'attributes','null'::jsonb),'[]'::jsonb))) AS ip_attributes,
 coalesce((connectivity->>'reachable')::boolean,false) AS reachable,
 connectivity->>'error' AS connectivity_error,connectivity->>'checkedAt' AS connectivity_at,
 exit_payload->>'ip' AS reported_exit_ip,exit_payload->>'error' AS exit_error,exit_payload->>'checkedAt' AS exit_at,
 ARRAY(SELECT DISTINCT coalesce(nullif(m->>'platform',''),nullif(m->>'Platform',''),nullif(m->>'tag',''),m->>'Tag')
 FROM jsonb_array_elements(coalesce(nullif(node->'media','null'::jsonb),'[]'::jsonb)) m
 WHERE coalesce(nullif(m->>'tag',''),nullif(m->>'Tag',''),nullif(m->>'platform',''),m->>'Platform','')<>'') AS media
 FROM node_explorer_live_facts
) `

// One statement shares a request-local CTE between metadata, facets, filters
// and the page. It observes one database snapshot and decodes only page nodes.
func (r *LiveReader) QueryPage(ctx context.Context, q *Query) (*Result, error) {
	var args []any
	var conditions []string
	param := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	list := func(column string, v []string) {
		if len(v) > 0 {
			conditions = append(conditions, column+"=ANY("+param(v)+"::text[])")
		}
	}
	if q != nil {
		list("region", q.Countries)
		list("protocol", q.Types)
		list("network", q.Networks)
		for _, f := range q.Features {
			if featureColumns[f] {
				conditions = append(conditions, f)
			}
		}
		if q.Search != "" {
			var countries []string
			for a := 'A'; a <= 'Z'; a++ {
				for b := 'A'; b <= 'Z'; b++ {
					code := string([]rune{a, b})
					if strings.Contains(strings.ToLower(domain.CountryName(code)), q.Search) {
						countries = append(countries, code)
					}
				}
			}
			term := param(q.Search)
			conditions = append(conditions, `(EXISTS(SELECT 1 FROM unnest(ARRAY[name,base_name,server,sni,country]) v WHERE strpos(lower(v),`+term+`)>0) OR region=ANY(`+param(countries)+`::text[]))`)
		}
		if len(q.IPSources) > 0 {
			conditions = append(conditions, `ip_status='success'`)
			list("ip_source", q.IPSources)
		}
		if len(q.IPAttributes) > 0 {
			conditions = append(conditions, `(ip_status='success' OR jsonb_typeof(classification)='object') AND ip_attributes && `+param(q.IPAttributes)+`::text[]`)
		}
		if q.MinSpeed > 0 {
			conditions = append(conditions, "(NOT (SELECT speed_test FROM metadata) OR speed>="+param(q.MinSpeed*1024)+")")
		}
		if len(q.Media) > 0 {
			conditions = append(conditions, "(NOT (SELECT media_check FROM metadata) OR "+param(q.Media)+"::text[] <@ media)")
		}
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	order := "snapshot_id DESC,node_index,identity"
	options := "SELECT 0::bigint AS page,0::bigint AS page_limit"
	paging := ""
	if q != nil {
		if q.Page < 1 || q.Limit < 1 {
			return nil, fmt.Errorf("invalid pagination")
		}
		pageParam, limitParam := param(q.Page), param(q.Limit)
		options = "SELECT least(" + pageParam + "::bigint,greatest(1,(total+" + limitParam + "::bigint-1)/" + limitParam + ")) AS page," + limitParam + "::bigint AS page_limit FROM tally"
		paging = " LIMIT (SELECT page_limit FROM page_options) OFFSET (SELECT (page-1)*page_limit FROM page_options)"
		direction := "ASC"
		if q.Direction == "desc" {
			direction = "DESC"
		}
		switch q.Sort {
		case "", "preferred":
			order = livePreferred + " DESC NULLS LAST,CASE WHEN (SELECT speed_test FROM metadata) THEN speed END DESC NULLS LAST,CASE WHEN NOT (SELECT speed_test FROM metadata) THEN " + liveSort["name"] + " END ASC NULLS LAST," + order
		case "speed":
			order = "CASE WHEN (SELECT speed_test FROM metadata) THEN speed END " + direction + " NULLS LAST,CASE WHEN NOT (SELECT speed_test FROM metadata) THEN " + liveSort["name"] + " END ASC NULLS LAST," + order
		default:
			expr, ok := liveSort[q.Sort]
			if !ok {
				return nil, fmt.Errorf("unsupported sort")
			}
			order = expr + " " + direction + " NULLS LAST," + order
		}
	}
	query := liveRows + `, metadata AS (
 SELECT count(*) AS total_nodes,count(DISTINCT nullif(country,'')) AS countries,
 coalesce(max(speed) FILTER(WHERE speed>0),0) AS max_speed,
 coalesce(percentile_cont(0.5) WITHIN GROUP(ORDER BY speed) FILTER(WHERE speed>0),0) AS median,
 coalesce(bool_or(speed_test),false) AS speed_test,
 coalesce(bool_or(media_check),false) AS media_check,
 coalesce(max(checked_at),'') AS checked_at FROM nodes
), facets AS (
 SELECT kind,value,count(*) AS count FROM nodes CROSS JOIN LATERAL (
 VALUES ('country',region),('type',protocol),('network',network),
 ('ipSource',CASE WHEN ip_status='success' THEN nullif(ip_source,'') END),
 ('features',CASE WHEN tls THEN 'tls' END),('features',CASE WHEN reality THEN 'reality' END),('features',CASE WHEN udp THEN 'udp' END)
 UNION ALL SELECT 'ipAttributes',value FROM (SELECT DISTINCT unnest(ip_attributes) AS value) a
 WHERE ip_status='success' OR jsonb_typeof(classification)='object'
 UNION ALL SELECT 'media',unnest(media)
 ) f(kind,value) WHERE value IS NOT NULL AND value<>'' GROUP BY kind,value
), filtered AS MATERIALIZED (SELECT * FROM nodes` + where + `),
tally AS (SELECT count(*) AS total FROM filtered),
page_options AS (` + options + `),
page_candidates AS MATERIALIZED (SELECT identity,row_number() OVER(ORDER BY ` + order + `) AS ordinal FROM filtered ORDER BY ` + order + paging + `),
page_nodes AS (SELECT v.payload,p.ordinal FROM page_candidates p JOIN node_explorer_live_nodes v USING(identity))
SELECT total_nodes,countries,max_speed,median,speed_test,media_check,checked_at,
 (SELECT coalesce(jsonb_agg(jsonb_build_object('kind',kind,'value',value,'count',count) ORDER BY count DESC,value COLLATE node_explorer_numeric),'[]'::jsonb) FROM facets),
 (SELECT total FROM tally),(SELECT page FROM page_options),(SELECT page_limit FROM page_options),
 (SELECT coalesce(jsonb_agg(payload ORDER BY ordinal),'[]'::jsonb) FROM page_nodes)
 FROM metadata`
	out := &Result{}
	var facetsRaw, nodesRaw []byte
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&out.TotalNodes, &out.Summary.Countries, &out.Summary.MaxSpeed, &out.Summary.Median,
		&out.SpeedTest, &out.MediaCheck, &out.CheckedAt, &facetsRaw, &out.Total, &out.Page, &out.Limit, &nodesRaw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(nodesRaw, &out.Nodes); err != nil {
		return nil, err
	}
	var facets []struct {
		Kind  string
		Value string
		Count int
	}
	if err := json.Unmarshal(facetsRaw, &facets); err != nil {
		return nil, err
	}
	out.Facets = domain.Facets{Country: []domain.Facet{}, Type: []domain.Facet{}, Network: []domain.Facet{}, IPSource: []domain.Facet{}, IPAttributes: []domain.Facet{}, Features: []domain.Facet{}, Media: []domain.Facet{}}
	for _, entry := range facets {
		f := domain.Facet{Value: entry.Value, Label: entry.Value, Count: entry.Count}
		switch entry.Kind {
		case "country":
			f.Label = domain.CountryName(f.Value)
			out.Facets.Country = append(out.Facets.Country, f)
		case "type":
			f.Label = strings.ToUpper(f.Value)
			out.Facets.Type = append(out.Facets.Type, f)
		case "network":
			f.Label = strings.ToUpper(f.Value)
			out.Facets.Network = append(out.Facets.Network, f)
		case "ipSource":
			out.Facets.IPSource = append(out.Facets.IPSource, f)
		case "ipAttributes":
			out.Facets.IPAttributes = append(out.Facets.IPAttributes, f)
		case "features":
			out.Facets.Features = append(out.Facets.Features, f)
		case "media":
			out.Facets.Media = append(out.Facets.Media, f)
		}
	}
	features := out.Facets.Features
	out.Facets.Features = []domain.Facet{}
	for _, f := range []domain.Facet{{Value: "tls", Label: "TLS"}, {Value: "reality", Label: "Reality"}, {Value: "udp", Label: "UDP"}} {
		for _, v := range features {
			if v.Value == f.Value {
				f.Count = v.Count
			}
		}
		out.Facets.Features = append(out.Facets.Features, f)
	}
	return out, nil
}

var liveSort = map[string]string{
	"country":         `nullif(country,'') COLLATE node_explorer_numeric`,
	"name":            `coalesce(nullif(btrim(regexp_replace(coalesce(nullif(base_name,''),name),'[🇦-🇿]','','g')),''),nullif(name,'')) COLLATE node_explorer_numeric`,
	"type":            `nullif(node_type,'') COLLATE node_explorer_numeric`,
	"speed":           `speed`,
	"asnOrganization": `(CASE WHEN ip_status='success' THEN nullif(asn_organization,'') END) COLLATE node_explorer_numeric`,
	"ipSource":        `(CASE WHEN ip_status='success' THEN nullif(ip_source,'') END) COLLATE node_explorer_numeric`,
	"ipAttributes":    `(CASE WHEN ip_status='success' OR jsonb_typeof(classification)='object' THEN nullif(array_to_string(ip_attributes,' · '),'') END) COLLATE node_explorer_numeric`,
	"ippure":          `CASE WHEN ip_status='success' THEN coalesce(ip_score,0) END`,
	"trafficRatio":    `CASE WHEN ip_status='success' THEN human_percent END`,
}

var livePreferred = fmt.Sprintf(`(reachable
 AND coalesce(connectivity_error,'')='' AND coalesce(exit_error,'')=''
 AND coalesce(reported_exit_ip,'')<>''
 AND connectivity_at::timestamptz BETWEEN now()-interval '10 minutes' AND now()+interval '1 minute'
 AND exit_at::timestamptz BETWEEN now()-interval '10 minutes' AND now()+interval '1 minute'
 AND classification->>'state'='residential'
 AND classification->>'version'='%d'
 AND attributes_ip=reported_exit_ip
 AND classification->>'ip'=reported_exit_ip
 AND (classification->>'checkedAt')::timestamptz BETWEEN now()-interval '24 hours' AND now()+interval '1 minute'
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce(nullif(classification->'evidence','null'::jsonb),'[]'::jsonb)) e
 WHERE nullif(e->>'checkedAt','') IS NULL OR NOT((e->>'checkedAt')::timestamptz BETWEEN now()-interval '24 hours' AND now()+interval '1 minute')))`, ipcheck.VotingVersion)
