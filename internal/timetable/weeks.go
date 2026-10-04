// Package timetable parses timetable CSV files, validates them against a
// term's periods and rooms, and materializes accepted rows into concrete
// recording sessions.
package timetable

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxWeek is the highest 1-based teaching week a week specification may
// reference. It bounds "N-M" ranges so a single cell cannot expand without
// limit.
const MaxWeek = 60

// ExpandWeeks parses a teaching-week specification into sorted, unique,
// 1-based week numbers. A specification is a comma list whose entries are
// either a single week ("7") or an inclusive ascending range ("1-16"); for
// example "1,3,5-9" expands to [1 3 5 6 7 8 9]. Empty entries, zero or
// negative weeks, reversed ranges, non-numeric text and weeks above MaxWeek
// are errors.
func ExpandWeeks(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("weeks: empty specification")
	}
	seen := make(map[int]struct{})
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("weeks: empty entry in %q", spec)
		}
		first, last, err := parseWeekRange(part)
		if err != nil {
			return nil, err
		}
		for week := first; week <= last; week++ {
			seen[week] = struct{}{}
		}
	}
	weeks := make([]int, 0, len(seen))
	for week := range seen {
		weeks = append(weeks, week)
	}
	sort.Ints(weeks)
	return weeks, nil
}

// parseWeekRange parses one comma-separated entry: "N" or "N-M".
func parseWeekRange(part string) (int, int, error) {
	lo, hi, isRange := strings.Cut(part, "-")
	if !isRange {
		week, err := parseWeek(part)
		if err != nil {
			return 0, 0, err
		}
		return week, week, nil
	}
	first, err := parseWeek(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, err
	}
	last, err := parseWeek(strings.TrimSpace(hi))
	if err != nil {
		return 0, 0, err
	}
	if last < first {
		return 0, 0, fmt.Errorf("weeks: reversed range %q", part)
	}
	return first, last, nil
}

// parseWeek parses one week number and checks the 1..MaxWeek bounds.
func parseWeek(text string) (int, error) {
	week, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("weeks: invalid week %q", text)
	}
	if week <= 0 {
		return 0, fmt.Errorf("weeks: week %d is not positive", week)
	}
	if week > MaxWeek {
		return 0, fmt.Errorf("weeks: week %d exceeds the maximum of %d", week, MaxWeek)
	}
	return week, nil
}

// weekMask renders a week list as a bitmask: week N is bit N-1. MaxWeek is 60,
// so a single uint64 always fits.
func weekMask(weeks []int) uint64 {
	var mask uint64
	for _, week := range weeks {
		if week >= 1 && week <= MaxWeek {
			mask |= 1 << uint(week-1)
		}
	}
	return mask
}

// joinInts renders week numbers for error messages.
func joinInts(values []int) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(value)
	}
	return strings.Join(parts, ",")
}
