package main

import (
	"strings"
	"time"
)

// parseInterval converts a string like "5m", "1h", "30s" to time.Duration.
// Suffix after '|' is stripped (e.g. "24h|daily@09:00" -> "24h").
// Empty or invalid strings return 0, nil (task will be skipped).
func parseInterval(s string) (time.Duration, error) {
	if idx := strings.Index(s, "|"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) == 0 {
		return 0, nil
	}

	switch s[len(s)-1] {
	case 's', 'm', 'h':
		return time.ParseDuration(s)
	default:

		return time.ParseDuration(s + "m")
	}
}

func heartbeatTaskDueAt(t HeartbeatTask, now time.Time) bool {
	if scheduled, ok := previousHeartbeatScheduleAt(t, now); ok {
		if t.CreatedAt != 0 && scheduled.Before(time.UnixMilli(t.CreatedAt)) {
			return false
		}
		if t.LastRunAt != 0 && !time.UnixMilli(t.LastRunAt).Before(scheduled) {
			return false
		}
		return !scheduled.After(now)
	}

	d, err := parseInterval(t.Interval)
	if err != nil || d <= 0 {
		return false
	}
	baseMillis := t.LastRunAt
	if baseMillis == 0 {
		baseMillis = t.CreatedAt
	}
	hasTimeWindow := t.TimeWindowStart != "" || t.TimeWindowEnd != ""
	if baseMillis == 0 {
		if hasTimeWindow {
			return heartbeatWithinTimeWindow(t, now)
		}
		return true
	}
	if now.Sub(time.UnixMilli(baseMillis)) < d {
		return false
	}

	if hasTimeWindow {
		return heartbeatWithinTimeWindow(t, now)
	}

	return true
}

// heartbeatWithinTimeWindow returns true when now falls within the task's
// configured time window. If the window is empty it returns true.
// Format: "HH:MM" in 24-hour clock; start inclusive, end exclusive.
func heartbeatWithinTimeWindow(t HeartbeatTask, now time.Time) bool {
	startH, startM, startOK := parseHeartbeatClock(t.TimeWindowStart)
	endH, endM, endOK := parseHeartbeatClock(t.TimeWindowEnd)

	if !startOK && !endOK {
		return true
	}

	minutes := now.Hour()*60 + now.Minute()

	if startOK && !endOK {
		return minutes >= startH*60+startM
	}

	if !startOK && endOK {
		return minutes < endH*60+endM
	}

	startMin := startH*60 + startM
	endMin := endH*60 + endM

	if startMin < endMin {

		return minutes >= startMin && minutes < endMin
	}

	return minutes >= startMin || minutes < endMin
}

type heartbeatSchedule struct {
	kind     string
	days     []time.Weekday
	month    int
	day      int
	hour     int
	minute   int
	hasRules bool
}

func parseHeartbeatSchedule(interval string) (heartbeatSchedule, bool) {
	_, after, ok0 := strings.Cut(interval, "|")
	if !ok0 {
		return heartbeatSchedule{}, false
	}
	raw := strings.TrimSpace(after)
	if raw == "" {
		return heartbeatSchedule{}, false
	}
	at := "09:00"
	if parts := strings.SplitN(raw, "@", 2); len(parts) == 2 {
		raw = parts[0]
		at = parts[1]
	}
	hour, minute, ok := parseHeartbeatClock(at)
	if !ok {
		return heartbeatSchedule{}, false
	}
	kind := raw
	rule := ""
	if parts := strings.SplitN(raw, ":", 2); len(parts) == 2 {
		kind = parts[0]
		rule = parts[1]
	}
	s := heartbeatSchedule{kind: kind, hour: hour, minute: minute, hasRules: true}
	switch kind {
	case "daily":
		return s, true
	case "weekly", "biweekly":
		for part := range strings.SplitSeq(rule, ",") {
			if wd, ok := parseHeartbeatWeekday(part); ok {
				s.days = append(s.days, wd)
			}
		}
		return s, len(s.days) > 0
	case "monthly":
		s.day = parsePositiveInt(rule, 1)
		return s, true
	case "yearly":
		parts := strings.SplitN(rule, "-", 2)
		s.month = parsePositiveInt(firstString(parts), 1)
		s.day = 1
		if len(parts) == 2 {
			s.day = parsePositiveInt(parts[1], 1)
		}
		if s.month < 1 {
			s.month = 1
		}
		if s.month > 12 {
			s.month = 12
		}
		return s, true
	default:
		return heartbeatSchedule{}, false
	}
}

func previousHeartbeatScheduleAt(t HeartbeatTask, now time.Time) (time.Time, bool) {
	s, ok := parseHeartbeatSchedule(t.Interval)
	if !ok || !s.hasRules {
		return time.Time{}, false
	}
	switch s.kind {
	case "daily":
		candidate := dateAt(now.Year(), now.Month(), now.Day(), s.hour, s.minute, now.Location())
		if candidate.After(now) {
			candidate = candidate.AddDate(0, 0, -1)
		}
		return candidate, true
	case "weekly":
		return previousHeartbeatWeeklyAt(s, now, 7, time.Time{})
	case "biweekly":
		anchor := heartbeatScheduleAnchor(t, now)
		return previousHeartbeatWeeklyAt(s, now, 14, anchor)
	case "monthly":
		return previousHeartbeatMonthlyAt(s, now), true
	case "yearly":
		return previousHeartbeatYearlyAt(s, now), true
	default:
		return time.Time{}, false
	}
}

func previousHeartbeatWeeklyAt(s heartbeatSchedule, now time.Time, windowDays int, anchor time.Time) (time.Time, bool) {
	var best time.Time
	for offset := range windowDays {
		day := now.AddDate(0, 0, -offset)
		for _, wd := range s.days {
			if day.Weekday() != wd {
				continue
			}
			candidate := dateAt(day.Year(), day.Month(), day.Day(), s.hour, s.minute, now.Location())
			if candidate.After(now) {
				continue
			}
			if !anchor.IsZero() && weeksBetween(weekStart(anchor), weekStart(candidate))%2 != 0 {
				continue
			}
			if best.IsZero() || candidate.After(best) {
				best = candidate
			}
		}
	}
	return best, !best.IsZero()
}

func previousHeartbeatMonthlyAt(s heartbeatSchedule, now time.Time) time.Time {
	candidate := monthlyCandidate(now.Year(), now.Month(), s.day, s.hour, s.minute, now.Location())
	if candidate.After(now) {
		prev := now.AddDate(0, -1, 0)
		candidate = monthlyCandidate(prev.Year(), prev.Month(), s.day, s.hour, s.minute, now.Location())
	}
	return candidate
}

func previousHeartbeatYearlyAt(s heartbeatSchedule, now time.Time) time.Time {
	month := time.Month(s.month)
	candidate := monthlyCandidate(now.Year(), month, s.day, s.hour, s.minute, now.Location())
	if candidate.After(now) {
		candidate = monthlyCandidate(now.Year()-1, month, s.day, s.hour, s.minute, now.Location())
	}
	return candidate
}

func heartbeatScheduleAnchor(t HeartbeatTask, now time.Time) time.Time {
	if t.CreatedAt != 0 {
		return time.UnixMilli(t.CreatedAt)
	}
	if t.LastRunAt != 0 {
		return time.UnixMilli(t.LastRunAt)
	}
	return now
}

func parseHeartbeatClock(s string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	hour := parsePositiveInt(parts[0], -1)
	minute := parsePositiveInt(parts[1], -1)
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

func parseHeartbeatWeekday(s string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sun":
		return time.Sunday, true
	case "mon":
		return time.Monday, true
	case "tue":
		return time.Tuesday, true
	case "wed":
		return time.Wednesday, true
	case "thu":
		return time.Thursday, true
	case "fri":
		return time.Friday, true
	case "sat":
		return time.Saturday, true
	default:
		return time.Sunday, false
	}
}

func parsePositiveInt(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func dateAt(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, loc)
}

func monthlyCandidate(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	if day < 1 {
		day = 1
	}
	if max := daysInMonth(year, month, loc); day > max {
		day = max
	}
	return dateAt(year, month, day, hour, minute, loc)
}

func daysInMonth(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

func weekStart(t time.Time) time.Time {
	dayOffset := (int(t.Weekday()) + 6) % 7
	base := dateAt(t.Year(), t.Month(), t.Day(), 0, 0, t.Location())
	return base.AddDate(0, 0, -dayOffset)
}

func weeksBetween(a, b time.Time) int {
	if b.Before(a) {
		a, b = b, a
	}
	return int(b.Sub(a).Hours() / 24 / 7)
}
