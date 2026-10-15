package patrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

// ResidentialVerifier checks the current tunnel, never a saved node label.
// Empty IP means unknown, disputed or unsupported; it never authorizes recovery.
type ResidentialVerifier func(context.Context, *http.Client) (string, error)

var (
	ErrResidentialDisputed = errors.New("residential classification disputed")
	ErrResidentialHosting  = errors.New("IP-specific hosting evidence")
)

// NewResidentialVerifier uses the three-provider majority policy. The optional
// IP-API Pro key selects HTTPS; otherwise the user-requested free API is used.
func NewResidentialVerifier(token, origin string, proKey ...string) ResidentialVerifier {
	host := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return newResidentialVerifier(token, origin, host, proKey...)
}

func newResidentialVerifier(token, origin string, host *http.Client, proKey ...string) ResidentialVerifier {
	assess := newResidentialAssessment(token, origin, host, proKey...)
	return func(ctx context.Context, tunnel *http.Client) (string, error) {
		ip, _, err := assess(ctx, tunnel)
		return ip, err
	}
}

type ResidentialAssessment func(context.Context, *http.Client) (string, ipcheck.Result, error)

func NewResidentialAssessment(token, origin, proKey string) ResidentialAssessment {
	host := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return newResidentialAssessment(token, origin, host, proKey)
}

func newResidentialAssessment(token, origin string, host *http.Client, proKey ...string) ResidentialAssessment {
	if origin == "" {
		origin = "https://api.123169.xyz"
	}
	key := ""
	if len(proKey) > 0 {
		key = proKey[0]
	}
	sources := ipcheck.NewClient(token, key)
	sources.HTTP = host
	return func(ctx context.Context, tunnel *http.Client) (string, ipcheck.Result, error) {
		empty := ipcheck.Result{State: "unknown"}
		ip, err := recoveryExitIP(ctx, tunnel)
		if err != nil {
			return "", empty, err
		}
		direct, err := recoveryExitIP(ctx, host)
		if err != nil || direct == ip {
			return "", empty, fmt.Errorf("current exit cannot be distinguished from host egress")
		}
		evidence := sources.Lookup(ctx, ip)
		// A failed IPPure request abstains; the other two may still form a majority.
		verdict, err := collector.QueryIPPure(ctx, collector.NewIPPureClient(tunnel, origin, 10*time.Second), ip, nil)
		if err == nil {
			evidence = append(evidence, ipcheck.IPPureEvidence(ip, verdict.Attributes, time.Now().UTC().Format(time.RFC3339Nano)))
		}
		result := ipcheck.Score(ip, evidence, time.Now())
		switch result.State {
		case "datacenter":
			return "", result, ErrResidentialHosting
		case "disputed":
			return "", result, ErrResidentialDisputed
		case "residential":
			confirmed, err := recoveryExitIP(ctx, tunnel)
			if err != nil || confirmed != ip {
				return "", result, fmt.Errorf("exit changed during classification")
			}
			return ip, result, nil
		default:
			return "", result, nil
		}
	}
}

// Independent HTTPS echoes must agree. A rotated or mixed exit is held for review.
func recoveryExitIP(ctx context.Context, client *http.Client) (string, error) {
	var observed string
	for _, url := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("exit echo unavailable")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		ip := net.ParseIP(strings.TrimSpace(string(raw)))
		if readErr != nil || resp.StatusCode != http.StatusOK || ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return "", fmt.Errorf("invalid exit echo")
		}
		value := ip.String()
		if observed != "" && observed != value {
			return "", fmt.Errorf("exit echoes disagree")
		}
		observed = value
	}
	return observed, nil
}
