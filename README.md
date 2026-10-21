# Rattler

轻量零外部依赖的 Go 模型额度监控与飞书群卡片 2.0 提醒工具，支持同时巡检：
- **DeepSeek**（账户 CNY / USD 充值与赠送余额）
- **OpenRouter**（账户总 Credits 剩余额度 / 单 Key 限额回退）
- **小米 MiMo TokenPlan**（月度套餐 Token 已用比例、剩余 Token、重置时间、现金余额及 Cookie 过期告警）

## 特性

- **飞书卡片 2.0（`schema: "2.0"`）官方监控看板样式**：三态主题色（正常浅蓝、凭证过期橙色、额度不足红色）、顶部 KPI 汇总栏、每渠道三列等宽指标网格、行内状态胶囊标签（`<text_tag>`）与底部一键跳转充值/管理后台按钮。
- **双模式运行**：支持单次巡检（`-once`，适合 Crontab / 计划任务）与常驻守护进程（定时巡检 + 每日定时汇总日报 + 告警冷却防刷屏）。
- **小米 MiMo 登录态过期识别**：自动拦截 `3xx` SSO 重定向与 JSON 内 `401/403`，第一时间推送 Cookie 过期提醒。

## Docker 部署（GHCR 镜像）

1. 准备配置文件 `config.json`（参考 [`config.example.json`](config.example.json)）：
   ```bash
   cp config.example.json config.json
   ```

2. 使用 Docker 或 Docker Compose 启动：
   ```bash
   docker run -d \
     --name rattler \
     --restart unless-stopped \
     -v $(pwd)/config.json:/app/config.json:ro \
     ghcr.io/nasa1024/rattler:0.1.0
   ```

   或使用环境变量直接运行：
   ```bash
   docker run -d \
     --name rattler \
     --restart unless-stopped \
     -e FEISHU_WEBHOOK_URL="https://open.feishu.cn/open-apis/bot/v2/hook/..." \
     -e DEEPSEEK_API_KEY="sk-..." \
     -e OPENROUTER_API_KEY="sk-or-v1-..." \
     -e XIAOMI_MIMO_COOKIE="api-platform_serviceToken=...; userId=..." \
     ghcr.io/nasa1024/rattler:0.1.0
   ```

## 本地编译与命令行参数

```bash
go build -o rattler .

# 发送仿真测试卡片到飞书群预览效果
./rattler -test-card

# 在终端预览卡片 2.0 JSON 结构（不发飞书）
./rattler -test-card -dry-run

# 立即执行一次真实检查并强制发送飞书汇总卡片
./rattler -once -force

# 仅执行一次检查（有告警才发飞书）
./rattler -once
```
