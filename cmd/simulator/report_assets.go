/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	_ "embed"
	"html/template"
)

// The report's markup, styles, and tooltip script live under assets/ as
// regular files (so prose linters and editors treat them as HTML/CSS/JS);
// the go:embed directives compile them into the binary, keeping the
// generated report a single self-contained HTML file with no runtime file
// dependencies.
//
// reportCSS defines the palette slots as CSS custom properties for both
// modes; the chart markup only ever references roles (var(--series-1), …).
// reportJS drives the crosshair tooltip on line charts and the per-mark
// tooltip on scatter dots, inserting all data-derived strings with
// textContent only.

//go:embed assets/report.tmpl
var reportTemplateText string

//go:embed assets/report.css
var reportCSSText string

//go:embed assets/report.js
var reportJSText string

var reportTemplates = template.Must(template.New("report").Parse(reportTemplateText))

//nolint:gosec // G203: static assets compiled into the binary, not user input
var (
	reportCSS = template.CSS(reportCSSText)
	reportJS  = template.JS(reportJSText)
)
