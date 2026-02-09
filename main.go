package main

import (
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const appVersion = "0.1.11"

// Web form defaults; URL query only includes params that differ from these.
const (
	webDefaultStart       = "18:30"
	webDefaultLength      = "4"
	webDefaultNormalStart = "09:00"
	webDefaultNormalEnd   = "17:30"
	webDefaultMinRest     = "11"
	webDefaultMaxOvertime = "4"
)

type Scenario struct {
	Title string

	WorkHours     string // Start -> End (regular)
	ReleaseWindow string // Start -> End (release)
	TotalWork     string // Start -> End (regular + overtime)

	ReleaseIncluded string // e.g. 4h00m
	Overtime        string // e.g. 0h00m

	NextDayHours string // Start -> End (normal window length)
}

type CalcResult struct {
	ReleaseStart string
	ReleaseEnd   string
	ReleaseLen   string

	FullDay string

	NormalStart string
	NormalEnd   string
	NormalLen   string

	MinRest     string
	MaxOvertime string

	Scenarios []Scenario
}

type PageData struct {
	Start   string
	Length  string
	Combine string

	NormalStart string
	NormalEnd   string
	MinRest     string
	MaxOvertime string

	// Full is shown but derived unless explicitly overridden via CLI.
	Full string

	Version string

	Error  string
	Result *CalcResult

	// Share text: meta description when Result is set (for link previews).
	ShareDescription string
}

func main() {
	var (
		startStr string
		lengthH  float64
		combineH float64
		fullH    float64
		port     int

		normalStartStr string
		normalEndStr   string
		minRestH       float64
		maxOvertimeH   float64
	)

	cmd := &cobra.Command{
		Use:   "nightrelcalc",
		Short: "Night release calculator (CLI or web)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, _ := cmd.Flags().GetBool("version"); ok {
				fmt.Printf("nightrelcalc v%s\n", appVersion)
				return nil
			}

			if port > 0 {
				printListenAddrs(port)
				return serveWeb(port, normalStartStr, normalEndStr, minRestH, maxOvertimeH)
			}

			if strings.TrimSpace(startStr) == "" {
				return fmt.Errorf("--start is required (or use --port)")
			}
			if lengthH <= 0 {
				return fmt.Errorf("--length must be > 0")
			}
			if minRestH <= 0 {
				return fmt.Errorf("--min-rest must be > 0")
			}

			res, err := compute(startStr, lengthH, combineH, fullH, normalStartStr, normalEndStr, minRestH, maxOvertimeH)
			if err != nil {
				return err
			}
			printCLI(res)
			return nil
		},
	}

	cmd.Version = appVersion
	cmd.SetVersionTemplate("nightrelcalc v{{.Version}}\n")
	cmd.Flags().BoolP("version", "v", false, "Show version and exit")

	cmd.Flags().StringVar(&startStr, "start", "", "Release start HH:MM")
	cmd.Flags().Float64Var(&lengthH, "length", 0, "Release length in hours (e.g. 4, 3.5)")
	cmd.Flags().Float64Var(&combineH, "combine", -1, "Hours of release included in full day (optional)")

	// Full is optional: 0 means "derive from normal day".
	cmd.Flags().Float64Var(&fullH, "full", 0, "Full workday hours (0 = derive from normal-start/normal-end)")

	cmd.Flags().IntVar(&port, "port", 0, "Run web UI on this port (e.g. 8484)")

	cmd.Flags().StringVar(&normalStartStr, "normal-start", "09:00", "Normal work start time (HH:MM)")
	cmd.Flags().StringVar(&normalEndStr, "normal-end", "17:30", "Normal work end time (HH:MM)")
	cmd.Flags().Float64Var(&minRestH, "min-rest", 11, "Minimum rest after release end in hours (default 11)")
	cmd.Flags().Float64Var(&maxOvertimeH, "max-overtime", 4, "Maximum allowed overtime in hours (legal cap, default 4)")

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

/* ---------------- core logic (clock math only) ---------------- */

func compute(startStr string, lengthH, combineH, fullH float64, normalStartStr, normalEndStr string, minRestH, maxOvertimeH float64) (*CalcResult, error) {
	rsMin, err := parseHHMMToMin(startStr)
	if err != nil {
		return nil, err
	}
	if lengthH <= 0 {
		return nil, fmt.Errorf("length must be > 0")
	}

	nsMin, err := parseHHMMToMin(normalStartStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --normal-start: %w", err)
	}
	neMin, err := parseHHMMToMin(normalEndStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --normal-end: %w", err)
	}
	normalLenMin := neMin - nsMin
	if normalLenMin <= 0 {
		return nil, fmt.Errorf("normal day must be within same day and end after start (e.g. 09:00 -> 17:30)")
	}

	minRestMin := hoursToMin(minRestH)
	if minRestMin <= 0 {
		return nil, fmt.Errorf("min rest must be > 0")
	}

	maxOvertimeMin := hoursToMin(maxOvertimeH)
	if maxOvertimeMin < 0 {
		return nil, fmt.Errorf("max overtime must be >= 0")
	}

	releaseLenMin := hoursToMin(lengthH)

	// Full day: derive from normal day unless explicitly provided and >0
	fullDayMin := normalLenMin
	if fullH > 0 {
		fullDayMin = hoursToMin(fullH)
	}

	reEndAbs := rsMin + releaseLenMin
	releaseWindow := fmtRange(rsMin, reEndAbs)

	// Next-day: start = max(next day normal-start, releaseEnd+minRest)
	// end = start + normal day length
	nextStart := calcNextDayStartAbs(reEndAbs, nsMin, minRestMin)
	nextEnd := nextStart + normalLenMin
	nextDayHours := fmtRange(nextStart, nextEnd)

	scenarios := make([]Scenario, 0, 3)

	// 1) Full day (release included as much as possible)
	// Legal cap: include at least (releaseLen - maxOvertime) so OT <= maxOvertime; pull work start later if needed
	requiredIncluded := maxInt(0, releaseLenMin-maxOvertimeMin)
	inc := minInt(fullDayMin, maxInt(requiredIncluded, minInt(releaseLenMin, fullDayMin)))
	pre := fullDayMin - inc
	workStart := rsMin - pre
	workEnd := rsMin + inc
	otMin := maxInt(releaseLenMin-inc, 0)

	scenarios = append(scenarios, Scenario{
		Title:           "Full day (release included) - No Overtime",
		WorkHours:       fmtRange(workStart, workEnd),
		ReleaseWindow:   releaseWindow,
		TotalWork:       fmtRange(workStart, reEndAbs),
		ReleaseIncluded: fmtHM(inc),
		Overtime:        fmtHM(otMin),
		NextDayHours:    nextDayHours,
	})

	// 2) Full day + release (all overtime) — cap OT at max by pulling work start later
	ot2 := releaseLenMin
	workStart2 := rsMin - fullDayMin
	workEnd2 := rsMin
	if ot2 > maxOvertimeMin {
		// End work (releaseEnd - maxOvertime) so only maxOvertime is OT after work
		workEnd2 = reEndAbs - maxOvertimeMin
		workStart2 = workEnd2 - fullDayMin
		ot2 = maxOvertimeMin
	}
	scenarios = append(scenarios, Scenario{
		Title:           "Full day + release (Overtime)",
		WorkHours:       fmtRange(workStart2, workEnd2),
		ReleaseWindow:   releaseWindow,
		TotalWork:       fmtRange(workStart2, reEndAbs),
		ReleaseIncluded: fmtHM(0),
		Overtime:        fmtHM(ot2),
		NextDayHours:    nextDayHours,
	})

	// 3) Full day + combine + rest (only if combine set)
	if combineH >= 0 {
		x := hoursToMin(combineH)
		x = minInt(x, releaseLenMin)
		x = minInt(x, fullDayMin)

		pre3 := fullDayMin - x
		workStart3 := rsMin - pre3
		workEnd3 := rsMin + x
		ot3 := releaseLenMin - x
		if ot3 > maxOvertimeMin {
			// Pull work start later: include more of release so OT <= max
			x = maxInt(releaseLenMin-maxOvertimeMin, 0)
			x = minInt(x, fullDayMin)
			pre3 = fullDayMin - x
			workStart3 = rsMin - pre3
			workEnd3 = rsMin + x
			ot3 = releaseLenMin - x
		}

		scenarios = append(scenarios, Scenario{
			Title:           fmt.Sprintf("Full day + %.2fh + %.2fh", combineH, lengthH-combineH),
			WorkHours:       fmtRange(workStart3, workEnd3),
			ReleaseWindow:   releaseWindow,
			TotalWork:       fmtRange(workStart3, reEndAbs),
			ReleaseIncluded: fmtHM(x),
			Overtime:        fmtHM(ot3),
			NextDayHours:    nextDayHours,
		})
	}

	return &CalcResult{
		ReleaseStart: fmtClock(rsMin),
		ReleaseEnd:   fmtClock(reEndAbs),
		ReleaseLen:   fmtHM(releaseLenMin),

		FullDay: fmtHM(fullDayMin),

		NormalStart: fmtClock(nsMin),
		NormalEnd:   fmtClock(neMin),
		NormalLen:   fmtHM(normalLenMin),

		MinRest:     fmtHM(minRestMin),
		MaxOvertime: fmtHM(maxOvertimeMin),

		Scenarios: scenarios,
	}, nil
}

func calcNextDayStartAbs(releaseEndAbs int, normalStartOfDayMin int, minRestMin int) int {
	earliest := releaseEndAbs + minRestMin
	reEndDay := floorDiv(releaseEndAbs, 1440)
	nextDay := (reEndDay + 1) * 1440
	baseline := nextDay + normalStartOfDayMin
	return maxInt(baseline, earliest)
}

func printCLI(res *CalcResult) {
	fmt.Printf("Release Window: %s -> %s (len %s)\n", res.ReleaseStart, res.ReleaseEnd, res.ReleaseLen)
	fmt.Printf("Normal day: %s -> %s (len %s)\n", res.NormalStart, res.NormalEnd, res.NormalLen)
	fmt.Printf("Full day used: %s, Min rest: %s, Max overtime (cap): %s\n\n", res.FullDay, res.MinRest, res.MaxOvertime)

	for _, s := range res.Scenarios {
		fmt.Println(s.Title)
		fmt.Printf("  Work Hours:                    %s\n", s.WorkHours)
		fmt.Printf("  Release Window:                %s\n", s.ReleaseWindow)
		fmt.Printf("  Total Work:                    %s\n", s.TotalWork)
		fmt.Printf("  Release Hours Included in Full %s\n", s.ReleaseIncluded)
		fmt.Printf("  Overtime:                      %s\n", s.Overtime)
		fmt.Printf("  Next Day Hours:                %s\n\n", s.NextDayHours)
	}
}

/* ---------------- web ---------------- */

func serveWeb(port int, defaultNormalStart, defaultNormalEnd string, defaultMinRestH, defaultMaxOvertimeH float64) error {
	tpl := template.Must(template.New("page").Parse(pageHTML))
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		data := PageData{
			Start:       orDefault(q.Get("start"), webDefaultStart),
			Length:      orDefault(q.Get("length"), webDefaultLength),
			Combine:     strings.TrimSpace(q.Get("combine")),
			NormalStart: orDefault(strings.TrimSpace(q.Get("normal_start")), webDefaultNormalStart),
			NormalEnd:   orDefault(strings.TrimSpace(q.Get("normal_end")), webDefaultNormalEnd),
			MinRest:     orDefault(strings.TrimSpace(q.Get("min_rest")), webDefaultMinRest),
			MaxOvertime: orDefault(strings.TrimSpace(q.Get("max_overtime")), webDefaultMaxOvertime),

			Full:    "(auto)",
			Version: appVersion,
		}
		if data.NormalEnd == "" {
			data.NormalEnd = webDefaultNormalEnd
		}

		// If we have start and valid length, run calculation (so URL with params shows results).
		if data.Start != "" && data.Length != "" {
			lengthH, err := parseFloat(data.Length)
			if err == nil && lengthH > 0 {
				normalStart := data.NormalStart
				normalEnd := data.NormalEnd
				minRestStr := data.MinRest
				maxOvertimeStr := data.MaxOvertime
				if normalStart == "" {
					normalStart = webDefaultNormalStart
				}
				if normalEnd == "" {
					normalEnd = webDefaultNormalEnd
				}
				if minRestStr == "" {
					minRestStr = webDefaultMinRest
				}
				if maxOvertimeStr == "" {
					maxOvertimeStr = webDefaultMaxOvertime
				}
				minRestH, _ := parseFloat(minRestStr)
				maxOvertimeH, _ := parseFloat(maxOvertimeStr)
				if minRestH <= 0 {
					minRestH = defaultMinRestH
				}
				if maxOvertimeH < 0 {
					maxOvertimeH = defaultMaxOvertimeH
				}
				combineH := -1.0
				if data.Combine != "" {
					if v, err := parseFloat(data.Combine); err == nil && v >= 0 {
						combineH = v
					}
				}
				res, err := compute(data.Start, lengthH, combineH, 0, normalStart, normalEnd, minRestH, maxOvertimeH)
				if err != nil {
					data.Error = err.Error()
				} else {
					data.Result = res
					data.Full = res.FullDay
					data.ShareDescription = buildShareDescription(res)
				}
			}
		}

		_ = tpl.Execute(w, data)
	})

	mux.HandleFunc("/calc", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		start := strings.TrimSpace(r.FormValue("start"))
		lengthStr := strings.TrimSpace(r.FormValue("length"))
		combineStr := strings.TrimSpace(r.FormValue("combine"))
		normalStart := strings.TrimSpace(r.FormValue("normal_start"))
		normalEnd := strings.TrimSpace(r.FormValue("normal_end"))
		minRestStr := strings.TrimSpace(r.FormValue("min_rest"))
		maxOvertimeStr := strings.TrimSpace(r.FormValue("max_overtime"))

		if normalEnd == "" {
			normalEnd = "17:30"
		}

		data := PageData{
			Start:       start,
			Length:      lengthStr,
			Combine:     combineStr,
			NormalStart: normalStart,
			NormalEnd:   normalEnd,
			MinRest:     minRestStr,
			MaxOvertime: maxOvertimeStr,
			Version:     appVersion,
		}

		if start == "" {
			data.Error = "release start is required (HH:MM)"
			_ = tpl.Execute(w, data)
			return
		}

		lengthH, err := parseFloat(lengthStr)
		if err != nil || lengthH <= 0 {
			data.Error = "release length must be > 0 (hours, e.g. 4)"
			_ = tpl.Execute(w, data)
			return
		}

		if normalStart == "" {
			normalStart = "09:00"
		}
		if minRestStr == "" {
			minRestStr = "11" // prefill behavior even after empty submit
		}
		if maxOvertimeStr == "" {
			maxOvertimeStr = "4"
		}

		minRestH, err := parseFloat(minRestStr)
		if err != nil || minRestH <= 0 {
			data.Error = "min rest must be > 0 (hours, default 11)"
			_ = tpl.Execute(w, data)
			return
		}

		maxOvertimeH, err := parseFloat(maxOvertimeStr)
		if err != nil || maxOvertimeH < 0 {
			data.Error = "max overtime must be >= 0 (hours, default 4)"
			_ = tpl.Execute(w, data)
			return
		}

		combineH := -1.0
		if combineStr != "" {
			v, err := parseFloat(combineStr)
			if err != nil || v < 0 {
				data.Error = "combine must be >= 0 (hours) or empty"
				_ = tpl.Execute(w, data)
				return
			}
			combineH = v
		}

		// Web: full day is derived from normal day.
		_, err = compute(start, lengthH, combineH, 0, normalStart, normalEnd, minRestH, maxOvertimeH)
		if err != nil {
			data.Error = err.Error()
			_ = tpl.Execute(w, data)
			return
		}
		// Redirect to GET with query params (only non-defaults) so the URL reflects the calculation.
		redir := buildCalcURL(start, lengthStr, combineStr, normalStart, normalEnd, minRestStr, maxOvertimeStr)
		http.Redirect(w, r, redir, http.StatusFound)
	})

	return http.ListenAndServe(fmt.Sprintf(":%d", port), mux)
}

// buildCalcURL returns "/?start=...&length=..." and only adds other params when not default.
func buildCalcURL(start, length, combine, normalStart, normalEnd, minRest, maxOvertime string) string {
	v := url.Values{}
	v.Set("start", start)
	v.Set("length", length)
	if combine != "" {
		v.Set("combine", combine)
	}
	if normalStart != "" && normalStart != webDefaultNormalStart {
		v.Set("normal_start", normalStart)
	}
	if normalEnd != "" && normalEnd != webDefaultNormalEnd {
		v.Set("normal_end", normalEnd)
	}
	if minRest != "" && minRest != webDefaultMinRest {
		v.Set("min_rest", minRest)
	}
	if maxOvertime != "" && maxOvertime != webDefaultMaxOvertime {
		v.Set("max_overtime", maxOvertime)
	}
	return "/?" + v.Encode()
}

func orDefault(val, def string) string {
	if strings.TrimSpace(val) == "" {
		return def
	}
	return strings.TrimSpace(val)
}

// buildShareDescription returns the meta description for link previews when Result is set.
func buildShareDescription(res *CalcResult) string {
	if len(res.Scenarios) == 0 {
		return fmt.Sprintf("Release %s → %s (len %s). Full day %s, min rest %s, max OT %s.",
			res.ReleaseStart, res.ReleaseEnd, res.ReleaseLen, res.FullDay, res.MinRest, res.MaxOvertime)
	}
	s := res.Scenarios[0]
	return fmt.Sprintf("Release %s→%s (%s). Work %s. Included %s, overtime %s. Next day %s.",
		res.ReleaseStart, res.ReleaseEnd, res.ReleaseLen, s.WorkHours, s.ReleaseIncluded, s.Overtime, s.NextDayHours)
}

/* ---------------- helpers ---------------- */

func fmtRange(aMin, bMin int) string {
	return fmtClock(aMin) + " -> " + fmtClock(bMin)
}

func parseHHMMToMin(s string) (int, error) {
	t := strings.TrimSpace(s)
	parts := strings.Split(t, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time %q, expected HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("invalid time %q, expected HH:MM", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q, expected HH:MM", s)
	}
	return h*60 + m, nil
}

func hoursToMin(h float64) int {
	return int(math.Round(h * 60.0))
}

func fmtClock(min int) string {
	days := floorDiv(min, 1440)
	min = mod(min, 1440)
	h := min / 60
	m := min % 60
	if days == 0 {
		return fmt.Sprintf("%02d:%02d", h, m)
	}
	return fmt.Sprintf("%02d:%02d (+%dd)", h, m, days)
}

func fmtHM(min int) string {
	if min < 0 {
		min = -min
	}
	h := min / 60
	m := min % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

func parseFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", ".")
	return strconv.ParseFloat(s, 64)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func floorDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	q := a / b
	r := a % b
	if (r != 0) && ((r > 0) != (b > 0)) {
		q--
	}
	return q
}

func mod(a, b int) int {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func printListenAddrs(port int) {
	fmt.Println("Listening on:")
	fmt.Printf("  http://127.0.0.1:%d/\n", port)

	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			fmt.Printf("  http://%s:%d/\n", ip.String(), port)
		}
	}
	fmt.Println()
}

/* ---------------- HTML ---------------- */

const pageHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>nightrelcalc</title>
  {{if .ShareDescription}}
  <meta name="description" content="{{.ShareDescription}}">
  <meta property="og:description" content="{{.ShareDescription}}">
  {{end}}
  <style>
    body { font-family: system-ui, sans-serif; margin: 0; padding: 24px; max-width: 960px; box-sizing: border-box; }
    * { box-sizing: border-box; }
    h2 { margin-top: 0; font-weight: 600; }
    .err { color: #b00020; margin: 12px 0; padding: 10px; background: #ffebee; border-radius: 6px; }
    .card { border: 1px solid #e0e0e0; border-radius: 10px; padding: 16px; margin: 16px 0; background: #fafafa; }
    .card:first-of-type { background: #fff; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace; }
    table { border-collapse: collapse; width: 100%; margin-top: 10px; }
    td { padding: 8px 10px; border-top: 1px solid #eee; vertical-align: top; }
    .k { width: 320px; color: #444; }
    .hint { color: #666; font-size: 0.9em; margin-top: 4px; }
    footer { margin-top: 40px; color: #666; font-size: 0.9em; text-align: center; }

    .form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0 32px; }
    @media (max-width: 640px) { .form-grid { grid-template-columns: 1fr; } }
    .form-section { margin-bottom: 4px; }
    .form-section-title { font-size: 0.85em; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: #555; margin-bottom: 12px; padding-bottom: 6px; border-bottom: 1px solid #e0e0e0; }
    .field { margin-bottom: 14px; }
    .field label { display: block; font-weight: 500; color: #333; margin-bottom: 4px; font-size: 0.95em; }
    .field input[type="number"], .field input[type="text"] { padding: 8px 10px; font-size: 1em; border: 1px solid #ccc; border-radius: 6px; width: 100%; max-width: 140px; }
    .time-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
    .time-row input.time-value { max-width: 80px; }
    .time-picker-btn { padding: 6px 12px; font-size: 0.9em; background: #f5f5f5; border: 1px solid #ccc; border-radius: 6px; cursor: pointer; }
    .time-picker-btn:hover { background: #e8e8e8; }
    .time-picker-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.4); display: none; align-items: center; justify-content: center; z-index: 1000; }
    .time-picker-overlay.open { display: flex; }
    .time-picker-modal { background: #fff; border-radius: 10px; padding: 20px; box-shadow: 0 4px 20px rgba(0,0,0,0.2); min-width: 200px; }
    .time-picker-modal h3 { margin: 0 0 14px 0; font-size: 1em; font-weight: 600; }
    .time-picker-row { display: flex; gap: 12px; align-items: center; margin-bottom: 16px; }
    .time-picker-row select { padding: 8px 10px; font-size: 1em; border: 1px solid #ccc; border-radius: 6px; }
    .time-picker-actions { display: flex; gap: 8px; justify-content: flex-end; }
    .time-picker-actions button { padding: 8px 16px; border-radius: 6px; border: 1px solid #ccc; background: #f5f5f5; cursor: pointer; font-size: 0.95em; }
    .time-picker-actions button.primary { background: #1976d2; color: #fff; border-color: #1976d2; }
    .time-picker-actions button.primary:hover { background: #1565c0; }
    .field input:focus { outline: none; border-color: #1976d2; box-shadow: 0 0 0 2px rgba(25,118,210,0.2); }
    .fields-row { display: flex; gap: 20px; flex-wrap: wrap; }
    .fields-row .field { flex: 1; min-width: 120px; }
    .form-actions { margin-top: 0px; padding-top: 16px; border-top: 1px solid #e0e0e0; }
    button[type="submit"] { padding: 10px 20px; font-size: 1em; font-weight: 500; background: #1976d2; color: #fff; border: none; border-radius: 6px; cursor: pointer; }
    button[type="submit"]:hover { background: #1565c0; }
  </style>
</head>
<body>
    <form method="POST" action="/calc">
    <div class="form-grid">
      <div class="form-section">
        <div class="form-section-title">Release</div>
        <div class="field">
          <label for="start">Release start</label>
          <div class="time-row">
            <input id="start" name="start" type="text" class="time-value" value="{{.Start}}" placeholder="18:30" pattern="[0-9]{1,2}:[0-9]{2}" required autocomplete="off">
            <button type="button" class="time-picker-btn" data-for="start" aria-label="Pick time">🕐</button>
          </div>
        </div>
        <div class="field">
          <label for="length">Release length (hours)</label>
          <input id="length" name="length" type="number" min="0.25" step="0.25" value="{{.Length}}" placeholder="4" required>
          <div class="hint">e.g. 4, 3.5, 2.25</div>
        </div>
        <div class="field">
          <label for="combine">Combine (hours)</label>
          <input id="combine" name="combine" type="number" min="0" step="0.25" value="{{.Combine}}" placeholder="optional">
        </div>
      </div>

      <div class="form-section">
        <div class="form-section-title">Work day</div>
        <div class="fields-row">
          <div class="field">
            <label for="normal_start">Normal work start</label>
            <div class="time-row">
              <input id="normal_start" name="normal_start" type="text" class="time-value" value="{{.NormalStart}}" placeholder="09:00" pattern="[0-9]{1,2}:[0-9]{2}" autocomplete="off">
              <button type="button" class="time-picker-btn" data-for="normal_start" aria-label="Pick time">🕐</button>
            </div>
          </div>
          <div class="field">
            <label for="normal_end">Normal work end</label>
            <div class="time-row">
              <input id="normal_end" name="normal_end" type="text" class="time-value" value="{{.NormalEnd}}" placeholder="17:30" pattern="[0-9]{1,2}:[0-9]{2}" autocomplete="off">
              <button type="button" class="time-picker-btn" data-for="normal_end" aria-label="Pick time">🕐</button>
            </div>
          </div>
        </div>
        <div class="form-section-title">Legal limits</div>
        <div class="fields-row">
          <div class="field">
            <label for="min_rest">Min rest after release (hours)</label>
            <input id="min_rest" name="min_rest" type="number" min="1" step="0.5" value="{{.MinRest}}" placeholder="11">
          </div>
          <div class="field">
            <label for="max_overtime">Max overtime (hours)</label>
            <input id="max_overtime" name="max_overtime" type="number" min="0" step="0.5" value="{{.MaxOvertime}}" placeholder="4">
            <div class="hint">Legal cap; work start shifts if OT would exceed this</div>
          </div>
        </div>
      </div>
    </div>

    <div class="form-actions">
      <button type="submit">Calculate</button>
    </div>
  </form>

  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}

  {{with .Result}}
    <div class="card">
      <div><b>Release Window</b>: <span class="mono">{{.ReleaseStart}}</span> → <span class="mono">{{.ReleaseEnd}}</span> (len <span class="mono">{{.ReleaseLen}}</span>)</div>
      <div><b>Normal day</b>: <span class="mono">{{.NormalStart}} → {{.NormalEnd}}</span> (len <span class="mono">{{.NormalLen}}</span>)</div>
      <div><b>Full day used</b>: <span class="mono">{{.FullDay}}</span>, <b>Min rest</b>: <span class="mono">{{.MinRest}}</span>, <b>Max overtime (cap)</b>: <span class="mono">{{.MaxOvertime}}</span></div>
    </div>

    {{range .Scenarios}}
      <div class="card">
        <div><b>{{.Title}}</b></div>
        <table>
          <tr><td class="k">Work Hours</td><td class="mono">{{.WorkHours}}</td></tr>
          <tr><td class="k">Release Window</td><td class="mono">{{.ReleaseWindow}}</td></tr>
          <tr><td class="k">Total Work</td><td class="mono">{{.TotalWork}}</td></tr>
          <tr><td class="k">Release Hours Included in Full</td><td class="mono">{{.ReleaseIncluded}}</td></tr>
          <tr><td class="k">Overtime</td><td class="mono">{{.Overtime}}</td></tr>
          <tr><td class="k">Next Day Hours</td><td class="mono">{{.NextDayHours}}</td></tr>
        </table>
      </div>
    {{end}}
  {{end}}

  <div id="time-picker-overlay" class="time-picker-overlay" role="dialog" aria-modal="true" aria-label="Pick time (24h)">
    <div class="time-picker-modal">
      <h3 id="time-picker-title">Time (24h)</h3>
      <div class="time-picker-row">
        <label for="tp-hour">Hour</label>
        <select id="tp-hour"></select>
        <label for="tp-minute">Min</label>
        <select id="tp-minute"></select>
      </div>
      <div class="time-picker-actions">
        <button type="button" id="tp-cancel">Cancel</button>
        <button type="button" id="tp-ok" class="primary">OK</button>
      </div>
    </div>
  </div>

  <script>
(function() {
  var overlay = document.getElementById('time-picker-overlay');
  var hourSelect = document.getElementById('tp-hour');
  var minuteSelect = document.getElementById('tp-minute');
  var okBtn = document.getElementById('tp-ok');
  var cancelBtn = document.getElementById('tp-cancel');
  var targetInput = null;

  function pad2(n) { return (n < 10 ? '0' : '') + n; }
  function parseTime(s) {
    if (!s || typeof s !== 'string') return { h: 0, m: 0 };
    s = s.trim();
    var m = s.match(/^(\d{1,2}):(\d{2})$/);
    if (!m) return { h: 0, m: 0 };
    var h = parseInt(m[1], 10);
    var min = parseInt(m[2], 10);
    if (h < 0 || h > 23 || min < 0 || min > 59) return { h: 0, m: 0 };
    return { h: h, m: min };
  }
  function fillDropdowns() {
    hourSelect.innerHTML = '';
    for (var i = 0; i < 24; i++) {
      var o = document.createElement('option');
      o.value = i;
      o.textContent = pad2(i);
      hourSelect.appendChild(o);
    }
    minuteSelect.innerHTML = '';
    for (var j = 0; j < 60; j++) {
      var o = document.createElement('option');
      o.value = j;
      o.textContent = pad2(j);
      minuteSelect.appendChild(o);
    }
  }
  fillDropdowns();

  function openPicker(inputId) {
    targetInput = document.getElementById(inputId);
    if (!targetInput) return;
    var val = targetInput.value;
    var t = parseTime(val);
    hourSelect.value = t.h;
    minuteSelect.value = t.m;
    overlay.classList.add('open');
    hourSelect.focus();
  }
  function closePicker() {
    overlay.classList.remove('open');
    targetInput = null;
  }
  function applyTime() {
    if (!targetInput) return;
    var h = parseInt(hourSelect.value, 10);
    var m = parseInt(minuteSelect.value, 10);
    targetInput.value = pad2(h) + ':' + pad2(m);
    closePicker();
  }

  document.querySelectorAll('.time-picker-btn').forEach(function(btn) {
    btn.addEventListener('click', function() { openPicker(btn.getAttribute('data-for')); });
  });
  okBtn.addEventListener('click', applyTime);
  cancelBtn.addEventListener('click', closePicker);
  overlay.addEventListener('click', function(e) {
    if (e.target === overlay) closePicker();
  });
  document.addEventListener('keydown', function(e) {
    if (!overlay.classList.contains('open')) return;
    if (e.key === 'Escape') { e.preventDefault(); closePicker(); }
    if (e.key === 'Enter') { e.preventDefault(); applyTime(); }
  });
})();
  </script>

  <footer>nightrelcalc v{{.Version}}</footer>
</body>
</html>`
