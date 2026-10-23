package ipcheck

import (
	"net/http"
	"strconv"
	"time"
)

// quotaHeaders turns IP-API's own accounting into the next slot for the
// endpoint that produced it. X-Rl is the number of requests left in the
// current window and X-Ttl the seconds until it resets, so the remaining
// budget can be spread evenly instead of guessed with a fixed interval.
//
// The two endpoints do not share a ceiling: the free /batch path allows far
// fewer requests per minute than the single-IP path. A single constant tuned
// for the single-IP path therefore asked /batch for roughly twice its
// permitted rate, and every other batch came back 429. Because both endpoints
// published to the same cooldown key, those 429s also blocked the single-IP
// path — the lookups that were supposed to classify an address ended up
// cooling down instead, which is what starved the attribute queue.
func quotaHeaders(h http.Header, floor time.Duration) (time.Duration, bool) {
	if h == nil || h.Get("X-Rl") == "" {
		return 0, false
	}
	left, err := strconv.Atoi(h.Get("X-Rl"))
	if err != nil {
		return 0, false
	}
	ttl, err := strconv.Atoi(h.Get("X-Ttl"))
	if err != nil || ttl <= 0 {
		return 0, false
	}
	window := time.Duration(ttl) * time.Second
	if left <= 0 {
		// Exhausted: nothing may leave until the window resets.
		return window, true
	}
	// Spread what is left so the last request of a window does not arrive at
	// the same instant as the first request of the next one.
	share := window / time.Duration(left+1)
	if share < floor {
		share = floor
	}
	return share, true
}
