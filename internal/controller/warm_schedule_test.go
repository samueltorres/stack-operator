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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

var _ = Describe("resolveWarm", func() {
	// Daylight saving time ends in Lisbon on Sunday 2026-10-25, at 02:00 WET.
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		panic(err)
	}
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, time.October, day, hour, minute, 0, 0, lisbon)
	}

	weekend := platformv1.WarmSchedule{
		Name: "weekend", Days: []platformv1.Weekday{platformv1.Saturday, platformv1.Sunday},
		Start: "00:00", End: "24:00", Warm: 1,
	}
	nights := platformv1.WarmSchedule{
		Name: "nights", Days: []platformv1.Weekday{
			platformv1.Monday, platformv1.Tuesday, platformv1.Wednesday, platformv1.Thursday, platformv1.Friday,
		},
		Start: "20:00", End: "07:00", Warm: 2,
	}
	everyDay := platformv1.WarmSchedule{
		Name: "always", Days: []platformv1.Weekday{
			platformv1.Monday, platformv1.Tuesday, platformv1.Wednesday, platformv1.Thursday,
			platformv1.Friday, platformv1.Saturday, platformv1.Sunday,
		},
		Warm: 4,
	}
	spec := func(timeZone string, schedules ...platformv1.WarmSchedule) *platformv1.StackPoolSpec {
		s := &platformv1.StackPoolSpec{Warm: 5, Max: 10, WarmSchedules: schedules}
		if timeZone != "" {
			s.TimeZone = &timeZone
		}
		return s
	}

	DescribeTable("targets",
		func(spec *platformv1.StackPoolSpec, t time.Time, warm int32, schedule string, until time.Time) {
			target, err := resolveWarm(spec, t)
			Expect(err).NotTo(HaveOccurred())
			Expect(target.warm).To(Equal(warm), "warm")
			Expect(target.schedule).To(Equal(schedule), "schedule")
			Expect(target.until).To(BeTemporally("==", until), "until")
		},
		Entry("without schedules, spec.warm holds for good",
			spec(""), at(24, 12, 0), int32(5), "", time.Time{}),
		Entry("outside every window, spec.warm holds until the next one opens",
			spec("Europe/Lisbon", weekend), at(23, 10, 0), int32(5), "", at(24, 0, 0)),
		Entry("an open window holds until the target changes, across consecutive days and daylight saving",
			spec("Europe/Lisbon", weekend), at(24, 12, 0), int32(1), "weekend", at(26, 0, 0)),
		Entry("a window opens on its start",
			spec("Europe/Lisbon", weekend), at(24, 0, 0), int32(1), "weekend", at(26, 0, 0)),
		Entry("a window closes on its end",
			spec("Europe/Lisbon", weekend), at(26, 0, 0), int32(5), "", at(31, 0, 0)),
		Entry("an overnight window runs into the next day",
			spec("Europe/Lisbon", nights), at(24, 3, 0), int32(2), "nights", at(24, 7, 0)),
		Entry("overlapping windows take the lowest warm",
			spec("Europe/Lisbon", nights, weekend), at(24, 3, 0), int32(1), "weekend", at(26, 0, 0)),
		Entry("the next window to open may be a higher one",
			spec("Europe/Lisbon", nights, weekend), at(26, 0, 0), int32(5), "", at(26, 20, 0)),
		Entry("a window open all week holds for good",
			spec("Europe/Lisbon", everyDay), at(24, 12, 0), int32(4), "always", time.Time{}),
		Entry("schedules default to UTC",
			spec("", weekend), at(24, 0, 30), int32(5), "", time.Date(2026, time.October, 24, 0, 0, 0, 0, time.UTC)),
	)

	It("falls back to spec.warm when the time zone is unknown", func() {
		target, err := resolveWarm(spec("Mars/Olympus_Mons", weekend), at(24, 12, 0))
		Expect(err).To(MatchError(ContainSubstring("Mars/Olympus_Mons")))
		Expect(target).To(Equal(warmTarget{warm: 5}))
	})
})

var _ = Describe("soonest", func() {
	DescribeTable("picks the shortest positive delay",
		func(delays []time.Duration, want time.Duration) {
			Expect(soonest(delays...)).To(Equal(want))
		},
		Entry("none", nil, time.Duration(0)),
		Entry("only zeros", []time.Duration{0, 0}, time.Duration(0)),
		Entry("zeros are ignored", []time.Duration{0, time.Hour, time.Minute}, time.Minute),
	)
})
