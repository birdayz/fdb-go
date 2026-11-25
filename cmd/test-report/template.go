package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
)

func renderReportHTML(output io.Writer, data templateData) error {
	tmpl, err := template.New("report").Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	_, err = output.Write(buf.Bytes())
	return err
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Test Report</title>
<style>
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  font-size: 14px;
  background: #f5f6fa;
  color: #2c3e50;
  min-height: 100vh;
}

header {
  background: #1a1f2e;
  color: #ecf0f1;
  padding: 20px 32px;
  display: flex;
  align-items: baseline;
  gap: 16px;
}
header h1 { font-size: 22px; font-weight: 600; letter-spacing: 0.03em; }
header .timestamp { font-size: 12px; color: #8fa3b1; }

.summary-bar {
  background: #fff;
  border-bottom: 1px solid #dde1ec;
  padding: 14px 32px;
  display: flex;
  gap: 28px;
  align-items: center;
  flex-wrap: wrap;
}
.summary-bar .stat { display: flex; flex-direction: column; align-items: center; min-width: 70px; }
.summary-bar .stat .val { font-size: 26px; font-weight: 700; line-height: 1.1; }
.summary-bar .stat .lbl { font-size: 11px; text-transform: uppercase; letter-spacing: 0.07em; color: #8fa3b1; margin-top: 2px; }
.stat-total .val  { color: #2c3e50; }
.stat-pass  .val  { color: #27ae60; }
.stat-fail  .val  { color: #e74c3c; }
.stat-skip  .val  { color: #f39c12; }
.stat-time  .val  { color: #2980b9; font-size: 20px; }
.summary-divider { width: 1px; height: 40px; background: #dde1ec; }

.targets { padding: 24px 32px; display: flex; flex-direction: column; gap: 12px; }

details {
  background: #fff;
  border: 1px solid #dde1ec;
  border-radius: 6px;
  overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04);
}
details.has-fail { border-left: 4px solid #e74c3c; }
details.has-pass { border-left: 4px solid #27ae60; }

summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  cursor: pointer;
  user-select: none;
  list-style: none;
  gap: 12px;
}
summary::-webkit-details-marker { display: none; }
summary::marker { display: none; }
summary:hover { background: #f8f9ff; }

.target-label {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 13px;
  font-weight: 500;
  color: #2c3e50;
  flex: 1;
  word-break: break-all;
}

.chevron {
  font-size: 11px;
  color: #8fa3b1;
  transition: transform 0.15s ease;
  flex-shrink: 0;
}
details[open] .chevron { transform: rotate(90deg); }

.badge {
  display: inline-flex;
  align-items: center;
  padding: 3px 10px;
  border-radius: 12px;
  font-size: 12px;
  font-weight: 600;
  flex-shrink: 0;
}
.badge-pass { background: #eafaf1; color: #1e8449; }
.badge-fail { background: #fdedec; color: #c0392b; }
.badge-cached { background: #eef0f5; color: #8fa3b1; font-weight: 400; }

table {
  width: 100%;
  border-collapse: collapse;
}
th {
  background: #f8f9ff;
  padding: 8px 16px;
  text-align: left;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: #8fa3b1;
  border-bottom: 1px solid #dde1ec;
  font-weight: 600;
}
td {
  padding: 7px 16px;
  border-bottom: 1px solid #f0f1f7;
  vertical-align: middle;
}
tr:last-child td { border-bottom: none; }
tr.row-fail td { background: #fff8f8; }
tr.row-skip td { background: #fffdf5; color: #7f8c8d; }
tr:hover td { background: #f8f9ff; }

.col-name { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; word-break: break-all; }
.col-status { text-align: center; width: 60px; }
.col-duration { text-align: right; width: 90px; font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; color: #7f8c8d; }

.icon { font-size: 15px; font-weight: 700; }
.icon.pass { color: #27ae60; }
.icon.fail { color: #e74c3c; }
.icon.skip { color: #f39c12; }

/* Tree styles for Ginkgo hierarchical specs */
.tree-container { padding: 8px 0; }
.tree-node { padding-left: 20px; }
.tree-node details {
  border: none;
  border-radius: 0;
  box-shadow: none;
  border-left: 2px solid #dde1ec;
  margin: 2px 0;
}
.tree-node details.has-fail { border-left: 2px solid #e74c3c; }
.tree-node details.has-pass { border-left: 2px solid #27ae60; }
.tree-node summary { padding: 6px 12px; }
.tree-node .container-name {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 13px;
  font-weight: 500;
  color: #2c3e50;
  flex: 1;
}
.tree-leaf {
  display: flex;
  align-items: center;
  padding: 4px 12px;
  gap: 8px;
  border-bottom: 1px solid #f0f1f7;
}
.tree-leaf:last-child { border-bottom: none; }
.tree-leaf.row-fail { background: #fff8f8; }
.tree-leaf.row-skip { background: #fffdf5; color: #7f8c8d; }
.tree-leaf .col-name { flex: 1; }
.tree-leaf .col-status { width: 40px; text-align: center; flex-shrink: 0; }
.tree-leaf .col-duration { width: 80px; text-align: right; flex-shrink: 0; font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; color: #7f8c8d; }

/* Coverage section */
.coverage-section {
  margin: 0 32px;
  background: #fff;
  border: 1px solid #dde1ec;
  border-radius: 6px;
  overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04);
}
.coverage-section summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  cursor: pointer;
  user-select: none;
  list-style: none;
  gap: 12px;
}
.coverage-section summary::-webkit-details-marker { display: none; }
.coverage-section summary::marker { display: none; }
.coverage-section summary:hover { background: #f8f9ff; }
.coverage-section .section-title {
  font-size: 14px;
  font-weight: 600;
  color: #2c3e50;
  flex: 1;
}
.coverage-bar-cell {
  width: 200px;
}
.coverage-bar {
  height: 10px;
  background: #ecf0f1;
  border-radius: 5px;
  overflow: hidden;
}
.coverage-bar-fill {
  height: 100%;
  border-radius: 5px;
  background: #27ae60;
}
.coverage-table { width: 100%; border-collapse: collapse; }
.coverage-table th {
  background: #f8f9ff;
  padding: 8px 16px;
  text-align: left;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: #8fa3b1;
  border-bottom: 1px solid #dde1ec;
  font-weight: 600;
}
.coverage-table td {
  padding: 7px 16px;
  border-bottom: 1px solid #f0f1f7;
  vertical-align: middle;
}
.coverage-table tr:last-child td { border-bottom: none; }
.coverage-table .col-pkg {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
}
.coverage-table .col-num {
  text-align: right;
  width: 80px;
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
  color: #7f8c8d;
}
.coverage-table .col-pct {
  text-align: right;
  width: 80px;
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
  font-weight: 600;
}
.coverage-table .col-bar { width: 200px; }

footer {
  padding: 20px 32px;
  text-align: center;
  color: #8fa3b1;
  font-size: 11px;
  border-top: 1px solid #dde1ec;
  background: #fff;
  margin-top: 12px;
}
</style>
</head>
<body>

<header>
  <h1>Test Report</h1>
  <span class="timestamp">{{.GeneratedAt}}</span>
</header>

<section style="padding: 12px 32px; background: #fff; border-bottom: 1px solid #dde1ec">
{{- if .Provenance.SHA}}
  <div>Tested commit: <code>{{.Provenance.SHA}}</code></div>
  <div>Lane: <strong>{{.Provenance.Lane}}</strong> &middot; <a href="{{.Provenance.RunURL}}">CI run</a></div>
  <div>Excluded / separate coverage: {{.Provenance.Exclusions}}</div>
{{- else}}
  <div>Unattributed report: commit, CI run and lane were not supplied.</div>
{{- end}}
  <div>Counts cover this report only. Cached results are not fresh test executions.</div>
  <div>Final run/shard results: {{.Summary.ExecutedResults}} executed, {{.Summary.CachedResults}} cached. These are not case counts.</div>
</section>

<div class="summary-bar">
  <div class="stat stat-total"><span class="val">{{.Summary.Total}}</span><span class="lbl">Total</span></div>
  <div class="summary-divider"></div>
  <div class="stat stat-pass"><span class="val">{{.Summary.Passed}}</span><span class="lbl">Passed</span></div>
  <div class="stat stat-fail"><span class="val">{{.Summary.Failed}}</span><span class="lbl">Failed</span></div>
  <div class="stat stat-skip"><span class="val">{{.Summary.Skipped}}</span><span class="lbl">Skipped</span></div>
  <div class="summary-divider"></div>
  <div class="stat stat-time"><span class="val">{{.Summary.Time}}</span><span class="lbl">Total Time</span></div>
{{- if .Coverage}}
  <div class="summary-divider"></div>
  <div class="stat"><span class="val" style="color: {{.Coverage.CoverageColor}}">{{.Coverage.PercentStr}}</span><span class="lbl">Coverage</span></div>
{{- end}}
</div>

{{- if .Coverage}}
<div style="padding: 24px 32px 0 32px;">
<details class="coverage-section">
  <summary>
    <span class="section-title">Coverage by Package</span>
    <span class="badge badge-pass" style="background: {{.Coverage.CoverageColor}}22; color: {{.Coverage.CoverageColor}}">{{.Coverage.TotalHit}}/{{.Coverage.TotalLines}} lines &mdash; {{.Coverage.PercentStr}}</span>
    <span class="chevron">&#9654;</span>
  </summary>
  <table class="coverage-table">
    <thead>
      <tr>
        <th>Package</th>
        <th style="text-align:right">Lines</th>
        <th style="text-align:right">Hit</th>
        <th style="text-align:right">Coverage</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
    {{- range .Coverage.Packages}}
      <tr>
        <td class="col-pkg">{{.Package}}</td>
        <td class="col-num">{{.Lines}}</td>
        <td class="col-num">{{.LinesHit}}</td>
        <td class="col-pct">{{.PercentStr}}</td>
        <td class="col-bar"><div class="coverage-bar"><div class="coverage-bar-fill" style="width: {{.BarWidth}}"></div></div></td>
      </tr>
    {{- end}}
    </tbody>
  </table>
</details>
</div>
{{- end}}

<div class="targets">
{{- range .Targets}}
<details {{if .HasFailures}}class="has-fail" open{{else}}class="has-pass"{{end}}>
  <summary>
    <span class="target-label">{{.Target}}</span>
    <span class="badge {{.BadgeClass}}">{{.BadgeText}}</span>
    <span class="badge badge-cached">{{.CachedLabel}}: {{.ExecutedResults}} executed, {{.CachedResults}} cached run/shard results</span>
    <span class="chevron">&#9654;</span>
  </summary>
  {{- if .Tree}}
  <div class="tree-container">
    {{- range .Tree.Children}}
    {{template "treenode" .}}
    {{- end}}
  </div>
  {{- else if .Tests}}
  <table>
    <thead>
      <tr><th>Test</th><th style="text-align:center">Status</th><th style="text-align:right">Duration</th></tr>
    </thead>
    <tbody>
    {{- range .SortedTests}}
      <tr class="{{.RowClass}}">
        <td class="col-name">{{.Name}}</td>
        <td class="col-status">{{.StatusIcon}}</td>
        <td class="col-duration">{{.DurationStr}}</td>
      </tr>
    {{- end}}
    </tbody>
  </table>
  {{- end}}
</details>
{{- end}}
</div>

<footer>Generated by bazel-test-report &middot; {{.GeneratedAt}}</footer>

</body>
</html>

{{define "treenode"}}
{{- if .Leaf}}
<div class="tree-leaf {{.Leaf.RowClass}}">
  <span class="col-name">{{.Leaf.Name}}</span>
  <span class="col-status">{{.Leaf.StatusIcon}}</span>
  <span class="col-duration">{{.Leaf.DurationStr}}</span>
</div>
{{- else}}
<div class="tree-node">
  <details {{if .TreeHasFailures}}class="has-fail" open{{else}}class="has-pass"{{end}}>
    <summary>
      <span class="container-name">{{.Name}}</span>
      <span class="badge {{.TreeBadgeClass}}">{{.TreeBadgeText}}</span>
      <span class="chevron">&#9654;</span>
    </summary>
    {{- range .Children}}
    {{template "treenode" .}}
    {{- end}}
  </details>
</div>
{{- end}}
{{end}}
`
