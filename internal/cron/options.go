package cron

import cronpkg "github.com/netresearch/go-cron"

// DefaultOptions enables standard 5-field cron syntax plus extended syntax
// (L, L-n, W, #n, #L) and descriptors (@hourly etc.).
const DefaultOptions = cronpkg.Minute | cronpkg.Hour | cronpkg.Dom | cronpkg.Month | cronpkg.Dow | cronpkg.Descriptor | cronpkg.Extended
