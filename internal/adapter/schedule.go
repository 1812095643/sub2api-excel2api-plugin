package adapter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc/metadata"
)

type cronExpression struct {
	minute [60]bool
	hour   [24]bool
	dom    [32]bool
	month  [13]bool
	dow    [7]bool
	domAny bool
	dowAny bool
}

func parseCronExpression(raw string) (*cronExpression, error) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron 表达式必须包含 5 段：分钟 小时 日期 月份 星期")
	}
	result := &cronExpression{}
	var err error
	var values []bool
	if values, _, err = parseCronField(parts[0], 0, 59, false); err != nil {
		return nil, fmt.Errorf("cron 分钟段无效：%w", err)
	}
	copy(result.minute[:], values)
	if values, _, err = parseCronField(parts[1], 0, 23, false); err != nil {
		return nil, fmt.Errorf("cron 小时段无效：%w", err)
	}
	copy(result.hour[:], values)
	if values, result.domAny, err = parseCronField(parts[2], 1, 31, false); err != nil {
		return nil, fmt.Errorf("cron 日期段无效：%w", err)
	}
	copy(result.dom[:], values)
	if values, _, err = parseCronField(parts[3], 1, 12, false); err != nil {
		return nil, fmt.Errorf("cron 月份段无效：%w", err)
	}
	copy(result.month[:], values)
	var dow []bool
	if dow, result.dowAny, err = parseCronField(parts[4], 0, 7, true); err != nil {
		return nil, fmt.Errorf("cron 星期段无效：%w", err)
	}
	for index := 0; index < 7; index++ {
		result.dow[index] = dow[index] || (index == 0 && dow[7])
	}
	return result, nil
}

func parseCronField(raw string, min, max int, sundaySeven bool) ([]bool, bool, error) {
	values := make([]bool, max+1)
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) == 0 || raw == "" {
		return values, false, fmt.Errorf("不能为空")
	}
	any := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return values, false, fmt.Errorf("含空项")
		}
		base := part
		step := 1
		if strings.Contains(part, "/") {
			pieces := strings.Split(part, "/")
			if len(pieces) != 2 {
				return values, false, fmt.Errorf("步长格式无效")
			}
			base = pieces[0]
			parsed, parseErr := strconv.Atoi(pieces[1])
			if parseErr != nil || parsed <= 0 {
				return values, false, fmt.Errorf("步长无效")
			}
			step = parsed
		}
		start, end := min, max
		switch {
		case base == "*" || base == "":
		case strings.Contains(base, "-"):
			pieces := strings.Split(base, "-")
			if len(pieces) != 2 {
				return values, false, fmt.Errorf("范围格式无效")
			}
			var parseErr error
			start, parseErr = strconv.Atoi(pieces[0])
			if parseErr != nil {
				return values, false, fmt.Errorf("范围起点无效")
			}
			end, parseErr = strconv.Atoi(pieces[1])
			if parseErr != nil {
				return values, false, fmt.Errorf("范围终点无效")
			}
		default:
			parsed, parseErr := strconv.Atoi(base)
			if parseErr != nil {
				return values, false, fmt.Errorf("数值无效")
			}
			start, end = parsed, parsed
		}
		if start < min || end > max || start > end {
			return values, false, fmt.Errorf("数值超出范围")
		}
		for value := start; value <= end; value += step {
			normalized := value
			if sundaySeven && normalized == 7 {
				normalized = 0
			}
			values[normalized] = true
			any = true
		}
	}
	return values, !any || strings.Contains(raw, "*"), nil
}

func (c *cronExpression) matches(now time.Time) bool {
	if c == nil || !c.minute[now.Minute()] || !c.hour[now.Hour()] || !c.month[int(now.Month())] {
		return false
	}
	dayOfMonth := c.dom[now.Day()]
	dayOfWeek := c.dow[int(now.Weekday())]
	if c.domAny && c.dowAny {
		return true
	}
	if c.domAny {
		return dayOfWeek
	}
	if c.dowAny {
		return dayOfMonth
	}
	return dayOfMonth || dayOfWeek
}

func (s *Server) configureScheduler(config *healthConfig) {
	s.scheduleMu.Lock()
	if s.scheduleCancel != nil {
		s.scheduleCancel()
		s.scheduleCancel = nil
	}
	if config == nil || !config.DiagnosticEnabled || config.ScheduleMode == "disabled" || len(config.DiagnosticModels) == 0 {
		s.scheduleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.scheduleCancel = cancel
	s.scheduleMu.Unlock()
	go s.schedulerLoop(ctx, config)
}

func (s *Server) schedulerLoop(ctx context.Context, config *healthConfig) {
	var cron *cronExpression
	if config.ScheduleMode == "cron" {
		cron, _ = parseCronExpression(config.ScheduleCron)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	lastMinute := ""
	conditionActive := false
	onStartPending := config.ScheduleMode == "condition" && config.ScheduleCondition == "on_start"
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			config = s.config.Load()
			if ctx.Err() != nil || config == nil || !config.DiagnosticEnabled || config.ScheduleMode == "disabled" {
				return
			}
			if config.ScheduleMode == "cron" {
				minute := now.Format("2006-01-02 15:04")
				if minute != lastMinute && cron.matches(now) {
					lastMinute = minute
					_, _ = s.startDiagnostics(ctx, config)
				}
				continue
			}
			if onStartPending {
				if started, _ := s.startDiagnostics(ctx, config); started {
					onStartPending = false
				}
				continue
			}
			if config.ScheduleCondition == "has_schedulable_accounts" {
				available := s.hasSchedulableAccounts(ctx)
				if available && !conditionActive {
					conditionActive, _ = s.startDiagnostics(ctx, config)
				} else if !available {
					conditionActive = false
				}
			}
		}
	}
}

func (s *Server) hasSchedulableAccounts(ctx context.Context) bool {
	s.hostMu.Lock()
	host := s.host
	s.hostMu.Unlock()
	if host == nil {
		return false
	}
	listCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("sub2api-schedulable-only", "true"))
	response, err := host.ListAccounts(listCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	return err == nil && len(response.GetAccountIds()) > 0
}
