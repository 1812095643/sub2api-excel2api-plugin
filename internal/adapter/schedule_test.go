package adapter

import (
	"testing"
	"time"
)

func TestCronEveryThirtyMinutes(t *testing.T) {
	expression, err := parseCronExpression("*/30 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !expression.matches(time.Date(2026, 9, 24, 10, 0, 0, 0, time.Local)) {
		t.Fatal("10:00 should match")
	}
	if !expression.matches(time.Date(2026, 9, 24, 10, 30, 0, 0, time.Local)) {
		t.Fatal("10:30 should match")
	}
	if expression.matches(time.Date(2026, 9, 24, 10, 15, 0, 0, time.Local)) {
		t.Fatal("10:15 should not match")
	}
}

func TestCronConditionDayOfMonthOrDayOfWeek(t *testing.T) {
	expression, err := parseCronExpression("0 0 1 * 5")
	if err != nil {
		t.Fatal(err)
	}
	if !expression.matches(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)) {
		t.Fatal("day of month should match")
	}
	if !expression.matches(time.Date(2026, 10, 2, 0, 0, 0, 0, time.Local)) {
		t.Fatal("day of week should match")
	}
}
