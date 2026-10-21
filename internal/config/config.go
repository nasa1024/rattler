package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Feishu         FeishuConfig         `json:"feishu"`
	Schedule       ScheduleConfig       `json:"schedule"`
	DeepSeek       []DeepSeekAccount    `json:"deepseek"`
	OpenRouter     []OpenRouterAccount  `json:"openrouter"`
	XiaomiMiMo     []XiaomiMiMoAccount  `json:"xiaomi_mimo"`
}

type FeishuConfig struct {
	WebhookURL string `json:"webhook_url"`
	Secret     string `json:"secret"`      // 可选：飞书自定义机器人签名校验密钥
	AtAllOnAlert bool `json:"at_all_on_alert"`
}

type ScheduleConfig struct {
	CheckIntervalMinutes int    `json:"check_interval_minutes"` // 巡检间隔（分钟），默认 60
	AlertCooldownMinutes int    `json:"alert_cooldown_minutes"` // 同一告警重复推送冷却时间（分钟），默认 360
	DailyReportTime      string `json:"daily_report_time"`      // 每日定时汇总时间（如 "09:30"），为空则仅在告警或 -force 时推送
}

type DeepSeekAccount struct {
	Name       string  `json:"name"`
	APIKey     string  `json:"api_key"`
	MinBalance float64 `json:"min_balance"` // 低于该余额触发告警（默认 20）
}

type OpenRouterAccount struct {
	Name          string  `json:"name"`
	APIKey        string  `json:"api_key"`
	MinBalanceUSD float64 `json:"min_balance_usd"` // 低于该美元余额触发告警（默认 5）
}

type XiaomiMiMoAccount struct {
	Name           string  `json:"name"`
	Cookie         string  `json:"cookie"`           // platform.xiaomimimo.com 控制台 Cookie（需含 api-platform_serviceToken 和 userId）
	MaxUsedPercent float64 `json:"max_used_percent"` // TokenPlan 已用百分比超过该阈值触发告警（默认 85）
	MinBalanceCNY  float64 `json:"min_balance_cny"`  // 可选：现金余额告警线（默认 0 即不按余额告警）
}

// LoadDotEnv loads key=value pairs from a .env file if it exists.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

// Load loads configuration from a JSON file (if present) and merges environment variable overrides.
func Load(configPath string) (*Config, error) {
	LoadDotEnv(".env")

	cfg := &Config{
		Feishu: FeishuConfig{
			AtAllOnAlert: true,
		},
		Schedule: ScheduleConfig{
			CheckIntervalMinutes: 60,
			AlertCooldownMinutes: 360,
			DailyReportTime:      "09:30",
		},
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		} else {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, err
			}
		}
	}

	applyEnvOverrides(cfg)
	applyDefaults(cfg)

	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("FEISHU_WEBHOOK_URL")); v != "" {
		cfg.Feishu.WebhookURL = v
	}
	if v := strings.TrimSpace(os.Getenv("FEISHU_SECRET")); v != "" {
		cfg.Feishu.Secret = v
	}
	if v := strings.TrimSpace(os.Getenv("FEISHU_AT_ALL_ON_ALERT")); v != "" {
		cfg.Feishu.AtAllOnAlert = v == "1" || strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv("CHECK_INTERVAL_MINUTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Schedule.CheckIntervalMinutes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ALERT_COOLDOWN_MINUTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Schedule.AlertCooldownMinutes = n
		}
	}
	if v, ok := os.LookupEnv("DAILY_REPORT_TIME"); ok {
		cfg.Schedule.DailyReportTime = strings.TrimSpace(v)
	}

	if key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); key != "" && len(cfg.DeepSeek) == 0 {
		minBal := parseFloatEnv("DEEPSEEK_MIN_BALANCE", 20.0)
		cfg.DeepSeek = append(cfg.DeepSeek, DeepSeekAccount{
			Name:       "DeepSeek 默认账号",
			APIKey:     key,
			MinBalance: minBal,
		})
	}

	if key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); key != "" && len(cfg.OpenRouter) == 0 {
		minBal := parseFloatEnv("OPENROUTER_MIN_BALANCE_USD", 5.0)
		cfg.OpenRouter = append(cfg.OpenRouter, OpenRouterAccount{
			Name:          "OpenRouter 默认账号",
			APIKey:        key,
			MinBalanceUSD: minBal,
		})
	}

	if cookie := strings.TrimSpace(os.Getenv("XIAOMI_MIMO_COOKIE")); cookie != "" && len(cfg.XiaomiMiMo) == 0 {
		maxPct := parseFloatEnv("XIAOMI_MIMO_MAX_USED_PERCENT", 85.0)
		minBal := parseFloatEnv("XIAOMI_MIMO_MIN_BALANCE_CNY", 0.0)
		cfg.XiaomiMiMo = append(cfg.XiaomiMiMo, XiaomiMiMoAccount{
			Name:           "小米 MiMo 默认账号",
			Cookie:         cookie,
			MaxUsedPercent: maxPct,
			MinBalanceCNY:  minBal,
		})
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Schedule.CheckIntervalMinutes <= 0 {
		cfg.Schedule.CheckIntervalMinutes = 60
	}
	if cfg.Schedule.AlertCooldownMinutes < 0 {
		cfg.Schedule.AlertCooldownMinutes = 360
	}
	for i := range cfg.DeepSeek {
		if cfg.DeepSeek[i].Name == "" {
			cfg.DeepSeek[i].Name = "DeepSeek #" + strconv.Itoa(i+1)
		}
		if cfg.DeepSeek[i].MinBalance <= 0 {
			cfg.DeepSeek[i].MinBalance = 20.0
		}
	}
	for i := range cfg.OpenRouter {
		if cfg.OpenRouter[i].Name == "" {
			cfg.OpenRouter[i].Name = "OpenRouter #" + strconv.Itoa(i+1)
		}
		if cfg.OpenRouter[i].MinBalanceUSD <= 0 {
			cfg.OpenRouter[i].MinBalanceUSD = 5.0
		}
	}
	for i := range cfg.XiaomiMiMo {
		if cfg.XiaomiMiMo[i].Name == "" {
			cfg.XiaomiMiMo[i].Name = "Xiaomi MiMo #" + strconv.Itoa(i+1)
		}
		if cfg.XiaomiMiMo[i].MaxUsedPercent <= 0 {
			cfg.XiaomiMiMo[i].MaxUsedPercent = 85.0
		}
	}
}

func parseFloatEnv(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}
