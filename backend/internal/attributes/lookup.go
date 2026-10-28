// Package attributes fills missing exit-IP metadata without probing nodes.
package attributes

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

type Candidate struct {
	IP                                 string
	Previous                           domain.IPPureResult
	ClassificationMissing, RiskMissing bool
	CountryMissing                     bool
}
type Result struct {
	Evidence      []ipcheck.Evidence
	Risk          *collector.IPPureResult
	RiskError     string
	RiskAttempted bool
	Country       string
}

func (r Result) hasData(ip string) bool {
	if r.Risk != nil && r.Risk.IP == ip || len(r.Country) == 2 && r.Country[0] >= 'A' && r.Country[0] <= 'Z' && r.Country[1] >= 'A' && r.Country[1] <= 'Z' {
		return true
	}
	for _, e := range r.Evidence {
		if e.IP == ip && e.QueryStatus == "success" && (e.ASN != "" || e.ASNType != "" || e.UsageType != "" || e.CompanyType != "" || e.Hosting != nil) {
			return true
		}
	}
	return false
}
type Lookup interface {
	Lookup(context.Context, Candidate) Result
}
type Client struct {
	Classification *ipcheck.Client
	Risk           *collector.IPPureClient
	Geolocation    *GeolocationClient
	ASN            *collector.ASNCache
	riskMu         sync.Mutex
	riskNext       time.Time
	geoMu          sync.Mutex
	geoNext        time.Time
	BeforeProvider func(context.Context, string) error
	AfterProvider  func(context.Context, string, time.Time) error
}

func NewClient(classification *ipcheck.Client, origin string) *Client {
	return &Client{
		Classification: classification,
		Risk:           collector.NewIPPureClient(&http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origin, 10*time.Second),
		Geolocation:    NewGeolocationClient(&http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}),
		ASN:            collector.NewASNCacheWithTTL(6 * time.Hour),
	}
}
func (c *Client) Lookup(ctx context.Context, n Candidate) Result {
	var r Result
	var wg sync.WaitGroup
	if n.ClassificationMissing {
		wg.Add(1)
		go func() { defer wg.Done(); r.Evidence = c.Classification.Lookup(ctx, n.IP) }()
	}
	if n.RiskMissing {
		// One direct risk request sequence at a time. A refused upstream cools down
		// globally instead of being retried once for every missing address.
		c.riskMu.Lock()
		wait := time.Until(c.riskNext)
		ready := ctx.Err() == nil
		if wait > 10*time.Second {
			ready = false
		} else if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				ready = false
			case <-timer.C:
			}
			timer.Stop()
		}
		if ready && c.BeforeProvider != nil {
			ready = c.BeforeProvider(ctx, "ippure") == nil
		}
		if !ready {
			r.RiskError = "quota-cooldown"
		} else {
			r.RiskAttempted = true
			v, err := collector.QueryIPPure(ctx, c.Risk, n.IP, c.ASN)
			c.riskNext = time.Now().Add(1500 * time.Millisecond)
			if err != nil {
				r.RiskError = collector.IPPureErrorCode(err)
				c.riskNext = time.Now().Add(30 * time.Minute)
			} else {
				r.Risk = v
			}
			if err == nil && c.AfterProvider != nil {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				_ = c.AfterProvider(cleanup, "ippure", c.riskNext)
				stop()
			}
		}
		c.riskMu.Unlock()
	}
	if n.CountryMissing && (r.Risk == nil || r.Risk.Country == "") {
		c.geoMu.Lock()
		wait := time.Until(c.geoNext)
		ready := ctx.Err() == nil
		if wait > 10*time.Second {
			ready = false
		} else if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				ready = false
			case <-timer.C:
			}
			timer.Stop()
		}
		if ready && c.BeforeProvider != nil {
			ready = c.BeforeProvider(ctx, "geolocation") == nil
		}
		if ready {
			country, err := c.Geolocation.Lookup(ctx, n.IP)
			c.geoNext = time.Now().Add(2 * time.Second)
			if err != nil {
				c.geoNext = time.Now().Add(30 * time.Minute)
			} else {
				r.Country = country
			}
			if err == nil && c.AfterProvider != nil {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				_ = c.AfterProvider(cleanup, "geolocation", c.geoNext)
				stop()
			}
		}
		c.geoMu.Unlock()
	}
	wg.Wait()
	return r
}
func merge(n Candidate, r Result, now time.Time) domain.IPPureResult {
	v := n.Previous
	classificationUpdated := false
	v.IP = n.IP
	stamp := now.UTC().Format(time.RFC3339Nano)
	if v.Status == "" {
		v.Status = "error"
		v.Error = "IP 风险属性待查询"
	}
	if r.Risk != nil && r.Risk.IP == n.IP {
		a := r.Risk
		v.Status = "success"
		v.Error = ""
		v.FailureStage = ""
		if a.ASN != "" {
			v.ASN = a.ASN
		}
		if a.Organization != "" {
			v.Organization = a.Organization
		}
		if a.Source != "" {
			v.Source = a.Source
		}
		if v.Country == "" {
			v.Country = a.Country
		}
		v.Attributes = append(append([]string{}, v.Attributes...), a.Attributes...)
		v.Score, v.Label = a.Score, a.Label
		if a.HumanPercent != nil && a.BotPercent != nil {
			v.HumanPercent, v.BotPercent = a.HumanPercent, a.BotPercent
			v.TrafficRatioStatus, v.TrafficRatioError = a.TrafficRatioStatus, a.TrafficRatioError
			v.TrafficRatioCheckedAt = stamp
		}
		v.CheckedAt, v.RiskQueryCheckedAt = stamp, stamp
		v.RiskQueryAttemptAt, v.RiskQueryStatus, v.RiskQueryError, v.QueryMode = stamp, "success", "", "direct"
		e := ipcheck.IPPureEvidence(n.IP, a.Attributes, stamp)
		e.QueryStatus = "success"
		r.Evidence = append(r.Evidence, e)
	}
	if v.Country == "" && len(r.Country) == 2 && r.Country[0] >= 'A' && r.Country[0] <= 'Z' && r.Country[1] >= 'A' && r.Country[1] <= 'Z' {
		v.Country = r.Country
	}
	if n.ClassificationMissing {
		var evidence []ipcheck.Evidence
		for _, e := range r.Evidence {
			if e.QueryStatus == "success" && e.IP == n.IP {
				evidence = append(evidence, e)
			}
		}
		if len(evidence) > 0 {
			if v.Classification != nil {
				evidence = append(append([]ipcheck.Evidence{}, v.Classification.Evidence...), evidence...)
			}
			votes := ipcheck.Score(n.IP, evidence, now)
			if len(votes.Types) > 0 {
				v.Classification = &votes
				classificationUpdated = true
			}
		}
	}
	if v.Classification != nil && len(v.Classification.Types) > 0 && (classificationUpdated || r.Risk != nil && r.Risk.IP == n.IP) {
		v.Attributes = v.Classification.Attributes(v.Attributes)
	}
	if ip, err := netip.ParseAddr(n.IP); err == nil && ip.Is6() {
		v.AddressFamily = 6
		if v.Status != "success" {
			v.Status = "unsupported"
			v.FailureStage = "unsupported-ipv6"
			v.Error = "IPv6 风险评分暂不支持"
		}
	}
	return v
}
