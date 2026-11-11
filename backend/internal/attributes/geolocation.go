package attributes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// GeolocationClient provides a country-code fallback when IPPure has no
// location data. The caller owns pacing and persistent provider cooldowns.
type GeolocationClient struct {
	HTTP *http.Client
}

func NewGeolocationClient(client *http.Client) *GeolocationClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &GeolocationClient{HTTP: client}
}

func (c *GeolocationClient) Lookup(ctx context.Context, target string) (string, error) {
	ip, err := netip.ParseAddr(target)
	if err != nil || ip.Zone() != "" {
		return "", errors.New("invalid IP address")
	}
	target = ip.Unmap().String()
	if country, err := c.lookupIPWho(ctx, target, ip); err == nil {
		return country, nil
	}
	return c.lookupIPAPI(ctx, target, ip)
}

func (c *GeolocationClient) lookupIPWho(ctx context.Context, target string, ip netip.Addr) (string, error) {
	endpoint := "https://ipwho.is/" + url.PathEscape(target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", errors.New("geolocation provider returned non-200 status")
	}
	var body struct {
		Success     bool   `json:"success"`
		IP          string `json:"ip"`
		CountryCode string `json:"country_code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&body); err != nil {
		return "", errors.New("invalid geolocation response")
	}
	returnedIP, err := netip.ParseAddr(body.IP)
	if err != nil || returnedIP.Unmap() != ip || !body.Success {
		return "", errors.New("geolocation response did not match queried IP")
	}
	code := strings.ToUpper(strings.TrimSpace(body.CountryCode))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "", errors.New("geolocation response has no country code")
	}
	return code, nil
}

// IP-API is a fallback for deployments where ipwho.is has exhausted its shared
// free quota. Its response is accepted only when it echoes the requested IP.
func (c *GeolocationClient) lookupIPAPI(ctx context.Context, target string, ip netip.Addr) (string, error) {
	endpoint := "http://ip-api.com/json/" + url.PathEscape(target) + "?fields=status,query,countryCode"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", errors.New("fallback geolocation provider returned non-200 status")
	}
	var body struct {
		Status      string `json:"status"`
		IP          string `json:"query"`
		CountryCode string `json:"countryCode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&body); err != nil {
		return "", errors.New("invalid fallback geolocation response")
	}
	returnedIP, err := netip.ParseAddr(body.IP)
	if err != nil || returnedIP.Unmap() != ip || body.Status != "success" {
		return "", errors.New("fallback geolocation response did not match queried IP")
	}
	code := strings.ToUpper(strings.TrimSpace(body.CountryCode))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "", errors.New("fallback geolocation response has no country code")
	}
	return code, nil
}
