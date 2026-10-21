package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nasa1024/rattler/internal/config"
)

const mimoBaseURL = "https://platform.xiaomimimo.com/api/v1"

type mimoEnvelope struct {
	Code     any             `json:"code"`
	Message  string          `json:"message"`
	LoginURL string          `json:"loginUrl"`
	Data     json.RawMessage `json:"data"`
}

type mimoUsageData struct {
	LoginURL   string         `json:"loginUrl"`
	Usage      mimoUsageBlock `json:"usage"`
	MonthUsage mimoUsageBlock `json:"monthUsage"`
}

type mimoUsageBlock struct {
	Percent any             `json:"percent"`
	Items   []mimoUsageItem `json:"items"`
}

type mimoUsageItem struct {
	Name    string `json:"name"`
	Used    any    `json:"used"`
	Limit   any    `json:"limit"`
	Percent any    `json:"percent"`
}

type mimoDetailData struct {
	PlanCode         string `json:"planCode"`
	PlanName         string `json:"planName"`
	CurrentPeriodEnd string `json:"currentPeriodEnd"`
	Status           string `json:"status"`
	Expired          any    `json:"expired"`
}

type mimoBalanceData struct {
	Balance  any    `json:"balance"`
	Currency string `json:"currency"`
}

func CheckXiaomiMiMo(ctx context.Context, acc config.XiaomiMiMoAccount) AccountReport {
	report := AccountReport{
		Provider:    "Xiaomi MiMo",
		AccountName: acc.Name,
		Status:      StatusOK,
		TagText:     "套餐正常",
		TagColor:    "green",
		ConsoleURL:  "https://platform.xiaomimimo.com/console/plan-manage",
	}

	cookie := cleanCookie(acc.Cookie)
	if cookie == "" {
		report.Status = StatusAuthExpired
		report.TagText = "缺少凭证"
		report.TagColor = "orange"
		report.Headline = "未配置控制台 Cookie"
		report.AlertReason = "小米 MiMo TokenPlan 需配置 platform.xiaomimimo.com 控制台 Cookie (含 api-platform_serviceToken 与 userId)"
		return report
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	usageEnv, status, err := fetchMimoEndpoint(ctx, client, "/tokenPlan/usage", cookie)
	if err != nil {
		return markError(report, "网络请求失败", err.Error())
	}
	if isMimoAuthExpired(status, usageEnv) {
		report.Status = StatusAuthExpired
		report.TagText = "Cookie 过期"
		report.TagColor = "orange"
		report.Headline = "控制台登录态已过期"
		report.AlertReason = "小米 MiMo 控制台 Cookie 已失效，请打开 platform.xiaomimimo.com 重新复制 Cookie"
		return report
	}
	if code := parseCode(usageEnv.Code); code != 0 {
		return markError(report, fmt.Sprintf("接口返回错误 (code=%d)", code), usageEnv.Message)
	}

	detailEnv, detailStatus, _ := fetchMimoEndpoint(ctx, client, "/tokenPlan/detail", cookie)
	balanceEnv, balanceStatus, _ := fetchMimoEndpoint(ctx, client, "/balance", cookie)

	var detail mimoDetailData
	if detailStatus == http.StatusOK && parseCode(detailEnv.Code) == 0 && len(detailEnv.Data) > 0 {
		_ = json.Unmarshal(detailEnv.Data, &detail)
	}

	var walletBalance *float64
	walletCurrency := "CNY"
	if balanceStatus == http.StatusOK && parseCode(balanceEnv.Code) == 0 && len(balanceEnv.Data) > 0 {
		var b mimoBalanceData
		if json.Unmarshal(balanceEnv.Data, &b) == nil {
			if val, ok := parseNumber(b.Balance); ok {
				walletBalance = &val
			}
			if strings.TrimSpace(b.Currency) != "" {
				walletCurrency = strings.TrimSpace(b.Currency)
			}
		}
	}

	var usageData mimoUsageData
	if len(usageEnv.Data) > 0 {
		if err := json.Unmarshal(usageEnv.Data, &usageData); err != nil {
			return markError(report, "解析 TokenPlan 数据失败", err.Error())
		}
	}
	if usageData.LoginURL != "" {
		report.Status = StatusAuthExpired
		report.TagText = "Cookie 过期"
		report.TagColor = "orange"
		report.Headline = "控制台登录态已过期"
		report.AlertReason = "小米 MiMo 接口返回登录跳转链接，请更新控制台 Cookie"
		return report
	}

	used, limit, usedPct, hasPlan := extractMimoPlanMetrics(usageData)
	planLabel := strings.TrimSpace(detail.PlanName)
	if planLabel == "" {
		planLabel = strings.TrimSpace(detail.PlanCode)
	}
	if planLabel == "" {
		planLabel = "TokenPlan"
	}
	isExpired := parseBool(detail.Expired) || strings.EqualFold(detail.Status, "expired")

	walletText := "—"
	if walletBalance != nil {
		walletText = fmt.Sprintf("¥%.2f %s", *walletBalance, walletCurrency)
	}

	resetText := detail.CurrentPeriodEnd
	if resetText == "" {
		resetText = "月末重置"
	}

	if hasPlan && !isExpired {
		remaining := math.Max(0, limit-used)
		report.Headline = fmt.Sprintf("已用 %.1f%% (剩余 %s)", usedPct, FormatTokenCount(remaining))
		report.Metrics = []MetricColumn{
			{
				Label: fmt.Sprintf("套餐进度 (%s)", planLabel),
				Value: fmt.Sprintf("**%.1f%%** (%s / %s)", usedPct, FormatTokenCount(used), FormatTokenCount(limit)),
			},
			{
				Label: "剩余 Token / 现金余额",
				Value: fmt.Sprintf("**%s** · %s", FormatTokenCount(remaining), walletText),
			},
			{
				Label: fmt.Sprintf("到期重置 (告警 >%.0f%%)", acc.MaxUsedPercent),
				Value: resetText,
			},
		}

		if usedPct >= acc.MaxUsedPercent {
			report.Status = StatusAlert
			report.TagText = "额度告急"
			report.TagColor = "red"
			report.AlertReason = fmt.Sprintf("小米 MiMo [%s] 本期已使用 %.1f%%（剩余 %s），超过告警阈值 %.1f%%",
				planLabel, usedPct, FormatTokenCount(remaining), acc.MaxUsedPercent)
		}
	} else if isExpired {
		report.Status = StatusAlert
		report.TagText = "套餐过期"
		report.TagColor = "red"
		report.Headline = fmt.Sprintf("[%s] 套餐已过期", planLabel)
		report.Metrics = []MetricColumn{
			{
				Label: "套餐状态",
				Value: fmt.Sprintf("**%s (已过期)**", planLabel),
			},
			{
				Label: "现金钱包余额",
				Value: walletText,
			},
			{
				Label: "到期时间",
				Value: resetText,
			},
		}
		report.AlertReason = fmt.Sprintf("小米 MiMo 套餐 [%s] 已于 %s 过期", planLabel, resetText)
	} else {
		report.TagText = "按量计费"
		report.TagColor = "wathet"
		report.Headline = fmt.Sprintf("无活跃套餐 · 余额 %s", walletText)
		report.Metrics = []MetricColumn{
			{
				Label: "当前计费模式",
				Value: "无生效 TokenPlan",
			},
			{
				Label: "现金钱包余额",
				Value: fmt.Sprintf("**%s**", walletText),
			},
			{
				Label: "余额告警线",
				Value: fmt.Sprintf("< ¥%.2f", acc.MinBalanceCNY),
			},
		}
	}

	if walletBalance != nil && acc.MinBalanceCNY > 0 && *walletBalance < acc.MinBalanceCNY {
		report.Status = StatusAlert
		report.TagText = "余额不足"
		report.TagColor = "red"
		if report.AlertReason == "" {
			report.AlertReason = fmt.Sprintf("小米 MiMo 现金余额 ¥%.2f %s 低于告警阈值 ¥%.2f %s", *walletBalance, walletCurrency, acc.MinBalanceCNY, walletCurrency)
		}
	}

	return report
}

func fetchMimoEndpoint(ctx context.Context, client *http.Client, path, cookie string) (mimoEnvelope, int, error) {
	var env mimoEnvelope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mimoBaseURL+path, nil)
	if err != nil {
		return env, 0, err
	}
	req.Header.Set("Accept", "application/json, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://platform.xiaomimimo.com/console/plan-manage")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return env, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return env, resp.StatusCode, err
	}

	_ = json.Unmarshal(body, &env)
	return env, resp.StatusCode, nil
}

func isMimoAuthExpired(httpStatus int, env mimoEnvelope) bool {
	if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden || (httpStatus >= 300 && httpStatus < 400) {
		return true
	}
	code := parseCode(env.Code)
	if code == 401 || code == 403 {
		return true
	}
	if env.LoginURL != "" {
		return true
	}
	return false
}

func extractMimoPlanMetrics(data mimoUsageData) (used, limit, usedPct float64, ok bool) {
	items := data.MonthUsage.Items
	if len(items) == 0 {
		items = data.Usage.Items
	}
	if len(items) == 0 {
		return 0, 0, 0, false
	}

	chosen := items[0]
	for _, it := range items {
		if strings.EqualFold(strings.TrimSpace(it.Name), "month_total_token") {
			chosen = it
			break
		}
	}

	u, uOk := parseNumber(chosen.Used)
	l, lOk := parseNumber(chosen.Limit)
	if !uOk || !lOk || l <= 0 {
		return 0, 0, 0, false
	}

	pct := math.Max(0, math.Min(100, (u/l)*100))
	return u, l, pct, true
}

func cleanCookie(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "cookie:") {
		s = strings.TrimSpace(s[len("cookie:"):])
	}
	return s
}

func parseCode(v any) int {
	if v == nil {
		return -1
	}
	if n, ok := parseNumber(v); ok {
		return int(n)
	}
	return -1
}

func parseNumber(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		cleaned := strings.TrimSpace(strings.NewReplacer(",", "", "%", "", "$", "").Replace(val))
		if cleaned == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(cleaned, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func parseBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true") || strings.TrimSpace(val) == "1"
	case float64:
		return val != 0
	default:
		return false
	}
}
