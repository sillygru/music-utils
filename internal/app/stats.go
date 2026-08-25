package app

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/reqlog"
)

// RunStats implements `music-utils stats`, which reads the request log SQLite
// database and prints performance, activity, and traffic statistics formatted
// in ASCII/Unicode boxes, alongside cached catalog counts.
func RunStats(args []string) int {
	return RunStatsTo(os.Stdout, os.Stderr, args)
}

// RunStatsTo runs the stats subcommand with custom output streams.
func RunStatsTo(out, errOut io.Writer, args []string) int {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	flags.SetOutput(errOut)
	dbPath := flags.String("db", "", "request log database path (defaults to REQUEST_LOG_DB_PATH)")
	metadataPath := flags.String("metadata", "", "metadata database path (defaults to METADATA_DB_PATH)")
	lyricsPath := flags.String("lyrics", "", "lyrics database path (defaults to LYRICS_DB_PATH)")
	coverPath := flags.String("cover", "", "cover database path (defaults to COVER_DB_PATH)")
	days := flags.Int("days", 14, "number of days for daily activity histogram")
	top := flags.Int("top", 10, "number of top endpoints and user agents to show")

	if err := flags.Parse(args); err != nil {
		return 1
	}

	cfg := config.Load()
	if strings.TrimSpace(*dbPath) == "" {
		*dbPath = cfg.RequestLogDBPath
	}
	if strings.TrimSpace(*metadataPath) == "" {
		*metadataPath = cfg.MetadataDBPath
	}
	if strings.TrimSpace(*lyricsPath) == "" {
		*lyricsPath = cfg.LyricsDBPath
	}
	if strings.TrimSpace(*coverPath) == "" {
		*coverPath = cfg.CoverDBPath
	}

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(errOut, "stats: cannot open database %s: %v\n", *dbPath, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var metadataDB, lyricsDB, coverDB *sql.DB
	dbCfg := db.Config{
		MmapSize:     64 * 1024 * 1024,
		CacheSizeKB:  -8000,
		MaxOpenConns: 1,
	}

	if _, err := os.Stat(*metadataPath); err == nil {
		if mDB, err := db.Open(*metadataPath, dbCfg); err == nil {
			metadataDB = mDB
			defer metadataDB.Close()
		}
	}
	if _, err := os.Stat(*lyricsPath); err == nil {
		if lDB, err := db.Open(*lyricsPath, dbCfg); err == nil {
			lyricsDB = lDB
			defer lyricsDB.Close()
		}
	}
	if _, err := os.Stat(*coverPath); err == nil {
		if cDB, err := db.Open(*coverPath, dbCfg); err == nil {
			coverDB = cDB
			defer coverDB.Close()
		}
	}

	cacheStats, _ := db.GetCacheStats(ctx, metadataDB, lyricsDB, coverDB)

	report, err := reqlog.QueryStats(ctx, *dbPath, reqlog.StatsOptions{
		DailyDays: *days,
		TopLimit:  *top,
	})
	if err != nil {
		fmt.Fprintf(errOut, "stats: query error: %v\n", err)
		return 1
	}

	renderStatsReport(out, report, &cacheStats)
	return 0
}

const totalBoxWidth = 82

func renderStatsReport(w io.Writer, r *reqlog.StatsReport, c *db.CacheStats) {
	if r.TotalRequests == 0 {
		renderEmptyBox(w, r)
		if c != nil {
			fmt.Fprintln(w)
			renderCachedContentBox(w, c)
		}
		return
	}

	renderOverviewBox(w, r)
	if c != nil {
		fmt.Fprintln(w)
		renderCachedContentBox(w, c)
	}
	fmt.Fprintln(w)
	renderWindowsBox(w, r)
	if len(r.Daily) > 0 {
		fmt.Fprintln(w)
		renderDailyBox(w, r)
	}
	if len(r.Endpoints) > 0 {
		fmt.Fprintln(w)
		renderEndpointsBox(w, r)
	}
	if len(r.Outcomes) > 0 || len(r.StatusCodes) > 0 || len(r.UserAgents) > 0 {
		fmt.Fprintln(w)
		renderBreakdownsBox(w, r)
	}
}

func renderCachedContentBox(w io.Writer, c *db.CacheStats) {
	printHeaderWithTitle(w, "CACHED CONTENT", totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("Unique Songs:       %10s individual songs", formatNumber(c.UniqueSongs)), totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("Total Cached:       %10s items", formatNumber(c.TotalCached)), totalBoxWidth)
	printBoxDivider(w, totalBoxWidth)
	printBoxLine(w, "CACHE BREAKDOWN:", totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  • Song Metadata:  %10s songs", formatNumber(c.MetadataSongs)), totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  • Song Lyrics:    %10s songs", formatNumber(c.LyricsSongs)), totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  • Song Covers:    %10s songs", formatNumber(c.SongCovers)), totalBoxWidth)
	if c.AlbumCovers > 0 || c.ArtistCovers > 0 {
		printBoxLine(w, fmt.Sprintf("  • Album Covers:   %10s covers", formatNumber(c.AlbumCovers)), totalBoxWidth)
		printBoxLine(w, fmt.Sprintf("  • Artist Covers:  %10s covers", formatNumber(c.ArtistCovers)), totalBoxWidth)
		printBoxLine(w, fmt.Sprintf("  • Total Covers:   %10s covers", formatNumber(c.TotalCovers)), totalBoxWidth)
	}
	printBoxBottom(w, totalBoxWidth)
}

func renderEmptyBox(w io.Writer, r *reqlog.StatsReport) {
	printBoxTop(w, totalBoxWidth)
	printBoxCenter(w, "MUSIC-UTILS REQUEST STATS", totalBoxWidth)
	printBoxDivider(w, totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("Database: %s (%s)", r.DBPath, formatBytes(r.FileSize)), totalBoxWidth)
	printBoxLine(w, "Status:   No request records logged yet.", totalBoxWidth)
	printBoxBottom(w, totalBoxWidth)
}

func renderOverviewBox(w io.Writer, r *reqlog.StatsReport) {
	printBoxTop(w, totalBoxWidth)
	printBoxCenter(w, "MUSIC-UTILS REQUEST STATS", totalBoxWidth)
	printBoxDivider(w, totalBoxWidth)

	dbInfo := fmt.Sprintf("Database:   %s (%s)", r.DBPath, formatBytes(r.FileSize))
	printBoxLine(w, dbInfo, totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("Total Logs: %s requests", formatNumber(r.TotalRequests)), totalBoxWidth)
	if !r.FirstRequest.IsZero() && !r.LastRequest.IsZero() {
		spanText := formatSpan(r.TimeSpan)
		printBoxLine(w, fmt.Sprintf("First Seen: %s", r.FirstRequest.Format("2006-01-02 15:04:05 UTC")), totalBoxWidth)
		printBoxLine(w, fmt.Sprintf("Last Seen:  %s (%s)", r.LastRequest.Format("2006-01-02 15:04:05 UTC"), spanText), totalBoxWidth)
	}

	printBoxDivider(w, totalBoxWidth)
	printBoxLine(w, "STATUS & PERFORMANCE", totalBoxWidth)

	latOverview := fmt.Sprintf("Avg Latency:      %s", formatLatency(r.AvgLatencyMs))
	printBoxLine(w, fmt.Sprintf("  2xx Success:  %10s (%5.1f%%)   │   %-30s", formatNumber(r.Status2xxCount), r.Status2xxPct, latOverview), totalBoxWidth)

	cacheLat := fmt.Sprintf("Cache Latency:    %s", formatLatency(r.AvgCacheMs))
	printBoxLine(w, fmt.Sprintf("  3xx Redirect: %10s (%5.1f%%)   │   %-30s", formatNumber(r.Status3xxCount), r.Status3xxPct, cacheLat), totalBoxWidth)

	upLat := fmt.Sprintf("Upstream Latency: %s", formatLatency(r.AvgUpstreamMs))
	printBoxLine(w, fmt.Sprintf("  4xx Client:   %10s (%5.1f%%)   │   %-30s", formatNumber(r.Status4xxCount), r.Status4xxPct, upLat), totalBoxWidth)

	p95 := fmt.Sprintf("p50/p95/p99:      %d / %d / %d ms", r.P50LatencyMs, r.P95LatencyMs, r.P99LatencyMs)
	printBoxLine(w, fmt.Sprintf("  5xx Server:   %10s (%5.1f%%)   │   %-30s", formatNumber(r.Status5xxCount), r.Status5xxPct, p95), totalBoxWidth)

	printBoxLine(w, fmt.Sprintf("  429 RateLimit:%10s (%5.1f%%)   │", formatNumber(r.Status429Count), r.Status429Pct), totalBoxWidth)

	printBoxDivider(w, totalBoxWidth)
	printBoxLine(w, "CACHE RESOLUTION", totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  Local Cache Hit:     %10s (%5.1f%%)", formatNumber(r.LocalHitCount), r.LocalHitPct), totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  Provider Fallback:   %10s (%5.1f%%)", formatNumber(r.FallbackHitCount), r.FallbackHitPct), totalBoxWidth)
	printBoxLine(w, fmt.Sprintf("  Checked Miss:        %10s (%5.1f%%)", formatNumber(r.MissCount), r.MissPct), totalBoxWidth)

	printBoxBottom(w, totalBoxWidth)
}

func renderWindowsBox(w io.Writer, r *reqlog.StatsReport) {
	printHeaderWithTitle(w, "ACTIVITY WINDOWS", totalBoxWidth)
	fmt.Fprintf(w, "│ %-16s │ %10s │ %14s │ %8s │ %8s │ %10s │\n",
		"Window", "Requests", "Rate", "Hit Rate", "Err Rate", "Avg Lat")
	fmt.Fprintf(w, "├──────────────────┼────────────┼────────────────┼──────────┼──────────┼────────────┤\n")

	for _, win := range r.Windows {
		fmt.Fprintf(w, "│ %-16s │ %10s │ %14s │ %7.1f%% │ %7.1f%% │ %10s │\n",
			win.Name,
			formatNumber(win.Requests),
			win.RateText,
			win.HitRatePct,
			win.ErrorRatePct,
			formatLatency(win.AvgLatencyMs),
		)
	}
	fmt.Fprintf(w, "└──────────────────┴────────────┴────────────────┴──────────┴──────────┴────────────┘\n")
}

func renderDailyBox(w io.Writer, r *reqlog.StatsReport) {
	title := fmt.Sprintf("DAILY ACTIVITY (LAST %d DAYS)", len(r.Daily))
	printHeaderWithTitle(w, title, totalBoxWidth)
	fmt.Fprintf(w, "│ %-10s │ %10s │ %-26s │ %8s │ %10s │\n",
		"Date (UTC)", "Requests", "Activity Volume", "Hit Rate", "Avg Lat")
	fmt.Fprintf(w, "├────────────┼────────────┼────────────────────────────┼──────────┼────────────┤\n")

	var maxReq int64 = 1
	for _, d := range r.Daily {
		if d.Requests > maxReq {
			maxReq = d.Requests
		}
	}

	for _, d := range r.Daily {
		hitRate := calcPct(d.LocalHits+d.FallbackHits, d.Requests)
		bar := renderBar(d.Requests, maxReq, 26)
		fmt.Fprintf(w, "│ %-10s │ %10s │ %s │ %7.1f%% │ %10s │\n",
			d.Date,
			formatNumber(d.Requests),
			bar,
			hitRate,
			formatLatency(d.AvgLatencyMs),
		)
	}
	fmt.Fprintf(w, "└────────────┴────────────┴────────────────────────────┴──────────┴────────────┘\n")
}

func renderEndpointsBox(w io.Writer, r *reqlog.StatsReport) {
	printHeaderWithTitle(w, "TOP ENDPOINTS", totalBoxWidth)
	fmt.Fprintf(w, "│ %-30s │ %10s │ %6s │ %8s │ %8s │ %10s │\n",
		"Endpoint", "Requests", "Share", "LocalHit", "Miss %", "Avg Lat")
	fmt.Fprintf(w, "├────────────────────────────────┼────────────┼────────┼──────────┼──────────┼────────────┤\n")

	for _, ep := range r.Endpoints {
		name := ep.Endpoint
		if utf8.RuneCountInString(name) > 30 {
			name = name[:27] + "..."
		}
		fmt.Fprintf(w, "│ %-30s │ %10s │ %5.1f%% │ %7.1f%% │ %7.1f%% │ %10s │\n",
			name,
			formatNumber(ep.Requests),
			ep.SharePct,
			ep.LocalHitPct,
			ep.MissPct,
			formatLatency(ep.AvgLatencyMs),
		)
	}
	fmt.Fprintf(w, "└────────────────────────────────┴────────────┴────────┴──────────┴──────────┴────────────┘\n")
}

func renderBreakdownsBox(w io.Writer, r *reqlog.StatsReport) {
	printHeaderWithTitle(w, "OUTCOMES & TOP USER AGENTS", totalBoxWidth)

	printBoxLine(w, "OUTCOMES BREAKDOWN:", totalBoxWidth)
	for _, o := range r.Outcomes {
		line := fmt.Sprintf("  • %-26s %10s (%5.1f%%)", o.Outcome, formatNumber(o.Requests), o.SharePct)
		printBoxLine(w, line, totalBoxWidth)
	}

	if len(r.UserAgents) > 0 {
		printBoxDivider(w, totalBoxWidth)
		printBoxLine(w, "TOP USER AGENTS:", totalBoxWidth)
		for _, ua := range r.UserAgents {
			name := ua.UserAgent
			if utf8.RuneCountInString(name) > 40 {
				name = name[:37] + "..."
			}
			line := fmt.Sprintf("  • %-40s %10s (%5.1f%%)", name, formatNumber(ua.Requests), ua.SharePct)
			printBoxLine(w, line, totalBoxWidth)
		}
	}

	printBoxBottom(w, totalBoxWidth)
}

func renderBar(value, max int64, width int) string {
	if max <= 0 || width <= 0 {
		return strings.Repeat("░", width)
	}
	ratio := float64(value) / float64(max)
	if ratio > 1.0 {
		ratio = 1.0
	}
	filled := int(math.Round(ratio * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func printHeaderWithTitle(w io.Writer, title string, width int) {
	rem := width - utf8.RuneCountInString(title) - 5
	if rem < 0 {
		rem = 0
	}
	fmt.Fprintf(w, "┌─ %s %s┐\n", title, strings.Repeat("─", rem))
}

func printBoxTop(w io.Writer, width int) {
	fmt.Fprintf(w, "┌%s┐\n", strings.Repeat("─", width-2))
}

func printBoxBottom(w io.Writer, width int) {
	fmt.Fprintf(w, "└%s┘\n", strings.Repeat("─", width-2))
}

func printBoxDivider(w io.Writer, width int) {
	fmt.Fprintf(w, "├%s┤\n", strings.Repeat("─", width-2))
}

func printBoxCenter(w io.Writer, text string, width int) {
	contentWidth := width - 4
	textLen := utf8.RuneCountInString(text)
	if textLen >= contentWidth {
		fmt.Fprintf(w, "│ %s │\n", text[:contentWidth])
		return
	}
	leftPad := (contentWidth - textLen) / 2
	rightPad := contentWidth - textLen - leftPad
	fmt.Fprintf(w, "│ %s%s%s │\n", strings.Repeat(" ", leftPad), text, strings.Repeat(" ", rightPad))
}

func printBoxLine(w io.Writer, text string, width int) {
	contentWidth := width - 4
	textLen := utf8.RuneCountInString(text)
	if textLen >= contentWidth {
		fmt.Fprintf(w, "│ %s │\n", text[:contentWidth])
		return
	}
	rightPad := contentWidth - textLen
	fmt.Fprintf(w, "│ %s%s │\n", text, strings.Repeat(" ", rightPad))
}

func formatNumber(n int64) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var res []byte
	rem := len(s) % 3
	if rem > 0 {
		res = append(res, s[:rem]...)
		if len(s) > rem {
			res = append(res, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		res = append(res, s[i:i+3]...)
		if i+3 < len(s) {
			res = append(res, ',')
		}
	}
	return string(res)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatLatency(ms float64) string {
	if ms < 0.1 && ms > 0 {
		return "<0.1 ms"
	}
	if ms >= 1000 {
		return fmt.Sprintf("%.2f s", ms/1000.0)
	}
	return fmt.Sprintf("%.1f ms", ms)
}

func formatSpan(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("spanning %d days, %d hours", days, hours)
		}
		return fmt.Sprintf("spanning %d days", days)
	}
	if hours > 0 {
		return fmt.Sprintf("spanning %d hours, %d mins", hours, mins)
	}
	return fmt.Sprintf("spanning %d mins", mins)
}

func calcPct(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	pct := (float64(part) / float64(total)) * 100.0
	if math.IsNaN(pct) || math.IsInf(pct, 0) {
		return 0
	}
	return pct
}
