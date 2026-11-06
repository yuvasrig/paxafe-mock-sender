package web

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/yuvasrig/paxafe-mock-sender/internal/runner"
)

// Charts are drawn as inline SVG on the server: no scripts, no libraries, and the same picture in the run report and
// in a saved copy of the page. Sent values are the grey line; what the API stored is drawn in blue on top of it, so a
// match shows blue dots sitting on the grey line, and a difference or a dropped value is visible at a glance.

const (
	chartW, chartH = 640, 240
	padL, padR     = 52, 14
	padT, padB     = 14, 28
)

// compared returns the results that have both a sent sample and a verdict, in the order they were sent.
func compared(results []runner.Result) []runner.Result {
	var out []runner.Result
	for _, r := range results {
		if r.Sent != nil && r.Compare != "" {
			out = append(out, r)
		}
	}
	return out
}

type axis struct{ lo, hi float64 }

func newAxis(vals []float64) axis {
	if len(vals) == 0 {
		return axis{0, 1}
	}
	a := axis{vals[0], vals[0]}
	for _, v := range vals {
		if v < a.lo {
			a.lo = v
		}
		if v > a.hi {
			a.hi = v
		}
	}
	if a.hi-a.lo < 1e-9 { // a flat series still needs a visible range
		a.lo, a.hi = a.lo-1, a.hi+1
	}
	pad := (a.hi - a.lo) * 0.08
	a.lo, a.hi = a.lo-pad, a.hi+pad
	return a
}

func (a axis) pos(v, from, to float64) float64 { return from + (v-a.lo)/(a.hi-a.lo)*(to-from) }

// TempChart plots the temperature we sent against the temperature the API stored, event by event.
func TempChart(results []runner.Result) template.HTML {
	rows := compared(results)
	var vals []float64
	for _, r := range rows {
		if r.Sent.Temp != nil {
			vals = append(vals, *r.Sent.Temp)
		}
		if r.Got != nil && r.Got.Temp != nil {
			vals = append(vals, *r.Got.Temp)
		}
	}
	if len(vals) == 0 {
		return ""
	}
	y := newAxis(vals)
	n := len(rows)
	xOf := func(i int) float64 {
		if n == 1 {
			return (padL + chartW - padR) / 2
		}
		return padL + float64(i)/float64(n-1)*(chartW-padL-padR)
	}
	yOf := func(v float64) float64 { return y.pos(v, chartH-padB, padT) }

	var b strings.Builder
	svgOpen(&b, "Temperature sent and stored, per event")
	frame(&b, y, "°C", fmt.Sprintf("event 1 to %d (in the order sent)", n))
	var line []string
	for i, r := range rows {
		if r.Sent.Temp != nil {
			line = append(line, fmt.Sprintf("%.1f,%.1f", xOf(i), yOf(*r.Sent.Temp)))
		}
	}
	if len(line) > 1 {
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#9aa4b2" stroke-width="2"/>`, strings.Join(line, " "))
	}
	for i, r := range rows {
		switch {
		case r.Sent.Temp == nil:
		case r.Got != nil && r.Got.Temp != nil:
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="#2563eb"><title>#%d sent %.3f, stored %.2f</title></circle>`, xOf(i), yOf(*r.Got.Temp), r.Index+1, *r.Sent.Temp, *r.Got.Temp)
		default: // sent, but nothing stored: a red ring on the sent line
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="none" stroke="#dc2626" stroke-width="2"><title>#%d sent %.3f, not stored (%s)</title></circle>`, xOf(i), yOf(*r.Sent.Temp), r.Index+1, *r.Sent.Temp, r.Compare)
		}
	}
	legend(&b)
	b.WriteString("</svg>")
	return template.HTML(b.String()) //nolint:gosec // built only from numbers and fixed text
}

// PathChart plots the positions we sent (a grey line) and the positions the API stored (blue dots): longitude across,
// latitude up. Each axis is scaled to its own range, so a short route still fills the chart.
func PathChart(results []runner.Result) template.HTML {
	rows := compared(results)
	var lons, lats []float64
	for _, r := range rows {
		if r.Sent.Lat != nil && r.Sent.Lon != nil {
			lats, lons = append(lats, *r.Sent.Lat), append(lons, *r.Sent.Lon)
		}
		if r.Got != nil && r.Got.Lat != nil && r.Got.Lon != nil {
			lats, lons = append(lats, *r.Got.Lat), append(lons, *r.Got.Lon)
		}
	}
	if len(lats) == 0 {
		return ""
	}
	x, y := newAxis(lons), newAxis(lats)
	xOf := func(v float64) float64 { return x.pos(v, padL, chartW-padR) }
	yOf := func(v float64) float64 { return y.pos(v, chartH-padB, padT) }

	var b strings.Builder
	svgOpen(&b, "Route sent and stored, latitude against longitude")
	frame(&b, y, "lat", "longitude "+fmt.Sprintf("%.4f to %.4f", x.lo, x.hi))
	var line []string
	for _, r := range rows {
		if r.Sent.Lat != nil && r.Sent.Lon != nil {
			line = append(line, fmt.Sprintf("%.1f,%.1f", xOf(*r.Sent.Lon), yOf(*r.Sent.Lat)))
		}
	}
	if len(line) > 1 {
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#9aa4b2" stroke-width="2"/>`, strings.Join(line, " "))
	}
	for _, r := range rows {
		switch {
		case r.Sent.Lat == nil || r.Sent.Lon == nil:
		case r.Got != nil && r.Got.Lat != nil && r.Got.Lon != nil:
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="#2563eb"><title>#%d stored %.6f, %.6f</title></circle>`, xOf(*r.Got.Lon), yOf(*r.Got.Lat), r.Index+1, *r.Got.Lat, *r.Got.Lon)
		default:
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="none" stroke="#dc2626" stroke-width="2"><title>#%d sent %.6f, %.6f, not stored (%s)</title></circle>`, xOf(*r.Sent.Lon), yOf(*r.Sent.Lat), r.Index+1, *r.Sent.Lat, *r.Sent.Lon, r.Compare)
		}
	}
	legend(&b)
	b.WriteString("</svg>")
	return template.HTML(b.String()) //nolint:gosec // built only from numbers and fixed text
}

func svgOpen(b *strings.Builder, title string) {
	fmt.Fprintf(b, `<svg viewBox="0 0 %d %d" role="img" aria-label="%s" style="width:100%%;max-width:%dpx;height:auto;background:var(--card,#fff);border:1px solid var(--line,#e1e5ea);border-radius:8px"><title>%s</title>`, chartW, chartH, title, chartW, title)
}

// frame draws the plot area's border, three horizontal grid lines with value labels, and the x caption.
func frame(b *strings.Builder, y axis, unit, xcap string) {
	for i := 0; i <= 2; i++ {
		v := y.lo + (y.hi-y.lo)*float64(i)/2
		py := y.pos(v, chartH-padB, padT)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#e1e5ea"/>`, padL, py, chartW-padR, py)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" font-size="10" text-anchor="end" fill="#667085">%.2f</text>`, padL-4, py+3, v)
	}
	fmt.Fprintf(b, `<text x="%d" y="%d" font-size="10" fill="#667085">%s</text>`, padL, chartH-8, xcap)
	fmt.Fprintf(b, `<text x="4" y="%d" font-size="10" fill="#667085">%s</text>`, padT+4, unit)
}

func legend(b *strings.Builder) {
	fmt.Fprintf(b, `<g font-size="10" fill="#667085"><line x1="%d" y1="10" x2="%d" y2="10" stroke="#9aa4b2" stroke-width="2"/><text x="%d" y="13">sent</text>`+
		`<circle cx="%d" cy="10" r="3.5" fill="#2563eb"/><text x="%d" y="13">stored by the API</text>`+
		`<circle cx="%d" cy="10" r="4.5" fill="none" stroke="#dc2626" stroke-width="2"/><text x="%d" y="13">sent, not stored</text></g>`,
		chartW-300, chartW-280, chartW-276, chartW-236, chartW-228, chartW-130, chartW-120)
}
