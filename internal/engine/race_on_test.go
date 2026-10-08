// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

//go:build race

package engine

// raceOn: the race detector slows every run several times over, so timing
// bounds do not hold under it.
const raceOn = true
