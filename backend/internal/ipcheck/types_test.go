package ipcheck

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestIPQualityNetworkTypeMappings(t *testing.T) {
	for _, tc := range []struct{ raw, label string }{
		{"business", "商业宽带"}, {"Commercial", "商业宽带"}, {" enterprise ", "商业宽带"},
		{"isp", "住宅IP"}, {"Fixed Line ISP", "住宅IP"}, {"ISP/MOB", "住宅IP"},
		{"Data Center/Web Hosting/Transit", "机房IP"}, {"hosting", "机房IP"}, {"DCH", "机房IP"},
		{"Mobile ISP", "移动网络"}, {"mobile", "移动网络"},
		{"University/College/School", "教育IP"}, {"education", "教育IP"},
		{"Government", "政府IP"}, {"Banking", "银行网络"}, {"Organization", "组织网络"},
		{"Military", "军事网络"}, {"Library", "图书馆网络"},
		{"Content Delivery Network", "CDN"}, {"Search Engine Spider", "搜索引擎"},
		{"Reserved", "保留IP"}, {"unrecognized-vendor-type", "其他网络"},
		{"", ""}, {"null", ""}, {"unknown", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := NetworkTypeLabel(tc.raw); got != tc.label {
				t.Fatalf("%q mapped to %q, want %q", tc.raw, got, tc.label)
			}
		})
	}
}

func TestBusinessUsageKeepsCompanyAndRiskSeparate(t *testing.T) {
	now := time.Now()
	e := ParseIPinfo([]byte(`{"data":{"ip":"8.8.8.8","asn":{"type":"Business"},"company":{"type":"ISP"},"privacy":{"hosting":false,"vpn":true}}}`), "8.8.8.8", now.Format(time.RFC3339Nano))
	r := Score("8.8.8.8", []Evidence{e, ParseIPAPI([]byte(`{"status":"success","query":"8.8.8.8","hosting":false}`), "8.8.8.8", e.CheckedAt)}, now)
	if r.State != "unknown" || r.ResidentialVotes != 0 {
		t.Fatalf("business/nonhosting became residential: %+v", r)
	}
	want := []TypeEvidence{{Provider: "ipinfo", UsageType: "商业宽带", CompanyType: "住宅IP"}}
	if !reflect.DeepEqual(r.Types, want) {
		t.Fatalf("usage/company fields lost: %+v", r.Types)
	}
	if got := r.Attributes([]string{"性质未知", "住宅IP", "商业宽带"}); !reflect.DeepEqual(got, []string{"商业宽带"}) {
		t.Fatalf("wrong attributes: %v", got)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var restored Result
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Types, want) || len(restored.Votes) != 0 {
		t.Fatalf("type evidence must persist independently of votes: %s", raw)
	}
}

func TestSourceDisagreementKeepsEachUsageType(t *testing.T) {
	now := time.Now()
	stamp := now.Format(time.RFC3339Nano)
	evidence := []Evidence{
		IPPureEvidence("8.8.8.8", []string{"商业宽带"}, stamp),
		ParseIPinfo([]byte(`{"ip":"8.8.8.8","asn":{"type":"hosting"},"company":{"type":"business"}}`), "8.8.8.8", stamp),
		ParseAbuseIPDB([]byte(`{"data":{"ipAddress":"8.8.8.8","usageType":"Mobile ISP"}}`), "8.8.8.8", stamp),
	}
	r := Score("8.8.8.8", evidence, now)
	if got := r.Attributes([]string{"性质争议", "住宅IP"}); !reflect.DeepEqual(got, []string{"住宅IP"}) {
		t.Fatalf("unresolved summary: %v", got)
	}
	if len(r.Types) != 3 || r.Types[1].CompanyType != "商业宽带" {
		t.Fatalf("missing database comparison: %+v", r.Types)
	}
}

func TestTypesRejectStaleWrongIPAndReplacedEvidence(t *testing.T) {
	now := time.Now()
	old := IPPureEvidence("8.8.8.8", []string{"商业宽带"}, now.Add(-time.Minute).Format(time.RFC3339Nano))
	for _, extra := range []Evidence{
		{Provider: "ippure", IP: "8.8.8.8", CheckedAt: now.Format(time.RFC3339Nano), Outcome: "unknown"},
		IPPureEvidence("1.1.1.1", []string{"教育IP"}, now.Format(time.RFC3339Nano)),
		IPPureEvidence("8.8.8.8", []string{"教育IP"}, now.Add(-25*time.Hour).Format(time.RFC3339Nano)),
	} {
		input := []Evidence{extra}
		if extra.Outcome == "unknown" && extra.IP == "8.8.8.8" && extra.ASNType == "" {
			input = append(input, old)
		}
		r := Score("8.8.8.8", input, now)
		if len(r.Types) != 0 || !reflect.DeepEqual(r.Attributes([]string{"商业宽带", "性质未知"}), []string{"类型未知"}) {
			t.Fatalf("invalid type evidence reused: %+v", r)
		}
	}
}

func TestIPAPIUsesMobileAndHostingFlagsWithoutGuessingResidential(t *testing.T) {
	for _, tc := range []struct{ raw, usage string }{
		{`{"status":"success","query":"8.8.8.8","hosting":false}`, ""},
		{`{"status":"success","query":"8.8.8.8","hosting":false,"mobile":true}`, "移动网络"},
		{`{"status":"success","query":"8.8.8.8","hosting":true}`, "机房IP"},
	} {
		e := ParseIPAPI([]byte(tc.raw), "8.8.8.8", "now")
		if e.networkTypes().UsageType != tc.usage || e.Outcome == "supports-residential" {
			t.Fatalf("wrong IP-API type: %+v", e)
		}
	}
}

func TestFiveProviderMajorityAndTie(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		points []int
		state  string
	}{
		{[]int{1, 1, -1, -1, -1}, "datacenter"},
		{[]int{1, 1, -1, -1, 0}, "residential"},
		{[]int{1, 1, -1, 1, -1}, "residential"},
	} {
		var input []Evidence
		for i, provider := range []string{"ippure", "ipinfo", "ip-api", "ipregistry", "abuseipdb"} {
			input = append(input, evidence(provider, tc.points[i], now))
		}
		if r := Score("8.8.8.8", input, now); r.State != tc.state {
			t.Fatalf("wrong five-provider decision: %+v", r)
		}
	}
}

func TestSummaryUsesIndependentProviderMajority(t *testing.T) {
	r := Result{Types: []TypeEvidence{
		{Provider: "ippure", UsageType: "住宅IP"},
		{Provider: "ip-api", UsageType: "机房IP"},
		{Provider: "ipregistry", UsageType: "机房IP", CompanyType: "机房IP"},
	}}
	if got := r.Attributes([]string{"住宅IP", "机房IP", "原生IP", "类型待确认"}); !reflect.DeepEqual(got, []string{"原生IP", "机房IP"}) {
		t.Fatalf("incorrect summary: %v", got)
	}
	raw, _ := json.Marshal(r)
	var restored Result
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if got := restored.Attributes(nil); !reflect.DeepEqual(got, []string{"机房IP"}) {
		t.Fatalf("persisted result differs: %v", got)
	}
	r.Types = []TypeEvidence{{Provider: "ippure", UsageType: "住宅IP"}, {Provider: "ippure", UsageType: "住宅IP"}, {Provider: "ip-api", UsageType: "机房IP"}}
	if got := r.Attributes(nil); !reflect.DeepEqual(got, []string{"住宅IP"}) {
		t.Fatalf("duplicate provider counted: %v", got)
	}
}
