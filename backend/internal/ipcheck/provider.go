// Package ipcheck classifies IP usage with three independent service votes.
package ipcheck

type Evidence struct {
	Provider    string `json:"provider"`
	CheckedAt   string `json:"checkedAt"`
	IP          string `json:"ip"`
	ASN         string `json:"asn,omitempty"`
	ASNType     string `json:"asnType,omitempty"`
	CompanyType string `json:"companyType,omitempty"`
	Hosting     *bool  `json:"hosting,omitempty"`
	VPN         bool   `json:"vpn"`
	Proxy       bool   `json:"proxy"`
	Outcome     string `json:"outcome"`
}
