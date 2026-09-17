package cron

import (
	"sync"

	cronpkg "github.com/netresearch/go-cron"
)

// parseCache memoizes parsed schedules. Callers parse the same handful of
// spec expressions over and over — the controller on every reconcile's
// scale-down window check, the simulator on every replayed minute — and
// parsing dominates those hot paths. Parsed schedules are immutable (Next is
// a pure computation), so sharing them across goroutines is safe.
//
// The keys come from CR specs, so the cache is bounded: churning specs must
// not grow the controller's memory without limit. The working set is a
// handful of expressions, so on overflow the whole cache is dropped rather
// than tracking recency.
const parseCacheMaxEntries = 1024

var (
	parseCacheMu sync.RWMutex
	parseCache   = make(map[string]cronpkg.Schedule)
)

// Parse parses a cron expression with support for both CRON_TZ format and standard cron format.
// Returns a Schedule that can be used for execution, or an error if the expression is invalid.
// For validation-only use cases, simply ignore the returned Schedule and check the error.
func Parse(cronExpr string) (cronpkg.Schedule, error) {
	parseCacheMu.RLock()
	cached, ok := parseCache[cronExpr]
	parseCacheMu.RUnlock()
	if ok {
		return cached, nil
	}
	schedule, err := cronpkg.MustNewParser(DefaultOptions).Parse(cronExpr)
	if err != nil {
		// Invalid expressions are not cached: they are rare (a spec typo) and
		// caching them would only grow the map.
		return nil, err
	}
	parseCacheMu.Lock()
	if len(parseCache) >= parseCacheMaxEntries {
		clear(parseCache)
	}
	parseCache[cronExpr] = schedule
	parseCacheMu.Unlock()
	return schedule, nil
}
