package cron

import cronpkg "github.com/netresearch/go-cron"

// NewParser returns a cron parser with extended syntax enabled:
// L (last day of month), L-n, W (nearest weekday), #n (nth weekday), #L (last weekday).
func NewParser() cronpkg.Parser {
	return cronpkg.MustNewParser(
		cronpkg.Minute | cronpkg.Hour | cronpkg.Dom | cronpkg.Month | cronpkg.Dow | cronpkg.Descriptor | cronpkg.Extended,
	)
}
