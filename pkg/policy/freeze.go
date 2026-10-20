package policy

import (
	"fmt"
	"strings"
	"time"
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func (w *FreezeWindow) init() error {
	if w.Name == "" {
		return fmt.Errorf("name is required")
	}
	if w.Action != Hold && w.Action != Deny {
		return fmt.Errorf("action must be hold or deny")
	}
	tz := w.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("timeZone: %w", err)
	}
	w.loc = loc

	oneOff := w.From != nil || w.Until != nil
	weekly := len(w.Days) > 0 || w.Start != "" || w.End != ""
	switch {
	case oneOff && weekly:
		return fmt.Errorf("use either from/until or days/start/end, not both")
	case oneOff:
		if w.From == nil || w.Until == nil || !w.Until.After(w.From.Time) {
			return fmt.Errorf("from and until are both required and until must be after from")
		}
	case weekly:
		for _, d := range w.Days {
			if _, ok := weekdays[strings.ToLower(d)[:min(3, len(d))]]; !ok {
				return fmt.Errorf("unknown day %q", d)
			}
		}
		if _, err := clock(w.Start); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		if _, err := clock(w.End); err != nil {
			return fmt.Errorf("end: %w", err)
		}
	default:
		return fmt.Errorf("set from/until or days/start/end")
	}
	return nil
}

// Active reports whether the window covers t for a change in namespace ns.
// Weekly windows whose end is before their start wrap past midnight.
func (w *FreezeWindow) Active(t time.Time, ns string) bool {
	if len(w.Namespaces) > 0 && !GlobAny(w.Namespaces, ns) {
		return false
	}
	if w.From != nil {
		return !t.Before(w.From.Time) && t.Before(w.Until.Time)
	}
	local := t.In(w.loc)
	start, _ := clock(w.Start)
	end, _ := clock(w.End)
	now := local.Hour()*60 + local.Minute()
	day := local.Weekday()
	if start <= end {
		return w.onDay(day) && now >= start && now < end
	}
	// Overnight window, e.g. Fri 22:00 -> 06:00 belongs to Friday.
	if now >= start {
		return w.onDay(day)
	}
	return now < end && w.onDay((day+6)%7)
}

func (w *FreezeWindow) onDay(d time.Weekday) bool {
	if len(w.Days) == 0 {
		return true
	}
	for _, s := range w.Days {
		if weekdays[strings.ToLower(s)[:min(3, len(s))]] == d {
			return true
		}
	}
	return false
}

// clock parses "HH:MM" into minutes after midnight. "24:00" is allowed as an end.
func clock(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("required, as HH:MM")
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		if s == "24:00" {
			return 24 * 60, nil
		}
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}
