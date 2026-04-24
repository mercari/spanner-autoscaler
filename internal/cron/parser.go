package cron

import (
	cronpkg "github.com/robfig/cron/v3"
)

// Parse parses a cron expression with support for both CRON_TZ format and standard cron format.
// Returns a Schedule that can be used for execution, or an error if the expression is invalid.
// For validation-only use cases, simply ignore the returned Schedule and check the error.
func Parse(cronExpr string) (cronpkg.Schedule, error) {
	return cronpkg.ParseStandard(cronExpr)
}