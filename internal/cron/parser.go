package cron

import (
	"sync"

	cronpkg "github.com/netresearch/go-cron"
)

// parseCache memoizes parsed schedules. Callers parse the same handful of
// spec expressions over and over — the controller on every reconcile's
// scale-down window check, the simulator on every replayed minute — and
// parsing dominates those hot paths. Parsed schedules are immutable (Next is
// a pure computation), so sharing them across goroutines is safe. Entries are
// never evicted: the key space is the set of cron expressions appearing in
// specs, which is small and stable.
var parseCache sync.Map // string -> cronpkg.Schedule

// Parse parses a cron expression with support for both CRON_TZ format and standard cron format.
// Returns a Schedule that can be used for execution, or an error if the expression is invalid.
// For validation-only use cases, simply ignore the returned Schedule and check the error.
func Parse(cronExpr string) (cronpkg.Schedule, error) {
	if cached, ok := parseCache.Load(cronExpr); ok {
		return cached.(cronpkg.Schedule), nil
	}
	schedule, err := cronpkg.MustNewParser(DefaultOptions).Parse(cronExpr)
	if err != nil {
		// Invalid expressions are not cached: they are rare (a spec typo) and
		// caching them would only grow the map.
		return nil, err
	}
	parseCache.Store(cronExpr, schedule)
	return schedule, nil
}
