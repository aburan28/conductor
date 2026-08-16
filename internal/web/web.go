// Package web serves the Conductor dashboard.
//
// The dashboard is a single self-contained HTML file with no build step and no external
// requests. That is partly convenience — `conductord` is one binary — and partly the point:
// a coordination dashboard that phones out to a CDN is a coordination dashboard that leaks
// which repository your team is working on.
package web

import _ "embed"

//go:embed dashboard.html
var dashboardHTML []byte

// Dashboard returns the embedded page.
func Dashboard() []byte { return dashboardHTML }
