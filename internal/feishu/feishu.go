package feishu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nasa1024/rattler/internal/config"
	"github.com/nasa1024/rattler/internal/providers"
)

// Card 2.0 (schema: "2.0") 飞书官方规范结构
type webhookPayload struct {
	Timestamp string   `json:"timestamp,omitempty"`
	Sign      string   `json:"sign,omitempty"`
	MsgType   string   `json:"msg_type"`
	Card      cardV2   `json:"card"`
}

type cardV2 struct {
	Schema string     `json:"schema"`
	Config cardConfig `json:"config"`
	Header cardHeader `json:"header"`
	Body   cardBody   `json:"body"`
}

type cardConfig struct {
	UpdateMulti bool `json:"update_multi"`
}

type cardHeader struct {
	Title       plainText `json:"title"`
	Subtitle    plainText `json:"subtitle"`
	Template    string    `json:"template"`
	TextTagList []textTag `json:"text_tag_list,omitempty"`
}

type plainText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type textTag struct {
	Tag   string    `json:"tag"`
	Text  plainText `json:"text"`
	Color string    `json:"color"`
}

type cardBody struct {
	Direction string `json:"direction"`
	Padding   string `json:"padding,omitempty"`
	Elements  []any  `json:"elements"`
}

type markdownElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type hrElement struct {
	Tag string `json:"tag"`
}

type columnSetElement struct {
	Tag               string          `json:"tag"`
	FlexMode          string          `json:"flex_mode"`
	BackgroundStyle   string          `json:"background_style,omitempty"`
	HorizontalSpacing string          `json:"horizontal_spacing,omitempty"`
	Columns           []columnElement `json:"columns"`
}

type columnElement struct {
	Tag           string `json:"tag"`
	Width         string `json:"width"`
	Weight        int    `json:"weight,omitempty"`
	VerticalAlign string `json:"vertical_align,omitempty"`
	Elements      []any  `json:"elements"`
}

type buttonElement struct {
	Tag       string           `json:"tag"`
	Text      plainText        `json:"text"`
	Type      string           `json:"type"`
	Size      string           `json:"size,omitempty"`
	Width     string           `json:"width,omitempty"`
	Behaviors []buttonBehavior `json:"behaviors"`
}

type buttonBehavior struct {
	Type       string `json:"type"`
	DefaultURL string `json:"default_url"`
}

type feishuResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// BuildCardPayload constructs a Feishu Card JSON 2.0 (schema: "2.0") monitoring/alert card.
func BuildCardPayload(cfg config.FeishuConfig, title string, reports []providers.AccountReport) (webhookPayload, error) {
	okCount := 0
	alertCount := 0
	authExpiredCount := 0

	for _, r := range reports {
		switch r.Status {
		case providers.StatusOK:
			okCount++
		case providers.StatusAuthExpired:
			authExpiredCount++
		default:
			alertCount++
		}
	}

	hasAlert := (alertCount + authExpiredCount) > 0

	// 决定卡片头部配色与右上角胶囊标签
	headerTemplate := "wathet"
	headerTagText := "运行正常"
	headerTagColor := "green"

	if alertCount > 0 {
		headerTemplate = "red"
		headerTagText = fmt.Sprintf("%d 项额度告警", alertCount+authExpiredCount)
		headerTagColor = "red"
	} else if authExpiredCount > 0 {
		headerTemplate = "orange"
		headerTagText = fmt.Sprintf("%d 项凭证失效", authExpiredCount)
		headerTagColor = "orange"
	}

	nowStr := time.Now().Format("2006-01-02 15:04:05")
	subtitle := fmt.Sprintf("巡检时间 %s · 监控 %d 个账号", nowStr, len(reports))

	var elements []any

	// 1. 顶部浅灰底色 KPI 汇总分栏 (官方监控看板模板风格)
	abnormalColor := "grey"
	if hasAlert {
		abnormalColor = "red"
	}
	elements = append(elements, columnSetElement{
		Tag:               "column_set",
		FlexMode:          "bisect",
		BackgroundStyle:   "grey",
		HorizontalSpacing: "default",
		Columns: []columnElement{
			{
				Tag:           "column",
				Width:         "weighted",
				Weight:        1,
				VerticalAlign: "top",
				Elements: []any{
					markdownElement{
						Tag:     "markdown",
						Content: fmt.Sprintf("<font color='grey'>巡检渠道数</font>\n**%d 个**", len(reports)),
					},
				},
			},
			{
				Tag:           "column",
				Width:         "weighted",
				Weight:        1,
				VerticalAlign: "top",
				Elements: []any{
					markdownElement{
						Tag:     "markdown",
						Content: fmt.Sprintf("<font color='grey'>状态正常</font>\n<font color='green'>**%d 个**</font>", okCount),
					},
				},
			},
			{
				Tag:           "column",
				Width:         "weighted",
				Weight:        1,
				VerticalAlign: "top",
				Elements: []any{
					markdownElement{
						Tag:     "markdown",
						Content: fmt.Sprintf("<font color='grey'>异常 / 告警</font>\n<font color='%s'>**%d 个**</font>", abnormalColor, alertCount+authExpiredCount),
					},
				},
			},
		},
	})

	// 2. 各渠道账号详情区块
	for _, r := range reports {
		elements = append(elements, hrElement{Tag: "hr"})

		tagColor := r.TagColor
		if tagColor == "" {
			tagColor = "neutral"
		}
		tagText := r.TagText
		if tagText == "" {
			tagText = string(r.Status)
		}

		// 账号标题 + 胶囊状态标签
		elements = append(elements, markdownElement{
			Tag:     "markdown",
			Content: fmt.Sprintf("**%s · %s** <text_tag color='%s'>%s</text_tag>", r.Provider, r.AccountName, tagColor, tagText),
		})

		// 三列核心指标布局
		if len(r.Metrics) > 0 {
			var cols []columnElement
			for _, m := range r.Metrics {
				cols = append(cols, columnElement{
					Tag:           "column",
					Width:         "weighted",
					Weight:        1,
					VerticalAlign: "top",
					Elements: []any{
						markdownElement{
							Tag:     "markdown",
							Content: fmt.Sprintf("<font color='grey'>%s</font>\n%s", m.Label, m.Value),
						},
					},
				})
			}
			elements = append(elements, columnSetElement{
				Tag:               "column_set",
				FlexMode:          "stretch",
				BackgroundStyle:   "default",
				HorizontalSpacing: "default",
				Columns:           cols,
			})
		} else {
			elements = append(elements, markdownElement{
				Tag:     "markdown",
				Content: fmt.Sprintf("<font color='grey'>当前状态</font>：%s", r.Headline),
			})
		}

		// 若存在告警或凭证失效，高亮显示原因
		if r.AlertReason != "" {
			color := "red"
			if r.Status == providers.StatusAuthExpired {
				color = "orange"
			}
			elements = append(elements, markdownElement{
				Tag:     "markdown",
				Content: fmt.Sprintf("<font color='%s'>**告警说明：**%s</font>", color, r.AlertReason),
			})
		}
	}

	// 3. 告警时可选 @所有人
	if hasAlert && cfg.AtAllOnAlert {
		elements = append(elements, hrElement{Tag: "hr"})
		elements = append(elements, markdownElement{
			Tag:     "markdown",
			Content: "<at id=all></at> 检测到模型额度低于水位线或登录凭证过期，请点击下方入口及时充值或更新 Cookie。",
		})
	}

	// 4. 底部快捷跳转控制台按钮栏
	btnCols := buildConsoleButtons(reports)
	if len(btnCols) > 0 {
		elements = append(elements, hrElement{Tag: "hr"})
		elements = append(elements, columnSetElement{
			Tag:               "column_set",
			FlexMode:          "flow",
			HorizontalSpacing: "default",
			Columns:           btnCols,
		})
	}

	payload := webhookPayload{
		MsgType: "interactive",
		Card: cardV2{
			Schema: "2.0",
			Config: cardConfig{UpdateMulti: true},
			Header: cardHeader{
				Title: plainText{
					Tag:     "plain_text",
					Content: title,
				},
				Subtitle: plainText{
					Tag:     "plain_text",
					Content: subtitle,
				},
				Template: headerTemplate,
				TextTagList: []textTag{
					{
						Tag: "text_tag",
						Text: plainText{
							Tag:     "plain_text",
							Content: headerTagText,
						},
						Color: headerTagColor,
					},
				},
			},
			Body: cardBody{
				Direction: "vertical",
				Padding:   "12px 12px 12px 12px",
				Elements:  elements,
			},
		},
	}

	if strings.TrimSpace(cfg.Secret) != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sign, err := genFeishuSign(cfg.Secret, ts)
		if err != nil {
			return payload, err
		}
		payload.Timestamp = ts
		payload.Sign = sign
	}

	return payload, nil
}

// SendReport builds and posts the Card 2.0 message to the Feishu group bot webhook.
func SendReport(ctx context.Context, cfg config.FeishuConfig, title string, reports []providers.AccountReport) error {
	if strings.TrimSpace(cfg.WebhookURL) == "" {
		return fmt.Errorf("未配置飞书机器人 Webhook 地址 (FEISHU_WEBHOOK_URL)")
	}

	payload, err := BuildCardPayload(cfg, title, reports)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("飞书 Webhook HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed feishuResp
	if err := json.Unmarshal(respBytes, &parsed); err == nil && parsed.Code != 0 {
		return fmt.Errorf("飞书 Webhook 返回错误 (code=%d): %s", parsed.Code, parsed.Msg)
	}

	return nil
}

func buildConsoleButtons(reports []providers.AccountReport) []columnElement {
	seen := make(map[string]bool)
	var cols []columnElement

	for _, r := range reports {
		if r.ConsoleURL == "" || seen[r.Provider] {
			continue
		}
		seen[r.Provider] = true

		btnType := "default"
		if r.NeedsAlert() {
			btnType = "primary_filled"
		}

		label := r.Provider + " 控制台"
		switch r.Provider {
		case "DeepSeek":
			label = "DeepSeek 充值后台"
		case "OpenRouter":
			label = "OpenRouter Credits"
		case "Xiaomi MiMo":
			label = "小米 MiMo 套餐管理"
		}

		cols = append(cols, columnElement{
			Tag:    "column",
			Width:  "auto",
			Elements: []any{
				buttonElement{
					Tag:  "button",
					Type: btnType,
					Size: "small",
					Text: plainText{
						Tag:     "plain_text",
						Content: label,
					},
					Behaviors: []buttonBehavior{
						{
							Type:       "open_url",
							DefaultURL: r.ConsoleURL,
						},
					},
				},
			},
		})
	}

	return cols
}

func genFeishuSign(secret, timestamp string) (string, error) {
	stringToSign := timestamp + "\n" + secret
	h := hmac.New(sha256.New, []byte(stringToSign))
	if _, err := h.Write([]byte{}); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
