package ipcheck

import "strings"

// TypeEvidence keeps the usage and company classifications separate, as in
// IPQuality's database comparison. Labels describe the source's classification,
// independently of its hosting/VPN risk signals or residential recovery votes.
type TypeEvidence struct {
	Provider    string `json:"provider"`
	UsageType   string `json:"usageType,omitempty"`
	CompanyType string `json:"companyType,omitempty"`
}

var typeLabels = map[string]string{
	"isp": "住宅IP", "business": "商业宽带", "hosting": "机房IP",
	"mobile": "移动网络", "education": "教育IP", "government": "政府IP",
	"banking": "银行网络", "organization": "组织网络", "military": "军事网络",
	"library": "图书馆网络", "cdn": "CDN", "spider": "搜索引擎", "reserved": "保留IP",
	"other": "其他网络",
}

// networkType mirrors IPQuality's ASN/company, AbuseIPDB usageType and
// IP2Location usage-code mappings. Missing fields do not imply a network type.
func networkType(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", "null", "unknown", "n/a", "-":
		return ""
	case "isp", "lineisp", "fixed line isp":
		return "isp"
	case "business", "enterprise", "commercial", "com":
		return "business"
	case "hosting", "data center/web hosting/transit", "dch":
		return "hosting"
	case "mobile", "mobile isp", "mob":
		return "mobile"
	case "education", "university/college/school", "edu":
		return "education"
	case "government", "gov":
		return "government"
	case "banking":
		return "banking"
	case "organization", "org":
		return "organization"
	case "military", "mil":
		return "military"
	case "library", "lib":
		return "library"
	case "cdn", "content delivery network":
		return "cdn"
	case "spider", "search engine spider", "ses":
		return "spider"
	case "reserved", "rsv":
		return "reserved"
	}
	// IP2Location can return multiple codes (e.g. ISP/MOB); its script uses
	// the first one. Do not split the long AbuseIPDB usage descriptions above.
	if first, _, found := strings.Cut(value, "/"); found {
		switch first {
		case "com", "dch", "edu", "gov", "org", "mil", "lib", "cdn", "isp", "mob", "ses", "rsv":
			return networkType(first)
		}
	}
	for kind, label := range typeLabels {
		if value == strings.ToLower(label) {
			return kind
		}
	}
	return "other"
}

// NetworkTypeLabel is shared with IPPure's normalization so every source uses
// the same labels for display and filtering.
func NetworkTypeLabel(raw string) string { return typeLabels[networkType(raw)] }

func (e Evidence) networkTypes() TypeEvidence {
	usage := e.UsageType
	if usage == "" {
		usage = e.ASNType
	}
	return TypeEvidence{Provider: e.Provider, UsageType: NetworkTypeLabel(usage), CompanyType: NetworkTypeLabel(e.CompanyType)}
}

// Recovery needs explicit ISP evidence. A false hosting flag alone says
// nothing about whether the IP is residential, mobile, business or education.
func (e *Evidence) classifyUsage() {
	types := e.networkTypes()
	switch {
	case e.Hosting != nil && *e.Hosting, types.UsageType == typeLabels["hosting"], types.CompanyType == typeLabels["hosting"]:
		e.Outcome = "provider-conflict"
	case types.UsageType == typeLabels["isp"], types.UsageType == "" && types.CompanyType == typeLabels["isp"]:
		e.Outcome = "supports-residential"
	case e.Hosting != nil && !*e.Hosting:
		e.Outcome = "non-hosting"
	default:
		e.Outcome = "unknown"
	}
}
