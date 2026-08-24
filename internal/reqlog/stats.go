package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// StatsOptions configures the request log statistics computation.
type StatsOptions struct {
	// DailyDays is how many days of daily traffic to include in the breakdown.
	// Default is 14 if <= 0.
	DailyDays int
	// TopLimit is the maximum number of top items (endpoints, user-agents) to return.
	// Default is 10 if <= 0.
	TopLimit int
}

// WindowStat describes traffic and performance within a time window.
type WindowStat struct {
	Name         string
	Requests     int64
	RateText     string
	LocalHits    int64
	FallbackHits int64
	Misses       int64
	Errors       int64
	HitRatePct   float64
	ErrorRatePct float64
	AvgLatencyMs float64
}

// DailyStat describes traffic for a single calendar day (UTC).
type DailyStat struct {
	Date         string // YYYY-MM-DD
	Requests     int64
	LocalHits    int64
	FallbackHits int64
	Misses       int64
	Errors       int64
	AvgLatencyMs float64
}

// EndpointStat describes request activity for a specific endpoint path.
type EndpointStat struct {
	Endpoint     string
	Requests     int64
	SharePct     float64
	LocalHitPct  float64
	MissPct      float64
	AvgLatencyMs float64
}

// OutcomeStat describes occurrences of an outcome label.
type OutcomeStat struct {
	Outcome  string
	Requests int64
	SharePct float64
}

// StatusCodeStat describes occurrences of an HTTP status code.
type StatusCodeStat struct {
	Status   int
	Requests int64
	SharePct float64
}

// UserAgentStat describes occurrences of a normalized client User-Agent.
type UserAgentStat struct {
	UserAgent string
	Requests  int64
	SharePct  float64
}

// StatsReport is the complete aggregated statistics report.
type StatsReport struct {
	DBPath        string
	FileSize      int64
	TotalRequests int64
	FirstRequest  time.Time
	LastRequest   time.Time
	TimeSpan      time.Duration

	// Status code overview
	Status2xxCount int64
	Status2xxPct   float64
	Status3xxCount int64
	Status3xxPct   float64
	Status4xxCount int64
	Status4xxPct   float64
	Status5xxCount int64
	Status5xxPct   float64
	Status429Count int64
	Status429Pct   float64

	// Latency metrics
	AvgLatencyMs  float64
	AvgCacheMs    float64
	AvgUpstreamMs float64
	P50LatencyMs  int64
	P90LatencyMs  int64
	P95LatencyMs  int64
	P99LatencyMs  int64

	// Cache outcomes
	LocalHitCount    int64
	LocalHitPct      float64
	FallbackHitCount int64
	FallbackHitPct   float64
	MissCount        int64
	MissPct          float64

	// Breakdown sections
	Windows     []WindowStat
	Daily       []DailyStat
	Endpoints   []EndpointStat
	Outcomes    []OutcomeStat
	StatusCodes []StatusCodeStat
	UserAgents  []UserAgentStat
}

// QueryStats opens the request log database in read-only mode and computes
// detailed statistical breakdowns.
func QueryStats(ctx context.Context, dbPath string, opts StatsOptions) (*StatsReport, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("request log database path is empty")
	}
	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open request log database: %w", err)
	}

	dailyDays := opts.DailyDays
	if dailyDays <= 0 {
		dailyDays = 14
	}
	topLimit := opts.TopLimit
	if topLimit <= 0 {
		topLimit = 10
	}

	database, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	var totalCount int64
	if err := database.QueryRowContext(ctx, "SELECT count(*) FROM request_log").Scan(&totalCount); err != nil {
		// If table doesn't exist, return empty report
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return &StatsReport{
				DBPath:   dbPath,
				FileSize: fileInfo.Size(),
			}, nil
		}
		return nil, fmt.Errorf("query total requests: %w", err)
	}

	report := &StatsReport{
		DBPath:        dbPath,
		FileSize:      fileInfo.Size(),
		TotalRequests: totalCount,
	}

	if totalCount == 0 {
		return report, nil
	}

	// 1. Min/Max timestamps & aggregates
	var (
		minTS, maxTS              sql.NullInt64
		sumCacheMs, sumUpstreamMs sql.NullInt64
		sumTotalLatencyMs         sql.NullInt64
		count2xx, count3xx        sql.NullInt64
		count4xx, count5xx        sql.NullInt64
		count429                  sql.NullInt64
		countLocalHit, countFbHit sql.NullInt64
		countMiss                 sql.NullInt64
	)

	aggregateQuery := `
		SELECT
			MIN(l.ts),
			MAX(l.ts),
			SUM(l.cache_ms),
			SUM(l.upstream_ms),
			SUM(l.cache_ms + l.upstream_ms),
			SUM(CASE WHEN l.status >= 200 AND l.status < 300 THEN 1 ELSE 0 END),
			SUM(CASE WHEN l.status >= 300 AND l.status < 400 THEN 1 ELSE 0 END),
			SUM(CASE WHEN l.status >= 400 AND l.status < 500 THEN 1 ELSE 0 END),
			SUM(CASE WHEN l.status >= 500 AND l.status < 600 THEN 1 ELSE 0 END),
			SUM(CASE WHEN l.status = 429 THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%local_hit%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%fallback_hit%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%miss%' THEN 1 ELSE 0 END)
		FROM request_log l
		LEFT JOIN outcomes o ON o.id = l.outcome_id`

	err = database.QueryRowContext(ctx, aggregateQuery).Scan(
		&minTS, &maxTS,
		&sumCacheMs, &sumUpstreamMs, &sumTotalLatencyMs,
		&count2xx, &count3xx, &count4xx, &count5xx, &count429,
		&countLocalHit, &countFbHit, &countMiss,
	)
	if err != nil {
		return nil, fmt.Errorf("query aggregates: %w", err)
	}

	if minTS.Valid && maxTS.Valid {
		report.FirstRequest = time.UnixMilli(minTS.Int64).UTC()
		report.LastRequest = time.UnixMilli(maxTS.Int64).UTC()
		if maxTS.Int64 > minTS.Int64 {
			report.TimeSpan = report.LastRequest.Sub(report.FirstRequest)
		}
	}

	report.Status2xxCount = count2xx.Int64
	report.Status3xxCount = count3xx.Int64
	report.Status4xxCount = count4xx.Int64
	report.Status5xxCount = count5xx.Int64
	report.Status429Count = count429.Int64

	report.Status2xxPct = calcPct(report.Status2xxCount, totalCount)
	report.Status3xxPct = calcPct(report.Status3xxCount, totalCount)
	report.Status4xxPct = calcPct(report.Status4xxCount, totalCount)
	report.Status5xxPct = calcPct(report.Status5xxCount, totalCount)
	report.Status429Pct = calcPct(report.Status429Count, totalCount)

	report.LocalHitCount = countLocalHit.Int64
	report.LocalHitPct = calcPct(report.LocalHitCount, totalCount)
	report.FallbackHitCount = countFbHit.Int64
	report.FallbackHitPct = calcPct(report.FallbackHitCount, totalCount)
	report.MissCount = countMiss.Int64
	report.MissPct = calcPct(report.MissCount, totalCount)

	if totalCount > 0 {
		report.AvgLatencyMs = float64(sumTotalLatencyMs.Int64) / float64(totalCount)
		report.AvgCacheMs = float64(sumCacheMs.Int64) / float64(totalCount)
		report.AvgUpstreamMs = float64(sumUpstreamMs.Int64) / float64(totalCount)
	}

	// 2. Compute Latency Percentiles (p50, p90, p95, p99)
	report.P50LatencyMs = queryPercentile(ctx, database, totalCount, 0.50)
	report.P90LatencyMs = queryPercentile(ctx, database, totalCount, 0.90)
	report.P95LatencyMs = queryPercentile(ctx, database, totalCount, 0.95)
	report.P99LatencyMs = queryPercentile(ctx, database, totalCount, 0.99)

	// 3. Activity Windows (24h, 7d, 30d, All-Time)
	now := time.Now()
	windows := []struct {
		name     string
		duration time.Duration
	}{
		{"Last 24 Hours", 24 * time.Hour},
		{"Last 7 Days", 7 * 24 * time.Hour},
		{"Last 30 Days", 30 * 24 * time.Hour},
		{"All Time", 0},
	}

	for _, w := range windows {
		var (
			cutoffMS int64
			wQuery   string
			args     []any
		)
		if w.duration > 0 {
			cutoffMS = now.Add(-w.duration).UnixMilli()
			wQuery = `
				SELECT
					COUNT(*),
					SUM(CASE WHEN o.name LIKE '%local_hit%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN o.name LIKE '%fallback_hit%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN o.name LIKE '%miss%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN l.status >= 400 THEN 1 ELSE 0 END),
					SUM(l.cache_ms + l.upstream_ms)
				FROM request_log l
				LEFT JOIN outcomes o ON o.id = l.outcome_id
				WHERE l.ts >= ?`
			args = append(args, cutoffMS)
		} else {
			wQuery = `
				SELECT
					COUNT(*),
					SUM(CASE WHEN o.name LIKE '%local_hit%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN o.name LIKE '%fallback_hit%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN o.name LIKE '%miss%' THEN 1 ELSE 0 END),
					SUM(CASE WHEN l.status >= 400 THEN 1 ELSE 0 END),
					SUM(l.cache_ms + l.upstream_ms)
				FROM request_log l
				LEFT JOIN outcomes o ON o.id = l.outcome_id`
		}

		var (
			wCount, wLocal, wFb, wMiss, wErr sql.NullInt64
			wSumLatency                      sql.NullInt64
		)
		err := database.QueryRowContext(ctx, wQuery, args...).Scan(&wCount, &wLocal, &wFb, &wMiss, &wErr, &wSumLatency)
		if err != nil {
			return nil, fmt.Errorf("query window %s: %w", w.name, err)
		}

		cnt := wCount.Int64
		local := wLocal.Int64
		fb := wFb.Int64
		miss := wMiss.Int64
		errs := wErr.Int64

		var hitPct, errPct, avgLat float64
		if cnt > 0 {
			hitPct = float64(local+fb) / float64(cnt) * 100
			errPct = float64(errs) / float64(cnt) * 100
			avgLat = float64(wSumLatency.Int64) / float64(cnt)
		}

		var rateText string
		switch w.duration {
		case 24 * time.Hour:
			ratePerMin := float64(cnt) / (24 * 60)
			rateText = fmt.Sprintf("%.2f req/min", ratePerMin)
		case 7 * 24 * time.Hour:
			ratePerDay := float64(cnt) / 7.0
			rateText = fmt.Sprintf("%.0f req/day", ratePerDay)
		case 30 * 24 * time.Hour:
			ratePerDay := float64(cnt) / 30.0
			rateText = fmt.Sprintf("%.0f req/day", ratePerDay)
		default:
			days := report.TimeSpan.Hours() / 24.0
			if days < 1.0 {
				days = 1.0
			}
			ratePerDay := float64(cnt) / days
			rateText = fmt.Sprintf("%.0f req/day", ratePerDay)
		}

		report.Windows = append(report.Windows, WindowStat{
			Name:         w.name,
			Requests:     cnt,
			RateText:     rateText,
			LocalHits:    local,
			FallbackHits: fb,
			Misses:       miss,
			Errors:       errs,
			HitRatePct:   hitPct,
			ErrorRatePct: errPct,
			AvgLatencyMs: avgLat,
		})
	}

	// 4. Daily Breakdown for past `dailyDays`
	dailyCutoffMS := now.AddDate(0, 0, -dailyDays).Truncate(24 * time.Hour).UnixMilli()
	dailyRows, err := database.QueryContext(ctx, `
		SELECT
			strftime('%Y-%m-%d', ts / 1000, 'unixepoch') AS day,
			COUNT(*),
			SUM(CASE WHEN o.name LIKE '%local_hit%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%fallback_hit%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%miss%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN l.status >= 400 THEN 1 ELSE 0 END),
			SUM(l.cache_ms + l.upstream_ms)
		FROM request_log l
		LEFT JOIN outcomes o ON o.id = l.outcome_id
		WHERE l.ts >= ?
		GROUP BY day
		ORDER BY day ASC`, dailyCutoffMS)
	if err != nil {
		return nil, fmt.Errorf("query daily activity: %w", err)
	}
	defer dailyRows.Close()

	for dailyRows.Next() {
		var (
			d                                DailyStat
			sumLat                           sql.NullInt64
			dLocal, dFb, dMiss, dErr, dCount sql.NullInt64
		)
		if err := dailyRows.Scan(&d.Date, &dCount, &dLocal, &dFb, &dMiss, &dErr, &sumLat); err != nil {
			return nil, fmt.Errorf("scan daily stat: %w", err)
		}
		d.Requests = dCount.Int64
		d.LocalHits = dLocal.Int64
		d.FallbackHits = dFb.Int64
		d.Misses = dMiss.Int64
		d.Errors = dErr.Int64
		if d.Requests > 0 {
			d.AvgLatencyMs = float64(sumLat.Int64) / float64(d.Requests)
		}
		report.Daily = append(report.Daily, d)
	}
	if err := dailyRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily stats: %w", err)
	}

	// 5. Top Endpoints
	epRows, err := database.QueryContext(ctx, `
		SELECT
			e.name,
			COUNT(*) AS cnt,
			SUM(CASE WHEN o.name LIKE '%local_hit%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN o.name LIKE '%miss%' THEN 1 ELSE 0 END),
			SUM(l.cache_ms + l.upstream_ms)
		FROM request_log l
		JOIN endpoints e ON e.id = l.endpoint_id
		LEFT JOIN outcomes o ON o.id = l.outcome_id
		GROUP BY e.name
		ORDER BY cnt DESC
		LIMIT ?`, topLimit)
	if err != nil {
		return nil, fmt.Errorf("query top endpoints: %w", err)
	}
	defer epRows.Close()

	for epRows.Next() {
		var (
			ep             EndpointStat
			cnt, loc, miss sql.NullInt64
			sumLat         sql.NullInt64
		)
		if err := epRows.Scan(&ep.Endpoint, &cnt, &loc, &miss, &sumLat); err != nil {
			return nil, fmt.Errorf("scan top endpoint: %w", err)
		}
		ep.Requests = cnt.Int64
		ep.SharePct = calcPct(ep.Requests, totalCount)
		if ep.Requests > 0 {
			ep.LocalHitPct = calcPct(loc.Int64, ep.Requests)
			ep.MissPct = calcPct(miss.Int64, ep.Requests)
			ep.AvgLatencyMs = float64(sumLat.Int64) / float64(ep.Requests)
		}
		report.Endpoints = append(report.Endpoints, ep)
	}
	if err := epRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top endpoints: %w", err)
	}

	// 6. Outcomes Breakdown
	outcomeRows, err := database.QueryContext(ctx, `
		SELECT
			o.name,
			COUNT(*) AS cnt
		FROM request_log l
		JOIN outcomes o ON o.id = l.outcome_id
		GROUP BY o.name
		ORDER BY cnt DESC`)
	if err != nil {
		return nil, fmt.Errorf("query outcomes breakdown: %w", err)
	}
	defer outcomeRows.Close()

	for outcomeRows.Next() {
		var o OutcomeStat
		if err := outcomeRows.Scan(&o.Outcome, &o.Requests); err != nil {
			return nil, fmt.Errorf("scan outcome stat: %w", err)
		}
		o.SharePct = calcPct(o.Requests, totalCount)
		report.Outcomes = append(report.Outcomes, o)
	}
	if err := outcomeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outcomes: %w", err)
	}

	// 7. Status Codes Breakdown
	statusRows, err := database.QueryContext(ctx, `
		SELECT
			l.status,
			COUNT(*) AS cnt
		FROM request_log l
		GROUP BY l.status
		ORDER BY cnt DESC`)
	if err != nil {
		return nil, fmt.Errorf("query status breakdown: %w", err)
	}
	defer statusRows.Close()

	for statusRows.Next() {
		var s StatusCodeStat
		if err := statusRows.Scan(&s.Status, &s.Requests); err != nil {
			return nil, fmt.Errorf("scan status code stat: %w", err)
		}
		s.SharePct = calcPct(s.Requests, totalCount)
		report.StatusCodes = append(report.StatusCodes, s)
	}
	if err := statusRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate status codes: %w", err)
	}

	// 8. Top User-Agents
	uaRows, err := database.QueryContext(ctx, `
		SELECT
			user_agent,
			COUNT(*) AS cnt
		FROM request_log
		WHERE user_agent != ''
		GROUP BY user_agent
		ORDER BY cnt DESC
		LIMIT ?`, topLimit)
	if err != nil {
		return nil, fmt.Errorf("query top user agents: %w", err)
	}
	defer uaRows.Close()

	for uaRows.Next() {
		var ua UserAgentStat
		if err := uaRows.Scan(&ua.UserAgent, &ua.Requests); err != nil {
			return nil, fmt.Errorf("scan user agent stat: %w", err)
		}
		ua.SharePct = calcPct(ua.Requests, totalCount)
		report.UserAgents = append(report.UserAgents, ua)
	}
	if err := uaRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user agents: %w", err)
	}

	return report, nil
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

func queryPercentile(ctx context.Context, db *sql.DB, totalCount int64, p float64) int64 {
	if totalCount <= 0 {
		return 0
	}
	offset := int64(float64(totalCount-1) * p)
	if offset < 0 {
		offset = 0
	}
	var latency sql.NullInt64
	query := fmt.Sprintf("SELECT (cache_ms + upstream_ms) FROM request_log ORDER BY (cache_ms + upstream_ms) ASC LIMIT 1 OFFSET %d", offset)
	_ = db.QueryRowContext(ctx, query).Scan(&latency)
	return latency.Int64
}

// SortDaily sorts DailyStat records chronologically.
func SortDaily(daily []DailyStat) {
	sort.Slice(daily, func(i, j int) bool {
		return daily[i].Date < daily[j].Date
	})
}
