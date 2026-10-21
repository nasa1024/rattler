package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nasa1024/rattler/internal/config"
	"github.com/nasa1024/rattler/internal/feishu"
	"github.com/nasa1024/rattler/internal/providers"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径 (JSON)")
	runOnce := flag.Bool("once", false, "仅执行一次检查后退出")
	forceNotify := flag.Bool("force", false, "无论是否触发告警均强制发送飞书卡片")
	dryRun := flag.Bool("dry-run", false, "仅在终端打印检查结果，不发送飞书消息")
	testCard := flag.Bool("test-card", false, "使用模拟数据生成并发送飞书卡片 2.0（若配合 -dry-run 则仅在终端输出卡片 JSON）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *testCard {
		runMockCardTest(ctx, cfg, *dryRun)
		return
	}

	totalAccounts := len(cfg.DeepSeek) + len(cfg.OpenRouter) + len(cfg.XiaomiMiMo)
	if totalAccounts == 0 {
		log.Fatalf("未配置任何监控账号，请在 %s 或 .env 中配置 DeepSeek / OpenRouter / Xiaomi MiMo 凭证（或使用 -test-card 预览卡片）", *configPath)
	}

	if *runOnce {
		runCheckAndNotify(ctx, cfg, *forceNotify, *dryRun, nil)
		return
	}

	log.Printf("启动常驻监控: 共 %d 个账号，巡检间隔 %d 分钟，每日汇总时间 %q",
		totalAccounts, cfg.Schedule.CheckIntervalMinutes, cfg.Schedule.DailyReportTime)

	lastAlertSent := make(map[string]time.Time)
	lastDailyReportDate := ""

	runCheckAndNotify(ctx, cfg, *forceNotify, *dryRun, lastAlertSent)

	checkTicker := time.NewTicker(time.Duration(cfg.Schedule.CheckIntervalMinutes) * time.Minute)
	defer checkTicker.Stop()

	clockTicker := time.NewTicker(30 * time.Second)
	defer clockTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("收到退出信号，停止监控。")
			return
		case <-checkTicker.C:
			runCheckAndNotify(ctx, cfg, false, *dryRun, lastAlertSent)
		case now := <-clockTicker.C:
			if cfg.Schedule.DailyReportTime != "" {
				today := now.Format("2006-01-02")
				hhmm := now.Format("15:04")
				if hhmm == cfg.Schedule.DailyReportTime && lastDailyReportDate != today {
					lastDailyReportDate = today
					log.Printf("触发每日定时汇总 (%s)", hhmm)
					runCheckAndNotify(ctx, cfg, true, *dryRun, lastAlertSent)
				}
			}
		}
	}
}

func runMockCardTest(ctx context.Context, cfg *config.Config, dryRun bool) {
	mockReports := []providers.AccountReport{
		{
			Provider:    "DeepSeek",
			AccountName: "生产主账号",
			Status:      providers.StatusOK,
			TagText:     "余额正常",
			TagColor:    "green",
			Headline:    "¥128.50 CNY",
			ConsoleURL:  "https://platform.deepseek.com/top_up",
			Metrics: []providers.MetricColumn{
				{Label: "可用总余额", Value: "**¥128.50 CNY**"},
				{Label: "充值 / 赠送明细", Value: "¥128.50 / ¥0.00"},
				{Label: "告警水位线", Value: "< ¥20.00 CNY"},
			},
		},
		{
			Provider:    "OpenRouter",
			AccountName: "海外路由账号",
			Status:      providers.StatusAlert,
			TagText:     "余额不足",
			TagColor:    "red",
			Headline:    "$3.42 USD",
			ConsoleURL:  "https://openrouter.ai/settings/credits",
			Metrics: []providers.MetricColumn{
				{Label: "剩余可用 Credits", Value: "**$3.42 USD**"},
				{Label: "累计已用 / 总充值", Value: "$96.58 / $100.00 (96.6%)"},
				{Label: "告警水位线", Value: "< $5.00 USD"},
			},
			AlertReason: "OpenRouter 剩余额度 $3.42 USD 低于告警阈值 $5.00 USD",
		},
		{
			Provider:    "Xiaomi MiMo",
			AccountName: "小米 TokenPlan",
			Status:      providers.StatusOK,
			TagText:     "套餐正常",
			TagColor:    "green",
			Headline:    "已用 34.2% (剩余 2.70B)",
			ConsoleURL:  "https://platform.xiaomimimo.com/console/plan-manage",
			Metrics: []providers.MetricColumn{
				{Label: "套餐进度 (Lite)", Value: "**34.2%** (1.40B / 4.10B)"},
				{Label: "剩余 Token / 现金余额", Value: "**2.70B** · ¥15.00 CNY"},
				{Label: "到期重置 (告警 >85%)", Value: "2026-11-01 00:00:00"},
			},
		},
	}

	title := "模型额度与套餐监控看板"
	payload, err := feishu.BuildCardPayload(cfg.Feishu, title, mockReports)
	if err != nil {
		log.Fatalf("生成测试卡片失败: %v", err)
	}

	if dryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}

	if err := feishu.SendReport(ctx, cfg.Feishu, title, mockReports); err != nil {
		log.Fatalf("发送飞书测试卡片失败: %v", err)
	}
	log.Println("飞书 Card 2.0 测试卡片已成功发送到群聊！")
}

func runCheckAndNotify(
	ctx context.Context,
	cfg *config.Config,
	forceNotify bool,
	dryRun bool,
	lastAlertSent map[string]time.Time,
) {
	reports := checkAllAccounts(ctx, cfg)
	printReports(reports)

	if dryRun {
		log.Println("[dry-run] 已跳过飞书推送")
		return
	}

	now := time.Now()
	cooldown := time.Duration(cfg.Schedule.AlertCooldownMinutes) * time.Minute
	hasNewAlert := false
	anyAlert := false

	for _, r := range reports {
		if r.NeedsAlert() {
			anyAlert = true
			if lastAlertSent == nil {
				hasNewAlert = true
				continue
			}
			key := r.AlertKey()
			if last, ok := lastAlertSent[key]; !ok || now.Sub(last) >= cooldown {
				hasNewAlert = true
			}
		} else if lastAlertSent != nil {
			for k := range lastAlertSent {
				if len(k) > len(r.Provider)+len(r.AccountName)+2 &&
					k[:len(r.Provider)+len(r.AccountName)+1] == r.Provider+":"+r.AccountName {
					delete(lastAlertSent, k)
				}
			}
		}
	}

	if !forceNotify && !hasNewAlert {
		if anyAlert {
			log.Println("存在告警项，但仍在冷却期内，跳过重复飞书推送")
		} else {
			log.Println("所有账号额度正常，无需发送告警")
		}
		return
	}

	title := "模型额度与套餐监控日报"
	if anyAlert {
		title = "模型额度与凭证异常告警"
	}

	if err := feishu.SendReport(ctx, cfg.Feishu, title, reports); err != nil {
		log.Printf("发送飞书通知失败: %v", err)
		return
	}

	log.Println("飞书群通知发送成功")
	if lastAlertSent != nil {
		for _, r := range reports {
			if r.NeedsAlert() {
				lastAlertSent[r.AlertKey()] = now
			}
		}
	}
}

func checkAllAccounts(ctx context.Context, cfg *config.Config) []providers.AccountReport {
	type indexedReport struct {
		idx    int
		report providers.AccountReport
	}

	total := len(cfg.DeepSeek) + len(cfg.OpenRouter) + len(cfg.XiaomiMiMo)
	results := make([]providers.AccountReport, total)
	ch := make(chan indexedReport, total)
	var wg sync.WaitGroup

	idx := 0
	for _, acc := range cfg.DeepSeek {
		i, a := idx, acc
		idx++
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- indexedReport{idx: i, report: providers.CheckDeepSeek(ctx, a)}
		}()
	}

	for _, acc := range cfg.OpenRouter {
		i, a := idx, acc
		idx++
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- indexedReport{idx: i, report: providers.CheckOpenRouter(ctx, a)}
		}()
	}

	for _, acc := range cfg.XiaomiMiMo {
		i, a := idx, acc
		idx++
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- indexedReport{idx: i, report: providers.CheckXiaomiMiMo(ctx, a)}
		}()
	}

	wg.Wait()
	close(ch)

	for item := range ch {
		results[item.idx] = item.report
	}
	return results
}

func printReports(reports []providers.AccountReport) {
	fmt.Println("------------------------------------------------------------")
	for _, r := range reports {
		fmt.Printf("[%s] %s · %s (%s) | %s\n", r.Status, r.Provider, r.AccountName, r.TagText, r.Headline)
		for _, m := range r.Metrics {
			fmt.Printf("  • %s: %s\n", m.Label, m.Value)
		}
		if r.AlertReason != "" {
			fmt.Printf("  ! 告警: %s\n", r.AlertReason)
		}
	}
	fmt.Println("------------------------------------------------------------")
}
