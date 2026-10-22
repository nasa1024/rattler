package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasa1024/rattler/internal/config"
)

const mimoBaseURL = "https://platform.xiaomimimo.com/api/v1"

var (
	mimoSessionMu    sync.Mutex
	mimoSessionCache = make(map[string]string)
)

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
	passToken, userID, deviceID := extractPassCredentials(acc, cookie)
	cacheKey := acc.Name + "|" + userID
	if cached := getCachedMimoCookie(cacheKey); cached != "" {
		cookie = cached
	}

	if cookie == "" && passToken == "" {
		report.Status = StatusAuthExpired
		report.TagText = "缺少凭证"
		report.TagColor = "orange"
		report.Headline = "未配置控制台 Cookie 或 passToken"
		report.AlertReason = "小米 MiMo TokenPlan 需配置 pass_token + user_id（推荐长期免维护）或控制台 Cookie"
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
	if isMimoAuthExpired(status, usageEnv) && passToken != "" && userID != "" {
		loginURL := strings.TrimSpace(usageEnv.LoginURL)
		if loginURL == "" && len(usageEnv.Data) > 0 {
			var tmp mimoUsageData
			if json.Unmarshal(usageEnv.Data, &tmp) == nil {
				loginURL = strings.TrimSpace(tmp.LoginURL)
			}
		}
		mintedCookie, mintErr := mintMimoServiceCookie(ctx, client, loginURL, passToken, userID, deviceID, cleanCookie(acc.Cookie))
		if mintErr == nil && mintedCookie != "" {
			log.Printf("[Xiaomi MiMo] %s: 已通过小米通行证 passToken 自动续期 api-platform_serviceToken", acc.Name)
			cookie = mintedCookie
			setCachedMimoCookie(cacheKey, cookie)
			usageEnv, status, err = fetchMimoEndpoint(ctx, client, "/tokenPlan/usage", cookie)
			if err != nil {
				return markError(report, "网络请求失败", err.Error())
			}
		} else if mintErr != nil {
			log.Printf("[Xiaomi MiMo] %s: passToken 自动续期失败: %v", acc.Name, mintErr)
		}
	}
	if isMimoAuthExpired(status, usageEnv) {
		report.Status = StatusAuthExpired
		report.TagText = "Cookie 过期"
		report.TagColor = "orange"
		report.Headline = "控制台登录态已过期"
		if passToken != "" {
			report.AlertReason = "小米通行证 passToken 已失效或需二次验证，请前往 account.xiaomi.com 重新获取 passToken"
		} else {
			report.AlertReason = "小米 MiMo 控制台 Cookie (24h 有效期) 已失效，建议配置 account.xiaomi.com 的 passToken 实现自动续期"
		}
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
	req.Header.Set("x-timezone", "Asia/Shanghai")
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
	if strings.TrimSpace(env.LoginURL) != "" {
		return true
	}
	if len(env.Data) > 0 {
		var tmp struct {
			LoginURL string `json:"loginUrl"`
		}
		if json.Unmarshal(env.Data, &tmp) == nil && strings.TrimSpace(tmp.LoginURL) != "" {
			return true
		}
	}
	return false
}

func getCachedMimoCookie(key string) string {
	mimoSessionMu.Lock()
	defer mimoSessionMu.Unlock()
	return mimoSessionCache[key]
}

func setCachedMimoCookie(key, cookie string) {
	mimoSessionMu.Lock()
	defer mimoSessionMu.Unlock()
	mimoSessionCache[key] = cookie
}

func parseCookiePairs(raw string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(cleanCookie(raw), ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.IndexByte(part, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:idx])
		v := strings.TrimSpace(part[idx+1:])
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func extractPassCredentials(acc config.XiaomiMiMoAccount, rawCookie string) (passToken, userID, deviceID string) {
	pairs := parseCookiePairs(rawCookie)
	passToken = strings.TrimSpace(acc.PassToken)
	if passToken == "" {
		passToken = strings.Trim(pairs["passToken"], `"`)
	}
	userID = strings.TrimSpace(acc.UserID)
	if userID == "" {
		userID = strings.Trim(pairs["userId"], `"`)
	}
	deviceID = strings.TrimSpace(acc.DeviceID)
	if deviceID == "" {
		deviceID = strings.Trim(pairs["deviceId"], `"`)
	}
	return passToken, userID, deviceID
}

// mintMimoServiceCookie exchanges a Xiaomi Account passToken + userId + deviceId for a fresh
// api-platform_serviceToken on platform.xiaomimimo.com via Xiaomi SSO (serviceLogin?_json=true -> serviceLoginAuth2 -> /sts).
func mintMimoServiceCookie(ctx context.Context, client *http.Client, loginURL, passToken, userID, deviceID, baseCookie string) (string, error) {
	if loginURL == "" {
		env, _, err := fetchMimoEndpoint(ctx, client, "/balance", "")
		if err != nil {
			return "", fmt.Errorf("fetch SSO loginUrl: %w", err)
		}
		loginURL = strings.TrimSpace(env.LoginURL)
		if loginURL == "" && len(env.Data) > 0 {
			var tmp struct {
				LoginURL string `json:"loginUrl"`
			}
			if json.Unmarshal(env.Data, &tmp) == nil {
				loginURL = strings.TrimSpace(tmp.LoginURL)
			}
		}
	}
	if loginURL == "" {
		return "", fmt.Errorf("xiaomi mimo API did not return loginUrl")
	}

	callback := extractCallbackFromLoginURL(loginURL)
	if callback == "" {
		return "", fmt.Errorf("failed to extract callback from loginUrl")
	}

	if deviceID == "" {
		deviceID = "wb_rattler_default"
	}
	accountCookie := fmt.Sprintf("passToken=%s; deviceId=%s; uLocale=zh_CN; pass_ua=web; passInfo=login-end; userId=%s",
		strings.Trim(passToken, `"`), deviceID, userID)

	chromeUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	setCommonHeaders := func(req *http.Request) {
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", chromeUA)
		req.Header.Set("Cookie", accountCookie)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Referer", "https://account.xiaomi.com/fe/service/login")
	}

	// Step 1: GET /pass/serviceLogin?_json=true&sid=api-platform&callback=...
	slURL := fmt.Sprintf("https://account.xiaomi.com/pass/serviceLogin?_json=true&sid=api-platform&callback=%s", url.QueryEscape(callback))
	req1, err := http.NewRequestWithContext(ctx, http.MethodGet, slURL, nil)
	if err != nil {
		return "", err
	}
	setCommonHeaders(req1)

	resp1, err := client.Do(req1)
	if err != nil {
		return "", err
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	loginBody, err := parseXiaomiJSONP(string(body1))
	if err != nil {
		return "", fmt.Errorf("serviceLogin parse error: %w", err)
	}

	stsLocation := strings.TrimSpace( fmt.Sprint(loginBody["location"]) )
	if loginBody["location"] == nil {
		stsLocation = ""
	}

	// Step 2: If serviceLogin did not directly return the /sts location, try POST /pass/serviceLoginAuth2
	if parseCode(loginBody["code"]) != 0 || !strings.HasPrefix(stsLocation, "https://platform.xiaomimimo.com") {
		sign, _ := loginBody["_sign"].(string)
		if sign == "" {
			return "", fmt.Errorf("serviceLogin did not return _sign (code=%v)", loginBody["code"])
		}
		serviceParam, _ := loginBody["serviceParam"].(string)
		if serviceParam == "" {
			serviceParam = `{"checkSafePhone":false,"checkSafeAddress":false,"lsrp_score":0.0}`
		}
		qs, _ := loginBody["qs"].(string)

		form := url.Values{}
		form.Set("bizDeviceType", "")
		form.Set("needTheme", "false")
		form.Set("theme", "")
		form.Set("showActiveX", "false")
		form.Set("serviceParam", serviceParam)
		form.Set("callback", callback)
		form.Set("qs", qs)
		form.Set("sid", "api-platform")
		form.Set("_sign", sign)
		form.Set("_json", "true")
		form.Set("policyName", "miaccount")
		form.Set("captCode", "")

		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://account.xiaomi.com/pass/serviceLoginAuth2", strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		setCommonHeaders(req2)
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

		resp2, err := client.Do(req2)
		if err != nil {
			return "", err
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		auth2Body, err := parseXiaomiJSONP(string(body2))
		if err != nil {
			return "", fmt.Errorf("serviceLoginAuth2 parse error: %w", err)
		}
		if parseCode(auth2Body["code"]) != 0 {
			desc, _ := auth2Body["description"].(string)
			return "", fmt.Errorf("serviceLoginAuth2 failed (code=%v %s)", auth2Body["code"], desc)
		}
		stsLocation, _ = auth2Body["location"].(string)
	}

	if stsLocation == "" {
		return "", fmt.Errorf("SSO did not return /sts location")
	}

	// Step 3: Follow /sts and subsequent redirects on platform.xiaomimimo.com to collect cookies
	platformJar := parseCookiePairs(baseCookie)
	delete(platformJar, "passToken")
	delete(platformJar, "deviceId")
	if userID != "" {
		platformJar["userId"] = userID
	}

	currentURL := stsLocation
	for hop := 0; hop < 5 && currentURL != ""; hop++ {
		u, err := url.Parse(currentURL)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(u.Hostname(), "platform.xiaomimimo.com") && u.Scheme == "http" {
			u.Scheme = "https"
			currentURL = u.String()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", chromeUA)
		if len(platformJar) > 0 {
			req.Header.Set("Cookie", serializeCookieMap(platformJar))
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		for _, rawSC := range resp.Header.Values("Set-Cookie") {
			name, val, _, expired := parseRawSetCookie(rawSC)
			if name == "" || val == "" || expired {
				continue
			}
			platformJar[name] = val
		}

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			break
		}
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if loc == "" {
			break
		}
		nextU, err := u.Parse(loc)
		if err != nil {
			break
		}
		currentURL = nextU.String()
	}

	if platformJar["api-platform_serviceToken"] == "" {
		return "", fmt.Errorf("/sts did not issue api-platform_serviceToken")
	}
	return serializeCookieMap(platformJar), nil
}

func extractCallbackFromLoginURL(loginURL string) string {
	u, err := url.Parse(loginURL)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("callback"))
}

func parseXiaomiJSONP(raw string) (map[string]any, error) {
	s := strings.TrimSpace(raw)
	const prefix = "&&&START&&&"
	if strings.HasPrefix(s, prefix) {
		s = strings.TrimSpace(s[len(prefix):])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseRawSetCookie(raw string) (name, val, domain string, expired bool) {
	parts := strings.Split(raw, ";")
	if len(parts) == 0 {
		return "", "", "", false
	}
	first := strings.TrimSpace(parts[0])
	idx := strings.IndexByte(first, '=')
	if idx <= 0 {
		return "", "", "", false
	}
	name = strings.TrimSpace(first[:idx])
	val = strings.TrimSpace(first[idx+1:])
	if strings.EqualFold(strings.Trim(val, `"`), "EXPIRED") {
		expired = true
	}
	for _, attr := range parts[1:] {
		attr = strings.TrimSpace(attr)
		aIdx := strings.IndexByte(attr, '=')
		if aIdx <= 0 {
			continue
		}
		ak := strings.ToLower(strings.TrimSpace(attr[:aIdx]))
		av := strings.TrimSpace(attr[aIdx+1:])
		if ak == "domain" {
			domain = strings.ToLower(av)
		} else if ak == "max-age" {
			if ma, err := strconv.Atoi(av); err == nil && ma <= 0 {
				expired = true
			}
		} else if ak == "expires" && strings.Contains(av, "1970") {
			expired = true
		}
	}
	return name, val, domain, expired
}

func serializeCookieMap(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		if k != "" && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
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
