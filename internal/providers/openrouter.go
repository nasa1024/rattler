package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nasa1024/rattler/internal/config"
)

type openRouterCreditsResp struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

type openRouterKeyResp struct {
	Data struct {
		Label          string   `json:"label"`
		Usage          float64  `json:"usage"`
		Limit          *float64 `json:"limit"`
		LimitRemaining *float64 `json:"limit_remaining"`
		IsFreeTier     bool     `json:"is_free_tier"`
	} `json:"data"`
}

func CheckOpenRouter(ctx context.Context, acc config.OpenRouterAccount) AccountReport {
	report := AccountReport{
		Provider:    "OpenRouter",
		AccountName: acc.Name,
		Status:      StatusOK,
		TagText:     "额度正常",
		TagColor:    "green",
		ConsoleURL:  "https://openrouter.ai/settings/credits",
	}

	client := &http.Client{Timeout: 15 * time.Second}
	apiKey := strings.TrimSpace(acc.APIKey)

	// 1. 优先查询账户级总充值与消耗 (/api/v1/credits)
	creditsStatus, creditsBody, err := doOpenRouterGet(ctx, client, "https://openrouter.ai/api/v1/credits", apiKey)
	if err != nil {
		return markError(report, "网络请求失败", err.Error())
	}

	if creditsStatus == http.StatusUnauthorized {
		report.Status = StatusAuthExpired
		report.TagText = "Key 失效"
		report.TagColor = "orange"
		report.Headline = "API Key 鉴权失败 (HTTP 401)"
		report.AlertReason = "OpenRouter API Key 无效或已吊销"
		return report
	}

	if creditsStatus >= 200 && creditsStatus < 300 {
		var parsed openRouterCreditsResp
		if err := json.Unmarshal(creditsBody, &parsed); err == nil {
			remaining := parsed.Data.TotalCredits - parsed.Data.TotalUsage
			var usedPct float64
			if parsed.Data.TotalCredits > 0 {
				usedPct = (parsed.Data.TotalUsage / parsed.Data.TotalCredits) * 100
			}
			report.Headline = fmt.Sprintf("$%.2f USD", remaining)
			report.Metrics = []MetricColumn{
				{
					Label: "剩余可用 Credits",
					Value: fmt.Sprintf("**$%.2f USD**", remaining),
				},
				{
					Label: "累计已用 / 总充值",
					Value: fmt.Sprintf("$%.2f / $%.2f (%.1f%%)", parsed.Data.TotalUsage, parsed.Data.TotalCredits, usedPct),
				},
				{
					Label: "告警水位线",
					Value: fmt.Sprintf("< $%.2f USD", acc.MinBalanceUSD),
				},
			}
			if remaining < acc.MinBalanceUSD {
				report.Status = StatusAlert
				report.TagText = "余额不足"
				report.TagColor = "red"
				report.AlertReason = fmt.Sprintf("OpenRouter 剩余额度 $%.2f USD 低于告警阈值 $%.2f USD", remaining, acc.MinBalanceUSD)
			}
			return report
		}
	}

	// 2. 若 Key 无 /credits 权限，回退查询 /api/v1/key 的单 Key 限额与用量
	keyStatus, keyBody, err := doOpenRouterGet(ctx, client, "https://openrouter.ai/api/v1/key", apiKey)
	if err != nil {
		return markError(report, "网络请求失败", err.Error())
	}
	if keyStatus == http.StatusUnauthorized || keyStatus == http.StatusForbidden {
		report.Status = StatusAuthExpired
		report.TagText = "Key 失效"
		report.TagColor = "orange"
		report.Headline = fmt.Sprintf("API Key 鉴权失败 (HTTP %d)", keyStatus)
		report.AlertReason = "OpenRouter API Key 无效或无权读取用量"
		return report
	}
	if keyStatus < 200 || keyStatus >= 300 {
		return markError(report, fmt.Sprintf("接口异常 (HTTP %d)", keyStatus), strings.TrimSpace(string(keyBody)))
	}

	var keyParsed openRouterKeyResp
	if err := json.Unmarshal(keyBody, &keyParsed); err != nil {
		return markError(report, "JSON 解析失败", err.Error())
	}

	if keyParsed.Data.Limit != nil && keyParsed.Data.LimitRemaining != nil {
		rem := *keyParsed.Data.LimitRemaining
		lim := *keyParsed.Data.Limit
		var usedPct float64
		if lim > 0 {
			usedPct = ((lim - rem) / lim) * 100
		}
		report.Headline = fmt.Sprintf("$%.2f USD (Key 限额)", rem)
		report.Metrics = []MetricColumn{
			{
				Label: "Key 剩余限额",
				Value: fmt.Sprintf("**$%.2f USD**", rem),
			},
			{
				Label: "Key 已用 / 限额",
				Value: fmt.Sprintf("$%.2f / $%.2f (%.1f%%)", keyParsed.Data.Usage, lim, usedPct),
			},
			{
				Label: "告警水位线",
				Value: fmt.Sprintf("< $%.2f USD", acc.MinBalanceUSD),
			},
		}
		if rem < acc.MinBalanceUSD {
			report.Status = StatusAlert
			report.TagText = "限额不足"
			report.TagColor = "red"
			report.AlertReason = fmt.Sprintf("OpenRouter Key 剩余限额 $%.2f USD 低于告警阈值 $%.2f USD", rem, acc.MinBalanceUSD)
		}
		return report
	}

	report.Headline = fmt.Sprintf("已消耗 $%.2f USD", keyParsed.Data.Usage)
	report.Metrics = []MetricColumn{
		{
			Label: "Key 累计消耗",
			Value: fmt.Sprintf("**$%.2f USD**", keyParsed.Data.Usage),
		},
		{
			Label: "Key 消费上限",
			Value: "未设上限",
		},
		{
			Label: "权限说明",
			Value: "建议使用 Management Key 查总余额",
		},
	}
	return report
}

func doOpenRouterGet(ctx context.Context, client *http.Client, url, apiKey string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}
