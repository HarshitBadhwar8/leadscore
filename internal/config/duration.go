// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package config

import (
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/duration"
)

// ParseDuration reads a Go duration plus `d` for days:
// "90d", "1d12h", "15m". It is duration.Parse, kept here for config's callers.
func ParseDuration(s string) (time.Duration, error) { return duration.Parse(s) }
