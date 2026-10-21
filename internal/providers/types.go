package providers

import (
	"fmt"
)

type StatusLevel string

const (
	StatusOK          StatusLevel = "OK"
	StatusAlert       StatusLevel = "ALERT"
	StatusAuthExpired StatusLevel = "AUTH_EXPIRED"
	StatusError       StatusLevel = "ERROR"
)

type MetricColumn struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type AccountReport struct {
	Provider    string         `json:"provider"`
	AccountName string         `json:"account_name"`
	Status      StatusLevel    `json:"status"`
	TagText     string         `json:"tag_text"`
	TagColor    string         `json:"tag_color"` // green, red, orange, wathet, neutral
	Headline    string         `json:"headline"`
	Metrics     []MetricColumn `json:"metrics"`
	AlertReason string         `json:"alert_reason,omitempty"`
	ConsoleURL  string         `json:"console_url,omitempty"`
}

func (r AccountReport) NeedsAlert() bool {
	return r.Status != StatusOK
}

func (r AccountReport) AlertKey() string {
	return fmt.Sprintf("%s:%s:%s", r.Provider, r.AccountName, r.Status)
}

// FormatTokenCount formats large token numbers into human-readable M / B strings.
func FormatTokenCount(n float64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", n/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}
