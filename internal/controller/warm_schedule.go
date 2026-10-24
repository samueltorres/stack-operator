/*
Copyright 2026.

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

package controller

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// warmTarget is the number of warm Stacks a pool aims for.
type warmTarget struct {
	warm int32
	// schedule names the warm schedule that sets warm. It is empty when
	// spec.warm applies.
	schedule string
	// until is when the target next changes. It is zero when it never does.
	until time.Time
}

// window is one occurrence of a warm schedule. It is open from start until,
// but not including, end.
type window struct {
	schedule   *platformv1.WarmSchedule
	start, end time.Time
}

// resolveWarm works out a pool's warm target at t, and when it next changes.
// When several windows are open at once, the lowest warm wins.
func resolveWarm(spec *platformv1.StackPoolSpec, t time.Time) (warmTarget, error) {
	if len(spec.WarmSchedules) == 0 {
		return warmTarget{warm: spec.Warm}, nil
	}
	loc, err := scheduleLocation(spec.TimeZone)
	if err != nil {
		return warmTarget{warm: spec.Warm}, err
	}
	t = t.In(loc)

	var windows []window
	for i := range spec.WarmSchedules {
		w, err := occurrences(&spec.WarmSchedules[i], t)
		if err != nil {
			return warmTarget{warm: spec.Warm}, err
		}
		windows = append(windows, w...)
	}

	// Schedules repeat every week, so a target that holds for a week holds
	// for good.
	target := targetAt(spec.Warm, windows, t)
	horizon := t.AddDate(0, 0, 7)
	edges := make([]time.Time, 0, 2*len(windows))
	for _, w := range windows {
		edges = append(edges, w.start, w.end)
	}
	slices.SortFunc(edges, time.Time.Compare)
	for _, edge := range edges {
		if !edge.After(t) || edge.After(horizon) {
			continue
		}
		if next := targetAt(spec.Warm, windows, edge); next.warm != target.warm || next.schedule != target.schedule {
			target.until = edge
			break
		}
	}
	return target, nil
}

// targetAt is the warm target at t, given every window that may be open then.
func targetAt(warm int32, windows []window, t time.Time) warmTarget {
	target := warmTarget{warm: warm}
	for _, w := range windows {
		open := !t.Before(w.start) && t.Before(w.end)
		if open && (target.schedule == "" || w.schedule.Warm < target.warm) {
			target.warm, target.schedule = w.schedule.Warm, w.schedule.Name
		}
	}
	return target
}

// occurrences returns the windows of a schedule that start between a week
// before t and a week after it. A window lasts a week at most, so these cover
// every instant of the week starting at t.
func occurrences(s *platformv1.WarmSchedule, t time.Time) ([]window, error) {
	start, err := minuteOfDay(cmp.Or(s.Start, "00:00"))
	if err != nil {
		return nil, fmt.Errorf("warm schedule %s: %w", s.Name, err)
	}
	duration := cmp.Or(s.Duration.Duration, 24*time.Hour)
	if duration <= 0 || duration > 7*24*time.Hour {
		return nil, fmt.Errorf("warm schedule %s: duration %s is not between 0 and a week", s.Name, duration)
	}

	// time.Date normalizes the minutes and nanoseconds on the wall clock, so
	// windows keep their local times across daylight saving changes.
	year, month, day := t.Date()
	var out []window
	for offset := -7; offset <= 7; offset++ {
		date := time.Date(year, month, day+offset, 0, 0, 0, 0, t.Location())
		// time.Weekday names match the Weekday enum.
		if !slices.Contains(s.Days, platformv1.Weekday(date.Weekday().String())) {
			continue
		}
		out = append(out, window{
			schedule: s,
			start:    time.Date(year, month, day+offset, 0, start, 0, 0, t.Location()),
			end:      time.Date(year, month, day+offset, 0, start, 0, int(duration), t.Location()),
		})
	}
	return out, nil
}

// minuteOfDay parses an HH:MM time of day into minutes since midnight.
func minuteOfDay(s string) (int, error) {
	tod, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("invalid time of day %q", s)
	}
	return tod.Hour()*60 + tod.Minute(), nil
}

// scheduleLocation returns the time zone warm schedules are evaluated in.
func scheduleLocation(timeZone *string) (*time.Location, error) {
	if timeZone == nil {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(*timeZone)
	if err != nil {
		return nil, fmt.Errorf("loading time zone %q: %w", *timeZone, err)
	}
	return loc, nil
}
