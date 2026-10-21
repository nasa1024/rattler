package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nasa1024/rattler/internal/config"
)

type deepseekBalanceResp struct {
	IsAvailable  bool                  `json:"is_available"`
	BalanceInfos []deepseekBalanceInfo `json:"balance_infos"`
}

type deepseekBalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

func CheckDeepSeek(ctx context.Context, acc config.DeepSeekAccount) AccountReport {
	report := AccountReport{
		Provider:    "DeepSeek",
		AccountName: acc.Name,
		Status:      StatusOK,
		TagText:     "余额正常",
		TagColor:    "green",
		ConsoleURL:  "https://platform.deepseek.com/top_up",
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.deepseek.com/user/balance", nil)
	if err != nil {
		return markError(report, "请求构造失败", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acc.APIKey))
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return markError(report, "网络请求失败", err.Error())
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return markError(report, "读取响应失败", err.Error())
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		report.Status = StatusAuthExpired
		report.TagText = "Key 失效"
		report.TagColor = "orange"
		report.Headline = fmt.Sprintf("API Key 鉴权失败 (HTTP %d)", resp.StatusCode)
		report.AlertReason = "DeepSeek API Key 无效或已失效，请检查配置"
		return report
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return markError(report, fmt.Sprintf("接口异常 (HTTP %d)", resp.StatusCode), strings.TrimSpace(string(body)))
	}

	var parsed deepseekBalanceResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return markError(report, "JSON 解析失败", err.Error())
	}

	if len(parsed.BalanceInfos) == 0 {
		return markError(report, "未返回余额信息", "balance_infos 为空")
	}

	selected := parsed.BalanceInfos[0]
	for _, info := range parsed.BalanceInfos {
		if v, _ := strconv.ParseFloat(info.TotalBalance, 64); v > 0 {
			selected = info
			break
		}
	}

	total, _ := strconv.ParseFloat(selected.TotalBalance, 64)
	granted, _ := strconv.ParseFloat(selected.GrantedBalance, 64)
	toppedUp, _ := strconv.ParseFloat(selected.ToppedUpBalance, 64)
	currency := selected.Currency
	if currency == "" {
		currency = "CNY"
	}

	symbol := "¥"
	if strings.EqualFold(currency, "USD") {
		symbol = "$"
	}

	report.Headline = fmt.Sprintf("%s%.2f %s", symbol, total, currency)
	report.Metrics = []MetricColumn{
		{
			Label: "可用总余额",
			Value: fmt.Sprintf("**%s%.2f %s**", symbol, total, currency),
		},
		{
			Label: "充值 / 赠送明细",
			Value: fmt.Sprintf("%s%.2f / %s%.2f", symbol, toppedUp, symbol, granted),
		},
		{
			Label: "告警水位线",
			Value: fmt.Sprintf("< %s%.2f %s", symbol, acc.MinBalance, currency),
		},
	}

	if !parsed.IsAvailable || total < acc.MinBalance {
		report.Status = StatusAlert
		report.TagText = "余额不足"
		report.TagColor = "red"
		if !parsed.IsAvailable {
			report.AlertReason = fmt.Sprintf("账户余额已耗尽不可用（当前 %s%.2f %s）", symbol, total, currency)
		} else {
			report.AlertReason = fmt.Sprintf("当前可用余额 %s%.2f %s 低于告警阈值 %s%.2f %s", symbol, total, currency, symbol, acc.MinBalance, currency)
		}
	}

	return report
}

func markError(report AccountReport, headline, reason string) AccountReport {
	report.Status = StatusError
	report.TagText = "查询异常"
	report.TagColor = "red"
	report.Headline = headline
	report.AlertReason = reason
	return report
}
