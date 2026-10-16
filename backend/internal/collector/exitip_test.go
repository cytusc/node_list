package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestASNCacheCoalescesConcurrentLoadsAndExpires(t *testing.T) {
	cache := NewASNCacheWithTTL(10 * time.Millisecond)
	var calls int
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := cache.Load(context.Background(), 3462, func(context.Context) (map[string]any, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				return map[string]any{"human": float64(74)}, nil
			})
			if err != nil || value["human"] != float64(74) {
				t.Errorf("load = %v, %v", value, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("concurrent loads = %d, want 1", calls)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := cache.Load(context.Background(), 3462, func(context.Context) (map[string]any, error) {
		calls++
		return map[string]any{"human": float64(75)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("loads after expiry = %d, want 2", calls)
	}
}

func TestQueryIPPureFetchesStatisticsWithoutCache(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cache      *ASNCache
		botPayload string
		botStatus  int
		wantStatus string
		wantCalls  int
	}{
		{"without cache", nil, `{"ok":true,"data":{"human":74.0479,"bot":25.9521}}`, 0, "available", 2},
		{"shared cache", NewASNCache(), `{"ok":true,"data":{"human":74.0479,"bot":25.9521}}`, 0, "available", 1},
		{"not found means no statistics", NewASNCache(), "", http.StatusNotFound, "no-data", 1},
		{"other http error", nil, "", http.StatusUnauthorized, "query-failed", 2},
		{"empty successful response", nil, `{"ok":true,"data":{}}`, 0, "no-data", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			botCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/info/ip-basic/118.168.211.221":
					fmt.Fprint(w, `{"ok":true,"data":{"ip":"118.168.211.221","asn":{"number":3462,"organization":"Data Communication Business Group","type":"isp","is_residential":true},"traits":{"is_broadcast":false}}}`)
				case "/api/info/ip-risk/118.168.211.221":
					fmt.Fprint(w, `{"ok":true,"data":{"risk_score":3}}`)
				case "/api/info/asn/botclass/3462":
					botCalls++
					if tc.botPayload == "" {
						http.Error(w, "unavailable", tc.botStatus)
						return
					}
					fmt.Fprint(w, tc.botPayload)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := NewIPPureClient(server.Client(), server.URL, time.Second)
			for range 2 {
				v, err := QueryIPPure(context.Background(), client, "118.168.211.221", tc.cache)
				if err != nil {
					t.Fatal(err)
				}
				if v.Score != 3 || v.Label != "极度纯净" || v.Source != "原生IP" || len(v.Attributes) != 1 || v.Attributes[0] != "住宅IP" {
					t.Fatalf("unexpected verdict: %+v", v)
				}
				if tc.wantStatus == "available" {
					if v.HumanPercent == nil || v.BotPercent == nil || *v.HumanPercent != 74.05 || *v.BotPercent != 25.95 {
						t.Fatalf("unexpected statistics: %+v", v)
					}
				} else if v.HumanPercent != nil || v.BotPercent != nil {
					t.Fatalf("expected absent statistics: %+v", v)
				}
				if v.TrafficRatioStatus != tc.wantStatus {
					t.Fatalf("traffic ratio status = %q, want %q", v.TrafficRatioStatus, tc.wantStatus)
				}
			}
			if botCalls != tc.wantCalls {
				t.Fatalf("statistics requests = %d, want %d", botCalls, tc.wantCalls)
			}
		})
	}
}

func TestBusinessASNDoesNotBecomeDatacenter(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"business", "商业宽带"}, {"hosting", "机房IP"}, {"isp", "住宅IP"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			basic := map[string]any{
				"ip":     "8.8.8.8",
				"asn":    map[string]any{"number": float64(15169), "organization": "Example", "type": tc.kind},
				"traits": map[string]any{"is_broadcast": false},
			}
			v, err := NormalizeIPPure("8.8.8.8", basic, map[string]any{"risk_score": float64(10)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Attributes) != 1 || v.Attributes[0] != tc.want {
				t.Fatalf("got %v", v.Attributes)
			}
		})
	}
}
