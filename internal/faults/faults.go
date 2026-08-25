package faults

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"sync/atomic"
)

// atomic because /admin/fault writes while handlers read concurrently
var (
	errorRate atomic.Int64 // percent, 0-100
	slowRate  atomic.Int64 // percent, 0-100
	n1        atomic.Bool
	noIndex   atomic.Bool
)

func ErrorRate() int { return int(errorRate.Load()) }
func SlowRate() int  { return int(slowRate.Load()) }
func NPlusOne() bool { return n1.Load() }
func NoIndex() bool  { return noIndex.Load() }

// Hit returns true for `percent` of calls.
func Hit(percent int) bool { //
	return percent > 0 && rand.Intn(100) < percent
}

// if faults.Hit(10) { 10 pr cent of the time, do something
// execute fault
// }

// POST /admin/fault?errors=10&slow=5&n1=true
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if v := q.Get("errors"); v != "" {
			var n int64
			json.Unmarshal([]byte(v), &n)
			errorRate.Store(n)
		}
		if v := q.Get("slow"); v != "" {
			var n int64
			json.Unmarshal([]byte(v), &n)
			slowRate.Store(n)
		}
		if v := q.Get("n1"); v != "" {
			n1.Store(v == "true" || v == "1")
		}
		if v := q.Get("noindex"); v != "" {
			noIndex.Store(v == "true" || v == "1")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"errors": errorRate.Load(), "slow": slowRate.Load(),
			"n1": n1.Load(), "noindex": noIndex.Load(),
		})
	}
}
