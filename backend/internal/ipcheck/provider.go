// Package ipcheck maps per-provider IP network types and checks residential
// recovery eligibility using independent service evidence.
package ipcheck

type Evidence struct {
	QueryStatus string `json:"queryStatus,omitempty"`
	HTTPStatus  int    `json:"httpStatus,omitempty"`
	Provider    string `json:"provider"`
	CheckedAt   string `json:"checkedAt"`
	IP          string `json:"ip"`
	ASN         string `json:"asn,omitempty"`
	ASNType     string `json:"asnType,omitempty"`
	CompanyType string `json:"companyType,omitempty"`
	UsageType   string `json:"usageType,omitempty"`
	Hosting     *bool  `json:"hosting,omitempty"`
	VPN         bool   `json:"vpn"`
	Proxy       bool   `json:"proxy"`
	Outcome     string `json:"outcome"`
}
