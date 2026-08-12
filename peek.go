package main

import "regexp"

// paneIDRe matches a tmux pane id ("%" followed by digits, e.g. "%3"). Pane ids
// are the peek feature's targeting key: they are ASCII, stable within a pane's
// life, and — unlike session names — need no cleaning. Any value that fails this
// pattern is rejected before it reaches a tmux command.
var paneIDRe = regexp.MustCompile(`^%\d+$`)
