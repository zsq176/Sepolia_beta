# Beta 交易平台

一个基于 Sepolia 测试网的交易演示平台，覆盖代币发行、Uniswap 多版本建池、自动做市与价格同步、以及面向用户的行情与账户控制台。

---

## 架构概览

```
.
├── issuer-service/          # 发行与流动性（Hardhat / TypeScript）
├── market-maker-service/    # 做市（Go）
└── user-console/            # 用户前端（React + Vite）
```

### 主体职责

- `issuer-service/`
  - 部署 BTC-Beta、USDT-Beta、ETH-Beta 三种 ERC20 代币。
  - 在 Uniswap V2 / V3 / V4 建池并注入初始流动性。
  - 执行 LP 销毁，归档证据清单。
- `market-maker-service/`
  - 同步 Binance 与 Uniswap V3 / V4 之间的目标价格。
  - 串联策略、风控、执行链路实现自动调价。
  - 提供公共行情、私有账户与参数治理接口，落审计日志。
- `user-console/`
  - 展示公共行情（K 线、最新价、系统状态）。
  - 通过钱包签名登录后展示私有账户数据（持仓、交易历史、操作记录）。
  - 提供受控的手工交互入口，便于演示和监控。

---

## 端到端运行顺序

推荐按下列顺序启动整套系统：

1. **发行与建池**：在 `issuer-service/` 部署代币并完成 V2/V3/V4 建池注资。
2. **做市服务**：启动 `market-maker-service/`，对接 Binance 与链上池子，承接 API。
3. **用户控制台**：启动 `user-console/`，连接做市服务的 API 与 WebSocket。

---

## 1. issuer-service（发行与流动性）

### 作用

- 部署 BTC-Beta、USDT-Beta、ETH-Beta。
- 在 Uniswap V2/V3/V4 建池并注入初始流动性。
- 执行 LP 销毁并输出证据清单。

### 常用命令

在 `issuer-service/` 目录下执行：

- `npm run compile`：编译合约与脚本。
- `npm run deploy`：部署三种 Beta 代币。
- `npm run v2`：配置 V2 BTC/USDT 交易对并处理 LP。
- `npm run v3`：配置 V3 BTC/USDT 交易对并处理 LP NFT。
- `npm run v4`：配置 V4 三个交易对并处理 LP NFT。

幂等说明：

- 默认按幂等方式执行，已存在/已注资的池会跳过重复操作。
- 如需强制追加流动性：
  - V2：设置 `V2_FORCE_ADD=1`
  - V3：设置 `V3_FORCE_ADD=1`
  - V4：设置 `V4_FORCE_ADD=1`

### 环境变量

复制 `issuer-service/.env.example` 为 `issuer-service/.env` 并填写，至少包含：

- `PRIVATE_KEY`
- `SEPOLIA_RPC`
- 可选已部署地址：`BTC_BETA_ADDR`、`USDT_BETA_ADDR`、`ETH_BETA_ADDR`

### 产物

执行脚本后会输出到：

- `issuer-service/reports/deployment-report.json`

其中包含代币地址、池地址、关键交易 Tx 等交付证据。

---

## 2. market-maker-service（做市服务）

### 作用

- 同步 Binance 与 Uniswap V3/V4 的目标价格关系。
- 通过策略、风控、执行链路进行自动调价。
- 提供公共行情接口、私有账户接口、参数治理与审计日志。

### 启动方式

在 `market-maker-service/` 目录下执行：

- `go run ./cmd/main.go`

### 核心能力

- 判定 TWAP + 执行 TWAP（分批执行）。
- 支持读、写 RPC多节点读故障切换和限流退避
- 风控门：偏离阈值、冷却、Gas 上限、日预算上限。
- 数据质量分级：`live / fallback / stale / invalid`。
- 账户隔离：基于钱包签名登录与 token 会话。
- Redis：审计异步落库队列 + instrument 分布式锁。

### 主要接口（节选）

- 公共：
  - `GET /api/market/prices`
  - `GET /api/market/klines`
  - `GET /api/snapshot`
- 私有（需鉴权）：
  - `GET /api/account/trades`
  - `GET /api/account/positions`
  - `GET /api/replay/decisions`
- 管理（operator/admin）：
  - `POST /api/admin/params`
  - `POST /api/sop/incidents`

### 配置

参考 `market-maker-service/.env.example` 填写 `.env`，重点配置：

- RPC 与私钥（支持多节点读故障切换）。
- 交易对地址与 PoolKey。
- 风控参数（`MAX_GAS_USD`、`DAILY_BUDGET_USD` 等）。
- 鉴权参数（`API_AUTH_SECRET`、`OPERATOR_WALLET`）。
- 中间件参数（`REDIS_ADDR`、`REDIS_QUEUE`、`REDIS_LOCK_TTL_SEC`）。

RPC 推荐配置：

- `SEPOLIA_RPC`：主 RPC（写交易与默认读请求入口）。
- `SEPOLIA_RPC_FALLBACKS`：备用 RPC 列表，逗号分隔；当主节点读失败时自动按顺序切换。
- 交易发送也会复用同一组备用节点做广播兜底（使用同一笔已签名交易，不会重复签名）。

---

## 3. user-console（用户控制台）

### 作用

- 展示公共行情（K 线、实时价格、状态）。
- 支持钱包登录并展示私有账户数据（持仓、交易历史）。
- 提供手工交互入口（用于演示和受控操作）。

### 启动命令

在 `user-console/` 目录下执行：

- `npm install`
- `npm run dev`

构建命令：

- `npm run build`
- `npm run preview`

### 数据分层

- 公共数据（无需鉴权）：
  - K 线、最新价、系统状态。
- 私有数据（需鉴权）：
  - 钱包持仓、交易历史、操作记录。

### 鉴权流程

1. 连接 MetaMask。
2. 获取 challenge 并签名。
3. 后端验签后发放会话 token。
4. 私有接口请求附带 `Authorization: Bearer <token>`。

---

## 部署步骤与验收清单

### 交付范围

- 发行与流动性主体：代币部署、建池注资、LP 销毁证据输出。
- 做市主体：V3/V4 价格同步、TWAP 分批执行、风控与成本控制。
- 用户前端主体：公共行情展示与按钱包隔离的账户数据展示。

### 部署步骤

1. **发行与建池**（`issuer-service`）
   - 填写 `issuer-service/.env`。
   - 依次执行：
     - `npm run deploy`
     - `npm run v2`
     - `npm run v3`
     - `npm run v4`
   - 检查 `issuer-service/reports/deployment-report.json`。

2. **做市服务**（`market-maker-service`）
   - 按 `market-maker-service/.env.example` 填写 `.env`。
   - 设置 `API_AUTH_SECRET` 与 `OPERATOR_WALLET`。
   - 在该目录下启动服务：`go run ./cmd/main.go`。

3. **用户前端**（`user-console`）
   - `npm install`
   - `npm run build` 或 `npm run dev`

### 验收证据清单

- 代币地址：BTC-Beta、USDT-Beta、ETH-Beta。
- V2/V3/V4 建池、注资、LP 销毁 Tx 记录（见 deployment report）。
- 做市决策与执行日志。

---

## 做市异常处理 SOP

### 覆盖异常类型

- Binance WebSocket 中断。
- RPC 超时或合约调用失败。
- Gas 突增超过阈值。
- Nonce 冲突或交易长期 pending。
- 价格源分歧或数据新鲜度下降。

### 状态机

- `Normal`：正常运行。
- `Degraded`：数据降级或临时基础设施异常。
- `Guarded`：连续失败或偏离异常，进入保护执行。
- `Halt`：预算超限或持续异常，暂停自动交易。
- `Recovering`：人工或自动恢复检查阶段。

### 检测与动作

1. 数据质量为 `stale` 或 `invalid`
   - 动作：拒绝交易决策。
   - 恢复：仅在恢复到 `live/fallback` 且满足新鲜度后重启。

2. Gas 超限或日预算超限
   - 动作：拒绝执行并记录原因。
   - 恢复：等待 Gas 回落或进入新 UTC 日预算窗口。

3. 连续执行失败
   - 动作：指数退避，必要时升级到 Guarded/Halt。
   - 恢复：出现一次确认成功后清空失败计数。

4. Nonce/pending 冲突
   - 动作：同一交易对暂停新单，等待前序交易结果。
   - 恢复：确认/失败后并满足冷却时间再恢复。