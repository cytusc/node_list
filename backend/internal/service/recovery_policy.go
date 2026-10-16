package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// Reviewed against the production ASN/organization inventory on 2026-10-16.
// These networks provide cloud/CDN/hosting services. Do not infer a whole ASN
// is hosting from a majority of IP labels: access/transit networks can be mixed.
var recoveryHostingASNs = map[string]string{
	"AS13335": "Cloudflare", "AS45102": "Alibaba Cloud",
	"AS37963": "Alibaba Cloud", "AS132203": "Tencent Cloud",
	"AS212238": "Datacamp", "AS60068": "Datacamp", "AS9009": "M247",
	"AS12876": "Scaleway", "AS47172": "Greenhost", "AS16276": "OVH",
	"AS16509": "Amazon", "AS14618": "Amazon", "AS31898": "Oracle Cloud",
	"AS62240": "Clouvider", "AS14061": "DigitalOcean", "AS24940": "Hetzner",
	"AS20473": "Vultr", "AS207957": "SERV.HOST", "AS63023": "GTHost",
	"AS36352": "HostPapa", "AS57043": "HOSTKEY", "AS141995": "Contabo",
	"AS51167": "Contabo", "AS210644": "AEZA", "AS21859": "Zenlayer",
	"AS63150": "BAGE CLOUD", "AS8075": "Microsoft", "AS63949": "Akamai Connected Cloud",
	"AS197540": "netcup", "AS154177": "LightNode", "AS9370": "SAKURA",
	"AS60404": "Liteserver", "AS8560": "IONOS", "AS62563": "GlobalTeleHost",
	"AS215730": "H2NEXUS", "AS61112": "AkileCloud", "AS63018": "Dedicated.com",
	"AS59711": "HZ Hosting", "AS31034": "Aruba", "AS201988": "VPSPay",
	"AS906": "DMIT", "AS396982": "Google Cloud", "AS9123": "TIMEWEB",
	"AS397966": "ReadyDedis", "AS397423": "Tier.Net", "AS200019": "ALEXHOST",
	"AS25198": "ZetServers", "AS51852": "Private Layer", "AS214640": "Hostup",
	"AS44259": "Ultahost",
}

func normalizeASN(value string) string {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(value)))
	if len(fields) == 0 {
		return ""
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(fields[0], "AS"), 10, 32)
	if err != nil || n == 0 {
		return ""
	}
	return "AS" + strconv.FormatUint(n, 10)
}

// Only metadata for the exact observed exit IP is usable. Server addresses,
// names and organization substrings cannot establish the exit network.
func recoveryASN(attr *domain.IPPureResult, ip string) string {
	if attr == nil || attr.IP != ip || ip == "" {
		return ""
	}
	if asn := normalizeASN(attr.ASN); asn != "" {
		return asn
	}
	asn := ""
	if attr.Classification != nil && attr.Classification.IP == ip {
		for _, e := range attr.Classification.Evidence {
			if e.IP != ip || e.QueryStatus != "success" {
				continue
			}
			candidate := normalizeASN(e.ASN)
			if candidate == "" {
				continue
			}
			if asn != "" && asn != candidate {
				return ""
			}
			asn = candidate
		}
	}
	return asn
}

func hostingRecoveryASN(attr *domain.IPPureResult, ip string) string {
	asn := recoveryASN(attr, ip)
	if _, ok := recoveryHostingASNs[asn]; ok {
		return asn
	}
	return ""
}

// Retire known hosting candidates in one database pass before admission, so a
// large excluded backlog does not consume the limited probe/coordinator slots.
func (r *Repository) skipHostingRecovery(ctx context.Context) error {
	asns := make([]string, 0, len(recoveryHostingASNs))
	for asn := range recoveryHostingASNs {
		asns = append(asns, strings.TrimPrefix(asn, "AS"))
	}
	_, err := r.Pool.Exec(ctx, `UPDATE node_explorer_work w SET state='done',
 payload=(w.payload-'startedAt') || jsonb_build_object('recoveryResult','skipped-datacenter-asn',
 'checkedAt',to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
 'ip',e.ip,'asn',a.payload->>'asn'),error='',lease_token=NULL,lease_until=NULL
 FROM node_explorer_exits e JOIN node_explorer_ip_attributes a ON a.ip=e.ip
 WHERE w.kind='recovery' AND w.state='queued' AND NOT(w.payload ? 'startedAt')
 AND e.identity=w.payload->>'identity' AND a.payload->>'ip'=e.ip
 AND regexp_replace(upper(split_part(trim(a.payload->>'asn'),' ',1)),'^AS','')=ANY($1::text[])`, asns)
	return err
}
