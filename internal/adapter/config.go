package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"local.sub2api/openai-health/internal/modeltrace"
)

const (
	maxDiagnosticModels      = 32
	maxDiagnosticAccounts    = 128
	maxTimezoneOverrides     = 256
	maxDiagnosticConcurrency = 16
)

var allowedRequestTimezones = map[string]struct{}{
	"Africa/Cairo": {}, "Africa/Johannesburg": {},
	"America/Argentina/Buenos_Aires": {}, "America/Chicago": {}, "America/Denver": {},
	"America/Los_Angeles": {}, "America/Mexico_City": {}, "America/New_York": {},
	"America/Sao_Paulo": {}, "America/Toronto": {}, "America/Vancouver": {},
	"Asia/Bangkok": {}, "Asia/Dubai": {}, "Asia/Ho_Chi_Minh": {}, "Asia/Jakarta": {},
	"Asia/Kolkata": {}, "Asia/Kuala_Lumpur": {}, "Asia/Manila": {}, "Asia/Seoul": {},
	"Asia/Singapore": {}, "Asia/Tokyo": {}, "Australia/Perth": {}, "Australia/Sydney": {},
	"Europe/Berlin": {}, "Europe/Istanbul": {}, "Europe/London": {}, "Europe/Moscow": {},
	"Europe/Paris": {}, "Pacific/Auckland": {}, "Pacific/Honolulu": {},
}

type healthConfig struct {
	DefaultTimezone       string            `json:"default_timezone"`
	AccountTimezones      map[string]string `json:"account_timezones"`
	DiagnosticEnabled     bool              `json:"diagnostic_enabled"`
	DirectEnabled         bool              `json:"direct_enabled"`
	DirectAccountIDs      []int64           `json:"direct_account_ids"`
	DiagnosticModels      []string          `json:"diagnostic_models"`
	DiagnosticAccountIDs  []int64           `json:"diagnostic_account_ids"`
	DiagnosticSchedulable bool              `json:"diagnostic_schedulable_only"`
	DiagnosticConcurrency int               `json:"diagnostic_concurrency"`
	ScheduleMode          string            `json:"schedule_mode"`
	ScheduleCron          string            `json:"schedule_cron"`
	ScheduleCondition     string            `json:"schedule_condition"`
}

func defaultHealthConfig() *healthConfig {
	return &healthConfig{
		DefaultTimezone:  "Asia/Singapore",
		AccountTimezones: map[string]string{},
		// 旧版配置没有总开关，缺省开启以保持升级前的检测行为。
		DiagnosticEnabled:     true,
		DirectAccountIDs:      []int64{},
		DiagnosticModels:      []string{},
		DiagnosticAccountIDs:  []int64{},
		DiagnosticConcurrency: 4,
		ScheduleMode:          "disabled",
		ScheduleCondition:     "has_schedulable_accounts",
	}
}

func validTimezone(name string) bool {
	if _, ok := allowedRequestTimezones[name]; !ok {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func validModelName(name string) bool {
	return name != "" && len(name) <= 256 && strings.IndexFunc(name, func(r rune) bool {
		return r == '*' || r == '\\' || r == '/' || r == '\n' || r == '\r' || r == '\t' || r < 0x20
	}) < 0
}

func parseHealthConfig(raw []byte) ([]byte, *healthConfig, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var fields map[string]json.RawMessage
	if len(raw) > 1<<20 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, nil, errors.New("配置必须是大小受限的 JSON 对象")
	}
	config := defaultHealthConfig()
	if value, ok := fields["diagnostic_enabled"]; ok {
		value = bytes.TrimSpace(value)
		if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return nil, nil, errors.New("启用测试必须是布尔值")
		}
		config.DiagnosticEnabled = bytes.Equal(value, []byte("true"))
		delete(fields, "diagnostic_enabled")
	}
	if value, ok := fields["default_timezone"]; ok {
		if err := json.Unmarshal(value, &config.DefaultTimezone); err != nil || !validTimezone(config.DefaultTimezone) {
			return nil, nil, errors.New("default_timezone 必须是允许的 IANA 时区")
		}
		delete(fields, "default_timezone")
	}
	if value, ok := fields["account_timezones"]; ok {
		if err := json.Unmarshal(value, &config.AccountTimezones); err != nil || config.AccountTimezones == nil {
			return nil, nil, errors.New("account_timezones 必须是对象")
		}
		if len(config.AccountTimezones) > maxTimezoneOverrides {
			return nil, nil, fmt.Errorf("最多配置 %d 个账号时区", maxTimezoneOverrides)
		}
		for accountID, timezoneName := range config.AccountTimezones {
			id, err := strconv.ParseInt(strings.TrimSpace(accountID), 10, 64)
			if err != nil || id <= 0 || !validTimezone(timezoneName) {
				return nil, nil, fmt.Errorf("账号时区配置 %q 无效", accountID)
			}
		}
		delete(fields, "account_timezones")
	}
	if value, ok := fields["diagnostic_models"]; ok {
		if err := json.Unmarshal(value, &config.DiagnosticModels); err != nil || len(config.DiagnosticModels) > maxDiagnosticModels {
			return nil, nil, fmt.Errorf("diagnostic_models 最多配置 %d 个模型", maxDiagnosticModels)
		}
		seen := map[string]bool{}
		knownModels := map[string]bool{}
		if known, modelErr := modeltrace.ModelTraceGPTModels(); modelErr == nil {
			for _, model := range known {
				knownModels[model] = true
			}
		}
		for index, model := range config.DiagnosticModels {
			config.DiagnosticModels[index] = strings.TrimSpace(model)
			if !validModelName(config.DiagnosticModels[index]) || !knownModels[config.DiagnosticModels[index]] || seen[config.DiagnosticModels[index]] {
				return nil, nil, fmt.Errorf("第 %d 个检测模型无效或重复", index+1)
			}
			seen[config.DiagnosticModels[index]] = true
		}
		delete(fields, "diagnostic_models")
	}
	if value, ok := fields["diagnostic_account_ids"]; ok {
		if err := json.Unmarshal(value, &config.DiagnosticAccountIDs); err != nil || len(config.DiagnosticAccountIDs) > maxDiagnosticAccounts {
			return nil, nil, fmt.Errorf("diagnostic_account_ids 最多配置 %d 个账号", maxDiagnosticAccounts)
		}
		seen := map[int64]bool{}
		for index, id := range config.DiagnosticAccountIDs {
			if id <= 0 || seen[id] {
				return nil, nil, fmt.Errorf("第 %d 个检测账号 ID 无效或重复", index+1)
			}
			seen[id] = true
		}
		delete(fields, "diagnostic_account_ids")
	}
	if value, ok := fields["direct_enabled"]; ok {
		value = bytes.TrimSpace(value)
		if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return nil, nil, errors.New("Excel2API 开关必须是布尔值")
		}
		config.DirectEnabled = bytes.Equal(value, []byte("true"))
		delete(fields, "direct_enabled")
	}
	if value, ok := fields["direct_account_ids"]; ok {
		if err := json.Unmarshal(value, &config.DirectAccountIDs); err != nil || len(config.DirectAccountIDs) > maxDiagnosticAccounts {
			return nil, nil, fmt.Errorf("direct_account_ids 最多配置 %d 个账号", maxDiagnosticAccounts)
		}
		seen := map[int64]bool{}
		for index, id := range config.DirectAccountIDs {
			if id <= 0 || seen[id] {
				return nil, nil, fmt.Errorf("第 %d 个 Excel2API 账号 ID 无效或重复", index+1)
			}
			seen[id] = true
		}
		delete(fields, "direct_account_ids")
	}
	if config.DirectEnabled && len(config.DirectAccountIDs) == 0 {
		return nil, nil, errors.New("开启 Excel2API 前，请至少填写一个 Sub2API 账号 ID；留空不会应用到全部账号")
	}
	if value, ok := fields["diagnostic_schedulable_only"]; ok {
		if err := json.Unmarshal(value, &config.DiagnosticSchedulable); err != nil {
			return nil, nil, errors.New("diagnostic_schedulable_only 必须是布尔值")
		}
		delete(fields, "diagnostic_schedulable_only")
	}
	if value, ok := fields["diagnostic_concurrency"]; ok {
		if err := json.Unmarshal(value, &config.DiagnosticConcurrency); err != nil || config.DiagnosticConcurrency < 1 || config.DiagnosticConcurrency > maxDiagnosticConcurrency {
			return nil, nil, fmt.Errorf("diagnostic_concurrency 必须在 1 到 %d 之间", maxDiagnosticConcurrency)
		}
		delete(fields, "diagnostic_concurrency")
	}
	if value, ok := fields["schedule_mode"]; ok {
		if err := json.Unmarshal(value, &config.ScheduleMode); err != nil || (config.ScheduleMode != "disabled" && config.ScheduleMode != "cron" && config.ScheduleMode != "condition") {
			return nil, nil, errors.New("schedule_mode 必须是 disabled、cron 或 condition")
		}
		delete(fields, "schedule_mode")
	}
	if value, ok := fields["schedule_cron"]; ok {
		if err := json.Unmarshal(value, &config.ScheduleCron); err != nil || len(config.ScheduleCron) > 128 {
			return nil, nil, errors.New("schedule_cron 必须是长度不超过 128 的五段 cron 表达式")
		}
		config.ScheduleCron = strings.TrimSpace(config.ScheduleCron)
		delete(fields, "schedule_cron")
	}
	if value, ok := fields["schedule_condition"]; ok {
		if err := json.Unmarshal(value, &config.ScheduleCondition); err != nil || (config.ScheduleCondition != "on_start" && config.ScheduleCondition != "has_schedulable_accounts") {
			return nil, nil, errors.New("schedule_condition 不受支持")
		}
		delete(fields, "schedule_condition")
	}
	if config.DiagnosticEnabled && config.ScheduleMode == "cron" {
		if config.ScheduleCron == "" {
			return nil, nil, errors.New("cron 定时模式必须填写 schedule_cron")
		}
		if _, err := parseCronExpression(config.ScheduleCron); err != nil {
			return nil, nil, err
		}
	}
	if config.DiagnosticEnabled && config.ScheduleMode != "disabled" && len(config.DiagnosticModels) == 0 {
		return nil, nil, errors.New("启用定时或条件任务前，至少配置一个检测模型")
	}
	base, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	return base, config, nil
}

func joinHealthConfig(base []byte, config *healthConfig) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(base, &fields) != nil || fields == nil {
		return nil, errors.New("原始传输程序未返回有效配置")
	}
	fields["default_timezone"], _ = json.Marshal(config.DefaultTimezone)
	fields["account_timezones"], _ = json.Marshal(config.AccountTimezones)
	fields["diagnostic_enabled"], _ = json.Marshal(config.DiagnosticEnabled)
	fields["direct_enabled"], _ = json.Marshal(config.DirectEnabled)
	fields["direct_account_ids"], _ = json.Marshal(config.DirectAccountIDs)
	fields["diagnostic_models"], _ = json.Marshal(config.DiagnosticModels)
	fields["diagnostic_account_ids"], _ = json.Marshal(config.DiagnosticAccountIDs)
	fields["diagnostic_schedulable_only"], _ = json.Marshal(config.DiagnosticSchedulable)
	fields["diagnostic_concurrency"], _ = json.Marshal(config.DiagnosticConcurrency)
	fields["schedule_mode"], _ = json.Marshal(config.ScheduleMode)
	fields["schedule_cron"], _ = json.Marshal(config.ScheduleCron)
	fields["schedule_condition"], _ = json.Marshal(config.ScheduleCondition)
	return json.Marshal(fields)
}

func (c *healthConfig) timezoneForAccount(accountID int64) string {
	if c != nil {
		if value := c.AccountTimezones[strconv.FormatInt(accountID, 10)]; validTimezone(value) {
			return value
		}
		if validTimezone(c.DefaultTimezone) {
			return c.DefaultTimezone
		}
	}
	return "Asia/Singapore"
}

func configForTest(raw []byte) (*healthConfig, error) {
	_, config, err := parseHealthConfig(bytes.TrimSpace(raw))
	return config, err
}
