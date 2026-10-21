package providers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

	client := newOpenRouterHTTPClient(acc.ProxyURL)
	apiKey := strings.TrimSpace(acc.APIKey)

	// 1. 优先查询账户级总充值与消耗 (/api/v1/credits)
	creditsStatus, creditsBody, err := doOpenRouterGetWithRetry(ctx, client, "https://openrouter.ai/api/v1/credits", apiKey, 3)
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
	keyStatus, keyBody, err := doOpenRouterGetWithRetry(ctx, client, "https://openrouter.ai/api/v1/key", apiKey, 3)
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

func newOpenRouterHTTPClient(configuredProxy string) *http.Client {
	proxyFunc := http.ProxyFromEnvironment
	proxyStr := strings.TrimSpace(configuredProxy)
	if proxyStr == "" {
		for _, envKey := range []string{"OPENROUTER_PROXY_URL", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
			if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
				proxyStr = v
				break
			}
		}
	}
	if proxyStr != "" {
		if !strings.Contains(proxyStr, "://") {
			proxyStr = "http://" + proxyStr
		}
		if u, err := url.Parse(proxyStr); err == nil {
			proxyFunc = http.ProxyURL(u)
		}
	}

	transport := &http.Transport{
		Proxy: proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		// 强制使用 HTTP/1.1 避免经过 Clash/代理隧道时与 Cloudflare H2 ALPN 握手卡死或 EOF
		TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	return &http.Client{
		Timeout:   25 * time.Second,
		Transport: transport,
	}
}

func doOpenRouterGetWithRetry(ctx context.Context, client *http.Client, targetURL, apiKey string, attempts int) (int, []byte, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(time.Duration(i) * 800 * time.Millisecond):
			}
		}
		status, body, err := doOpenRouterGet(ctx, client, targetURL, apiKey)
		if err == nil {
			return status, body, nil
		}
		lastErr = err
	}
	return 0, nil, lastErr
}

func doOpenRouterGet(ctx context.Context, client *http.Client, targetURL, apiKey string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rattler/0.1.0")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}
