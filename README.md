# Price Desk · 多交易所比价

一页对比同一交易对在 13 家中心化交易所与 7 个链上去中心化场所的最新价、买一/卖一、24h 涨跌与成交额，并实时计算跨所价差。
线上：https://price.tsuibo.xyz

## 形态

- `price-compare`：单一 Go 静态二进制（无第三方依赖），监听 `127.0.0.1:8990`，由 systemd 托管。
- `static/`：纯前端（`index.html` + `app.js`），由 host nginx 静态托管；`/api/` 反代到本机进程。
- 浏览器无任何外部请求（无 CDN/字体），CSP `default-src 'self'`，数据只经同源 `/api/`。

## 交易所与报价

| 交易所 | 市场 | 计价 |
| --- | --- | --- |
| Binance / OKX / Bybit / Bitget / Gate / KuCoin / MEXC / HTX / Kraken / Bitstamp / Bitfinex / Crypto.com | `<BASE>USDT` 等各所格式 | USDT |
| Coinbase | `<BASE>-USD` | USD → 按实时 USDT/USD 折算 |

### 链上 DEX（`dex.go`）

| 场所 | 链 | 索引 |
| --- | --- | --- |
| Uniswap | Ethereum / Base / Arbitrum | 该链 Uniswap 池 |
| PancakeSwap | BNB Chain | PancakeSwap 池 |
| Aerodrome | Base | `aerodrome` / `aero` 池 |
| Raydium / Orca | Solana | 各自 AMM 池 |

- 每个 DEX 行代表「该场所在该链上的一个池子」：在命中的池子里优先选 24h 有成交（≥ $1,000）的，再取流动性最深的；全是死池时退回最深的一个。零成交池的「最新价」是陈旧成交价，不能当作该场所报价。
- 币种识别用**合约地址白名单**（`dexTokens`），不用符号搜索。符号搜索会返回大量同名假币，且假币往往自报超深流动性；地址经实时 API 校验后才写进表。
- 未列入白名单的币种在该链上显示「未上架」，与「交易所故障」区分。
- DEX 只有成交价、没有盘口：`bid`/`ask` 为 0，页面上显示 `—`，也不参与「最低卖一 / 最高买一」比较，只计入中位价与离散度。
- 池子流动性低于 `$250k` 时标 `low_liquidity`，页面打「低流动」标签并**排除在中位价/价差计算之外**（仍然展示），因为一笔小额交易就能推动这种池子的价格。
- 数据源：DexScreener 公共 API（免 key），按 token 地址取全部交易对，进程内缓存 4s；同一链上的多个场所共用一次上游请求。

### 归一化

- USD 报价（Coinbase）与链上池（DEX）按 Kraken `USDT/USD` ticker 折算为 USDT（一分钟缓存）。取不到汇率时回退 1.0 并标记 `usdt_usd_source=fallback`，不伪造汇率。
- 24h 涨跌：各所字段口径不同的地方已逐家换算（MEXC/Bitget/KuCoin/Bybit/Crypto.com 返回比例，Binance/Gate/Bitstamp 返回百分数）；Kraken 不提供 24h 基准价，`change_24h` 明确为 `null`，页面上显示 `—`。
- 未上架（`not_listed`）与请求失败（`error`）分开标记，不把「没有这个市场」显示成「交易所故障」。
- 24h 成交额统一为计价币金额（USD 所与 DEX 按汇率折算），Kraken/Bitfinex/Coinbase/Bitstamp 用 24h 量 × 均价或最近价估算，量级参考用。

## 脱锚监测（detector）

页面是「打开才抓」的快照；监测是常驻后台的采样与判定，用来回答「某个场所的价格，是否已经偏离它自己的常态，且偏离幅度不是噪音」。

- **采样**：daemon 以 `-interval`（默认 60s）遍历 `symbolCatalog` 全部币种；基线未就绪时用 `-warm-interval`（默认 20s）加速预热。单轮约 4–6s，29 币种 × 20 场所。
- **共识价**：同一币种各 CEX 最新价的**中位数**（至少 4 家有效才计算）。DEX 不参与共识，只被比较。
- **偏离**：每个场所 `dev_bps = (price / consensus − 1) × 10000`，写入该币种的环形缓冲（默认 180 个样本，1 分钟采样约 3 小时）。
- **基线**：每个「币种 × 场所」的偏离序列取**中位数 + MAD**（`1.4826 × MAD` 作 σ 估计）。用 MAD 而不是标准差，是因为要检测的那一根异常价不能同时把基线抬高。因此「某所长期比共识贵 20bps」不会被当成事件，只有**相对于自身常态的变化**才会。
- **触发**：`|dev − 基线| > max(4σ, 下限)` 且 `|dev| > 绝对下限`，并**连续 2 个样本**成立。下限：CEX 45bps / 绝对 25bps；DEX 150bps / 绝对 60bps（池子最新成交价可能陈旧，且薄池已排除）。
- **冻结基线**：某场所一旦进入报警状态，此后它的样本**不再进入该场所的基线**。否则一次持续数小时的脱锚会被基线慢慢吸收成「正常」，然后在价格仍偏离几百 bps 时发出假「回归」——线上确实出现过（WLD/HTX 从 301bps 涨到 346bps 却报回归），现有单测 `TestDetectorDoesNotAbsorbSustainedDislocation` 锁死该行为。
- **冷却**：报警存续期间不再按时间重复推送，只有偏离扩大到上次的 1.6 倍以上才再报（持续脱锚不该变成定时器）；回到基线区间发一次「回归」并清除状态；清除后的 30 分钟内若再次越界且幅度未扩大，则抑制，避免阈值附近抖动刷屏。
- **整币离散度**：`dispersion_bps` 超过 `max(2.5 × 自身基线, 150bps)` 连续 2 次 → 报「报价离散」，捕捉没有单一场所越界、但全场分歧的情况。
- **稳定币锚**：Kraken `USDT/USD`、`USDC/USD`，`|价 − 1| > 50bps` 即告警（绝对口径，不跟滚动基线）；回锚发一次恢复。
- **行情中断**：某场所对该币种**曾经成功报价 ≥3 次**，之后连续 5 次失败 → 「行情中断」，恢复后发「行情恢复」。从未报过价的场所（例如对上架范围外交易对返回 HTTP 500）不计为故障，避免每个币种刷一次假中断。中断/恢复按**场所**去重冷却，不会因为 29 个币种各发一遍。
- **重启后的基线**：优先用 `baseline.json` 的 prior 继续判定，但 ①prior 超过 60 分钟即作废（旧基线属于另一个行情区间）；②阈值翻倍、确认样本 +1；③至少 3 个新样本才判定；④prior 模式下只认**扩大**方向，溢价回落视为收敛而不是脱锚（实测中 Bitfinex UNI 从 +355bps 回到 +40bps 曾被误报成 alert 并发邮件）。prior 模式下不冻结样本，否则环形缓冲永远填不满、告警也永远解除不了。
- **告警通道**：`/etc/price-alert.env`（600），多通道同时生效，journal 始终保留一份。
  - Telegram：`PRICE_ALERT_TELEGRAM_TOKEN` + `PRICE_ALERT_TELEGRAM_CHAT`。
  - 邮件（任一传输即可）：收件人 `PRICE_ALERT_EMAIL_TO`（逗号分隔）；SMTP 组 `PRICE_ALERT_SMTP_HOST/PORT/USER/PASS/TLS`（`TLS=implicit` 对应 465，`starttls` 对应 587；AUTH 先试 PLAIN 再试 LOGIN，兼容 QQ/Gmail/Mailgun/SendGrid/Brevo/SES），或 Resend 组 `PRICE_ALERT_RESEND_KEY`（只需一个 key，`FROM` 默认 `onboarding@resend.dev`）。
  - 邮件默认只发**需要人处理**的事件（`severity=alert`、行情中断、脱锚回归、稳定币回锚），避免 watch 级样本塞满邮箱；`PRICE_ALERT_EMAIL_ALL=1` 可全量。
  - 当前线上**只启用邮件**：Resend（`onboarding@resend.dev` 发件）→ `you@example.com`，`/api/status` 显示 `notifier=email`；Telegram 已按用户要求停用（代码保留该通道，加回两个环境变量即可恢复）。
  - 坑一：`PRICE_ALERT_EMAIL_FROM` 含 `<...>` 时必须写引号（`"Price Desk <onboarding@resend.dev>"`），否则 shell `source` 会报 syntax error。
  - 坑二：Resend 的 send-only key 不能调 `GET /emails/{id}` 查投递状态（401 `restricted_api_key`），验证只能看发送返回的 `id` 与日志 `email: resend accepted id=...`。
  - 未配置任何通道时只写 journal，`/api/status` 显示 `notifier=log`，不假装推送；配了收件人但没配传输会在日志里明确报错。
- **状态文件**：`/var/lib/price-compare/events.jsonl`（事件追加，重启后回读最近 500 条）、`baseline.json`（每 10 轮快照，重启后 6h 内可直接接管基线，不必重新预热）。

### 阈值调参（不用改代码）

全部走 `/etc/price-alert.env`，改完 `systemctl restart price-compare`，启动日志会打印生效值 `monitor: thresholds cex=.. dex=.. peg=.. confirm=..`。

| 变量 | 默认 | 作用 |
| --- | --- | --- |
| `PRICE_ALERT_CEX_BPS` | 45 | CEX 判定的最小阈值（与 4σ 取大） |
| `PRICE_ALERT_DEX_BPS` | 150 | DEX 判定的最小阈值 |
| `PRICE_ALERT_CEX_ABS_BPS` / `_DEX_ABS_BPS` | 25 / 60 | 绝对偏离下限，低于此值即使越过基线也不报 |
| `PRICE_ALERT_PEG_BPS` | 50 | 稳定币对美元脱锚阈值 |
| `PRICE_ALERT_CONFIRM` | 2 | 连续多少个样本才成立 |
| `PRICE_ALERT_EMAIL_MIN_BPS` | 100（线上 300） | **只有偏离 ≥ 该值的 alert 才进邮箱**；watch 永不进邮箱 |
| `PRICE_ALERT_EMAIL_ALL` | 0 | 设为 1 则全量发邮件（含 watch） |

定阈值的依据（2026-10-31 线上 349 个「币种×场所」横截面）：`|偏离 − 自身基线|` 的 CEX p50=2.6bps、p90=15.7bps、p99=42.9bps、max=83.9bps；DEX max=67.8bps。也就是说 45bps 的下限已经卡在 CEX 的 p99 之外，日常噪音基本不触发；把下限抬到 1000bps（10%）意味着除了灾难性事件几乎永不告警，而交易所被黑/暂停提币造成的折价通常在 0.3%–3%。稳定币同理：USDT 历史最低约 0.95（-5%）、USDC 2023 年最低约 0.87，5% 的阈值等于在崩盘尾部才通知。

## API

- `GET /api/compare?base=BTC` → 全部场所报价 + 汇总（中位价、最低卖一、最高买一、跨所价差、离散度）。每条报价带 `kind`（`cex`/`dex`），DEX 另带 `chain`/`chain_name`/`market`/`liquidity_usd`/`pool`/`low_liquidity`。
- `GET /api/symbols` → 内置热门币种目录（也可直接传任意 2–12 位 `[A-Z0-9]` 代码）＋场所数（`exchanges`、`dex_exchanges`）。
- `GET /api/healthz` → 健康检查（含 `dex` 数量与 `monitor` 开关）。
- `GET /api/status` → 监测状态：`enabled`、`interval_s`、`ready_symbols/total_symbols`、`warmup`、`alerts_open`、`notifier`、`cycles`、`last_cycle_at`、`last_cycle_ms`。
- `GET /api/events?limit=50&since=<ms>` → 最近事件（新→旧），最多 200 条。
- `GET /api/signals?base=BTC` → 该币种每个场所的当前偏离、基线、MAD、阈值、样本数与状态（`ok|watch|alert|warmup`）。

结果按 base 缓存 2s，并对同名请求做并发合并（singleflight），避免前端自动刷新放大成对交易所的请求风暴。

## 部署

```bash
cd /srv/price-compare
go build -o price-compare .
install -m 0644 deploy/price-compare.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now price-compare
mkdir -p /var/www/price && cp static/index.html static/app.js /var/www/price/
install -m 0644 deploy/price.tsuibo.xyz.conf /etc/nginx/conf.d/
nginx -t && nginx -s reload
```

回滚：`cp -a /etc/nginx/conf.d/price.tsuibo.xyz.conf.bak.<ts> ...` → `nginx -t` → reload。
只改前端时无需 reload nginx：覆盖 `/var/www/price/` 下的文件即可（vhost 对 `index.html`/`app.js` 设了 `no-store`）。

## 验证

```bash
curl -s http://127.0.0.1:8990/api/healthz
curl -s 'http://127.0.0.1:8990/api/compare?base=BTC' | jq '.summary, [.quotes[] | select(.kind=="dex") | {exchange, market, price_usdt, liquidity_usd, low_liquidity}]'
curl -sI https://price.tsuibo.xyz/ | grep -i content-security
go test ./...    # 解析器 fixture + DEX 选池/白名单 + 汇总/价差计算
```

DEX 改动后除单测外，应对照 CEX 中位价抽查：任一 DEX 价格与中位价差出 1% 以上，先查是不是选错了池子或币种地址。

监测侧：

```bash
curl -s http://127.0.0.1:8990/api/status | jq
curl -s 'http://127.0.0.1:8990/api/signals?base=BNB' | jq '.signals[] | {venue,dev_bps,baseline_bps,threshold_bps,samples,state}'
curl -s 'http://127.0.0.1:8990/api/events?limit=20' | jq '.events[] | {at,kind,symbol,venue,dev_bps,severity}'
set -a; . /etc/price-alert.env; set +a; ./price-compare -test-alert   # 验证告警通道
```

判定是否误报看两件事：预热结束后 `ready_symbols == total_symbols` 且事件为 0；`/api/signals` 里 DEX 行的 `baseline_bps` 应稳定在一个非零的常数上（例如 BNB 在 Pancake 上长期 +30~40bps），说明基线吸收的是结构性价差而不是漂移。

## 已知边界

- CEX 只有现货 ticker，不含合约、资金费率与深度；买一/卖一是单档盘口，大额成交会穿透。
- DEX 价格是池子最近一笔成交价，不是报价；没有深度曲线，大额兑换的滑点、池子手续费、gas、MEV 都未计入。
- DEX 行只覆盖白名单内的币种与链，长尾币种会显示「未上架」；同一场所可能有多条链的多个池子，页面一行只代表其中最深且活跃的那个。
- 正价差（`cross_spread_bps > 0`）只代表盘口或池子价格重叠，未计手续费、滑点、提币成本与链上 gas，不代表可套利。
- 各所/DEX 推送频率不同，几秒级的价差可能只是刷新时差；页面 5s 刷新，2s 服务端缓存，DEX 上游缓存 4s。
- 监测只用公开 ticker，**不含提币/充币状态**。脱锚方向取决于哪一侧被冻结，服务只能看到价格偏离，不能判断方向，也无法替代交易所签名 API 的 `withdrawEnable`。
- 监测覆盖 `symbolCatalog`（29 币种）与 `dexTokens` 白名单；长尾币种被盗后砸盘不在视野内。稳定币只覆盖 USDT/USDC 对美元，不含 FDUSD/DAI 及各链池子。
- DEX 侧是「最近一笔成交价」，没有成交时间戳；长期无成交但有成交记录的池子可能给出陈旧价。已用「24h 成交额 ≥ $1k + 流动性 ≥ $250k」过滤，但这不等于价格新鲜。
- 告警是**只读**的：不连接交易、不下单，也不做任何自动处置。
