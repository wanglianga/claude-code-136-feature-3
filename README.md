# passgate — 景区年卡实名入园同行权益、异常核验与离线权益回补服务

基于 **Go + Fiber + Redis** 的景区年卡核验后端。覆盖：家庭/单人/亲子年卡售卖与实名信息管理、
闸机入园核验（人脸/证件/二维码/同行人/时段/容量）、异常核验原因可追溯、冻结/退款/升级权益重算、
现场人工放行留痕、客服申诉与权益返还、运营售卖规则与统计分析、
**闸机离线本地放行后的日志合并、重复扣次自动回补与通知、未绑定同行人现场复核、闸机离线质量排行**。

## 快速启动

依赖：Go 1.19+、Redis 5+（本机已在 127.0.0.1:6379 运行 Redis 7）。

```bash
# 1. 启动 Redis（若未运行）
redis-server --daemonize yes

# 2. 启动服务（默认端口 8080，空库自动写入演示数据）
go run .
# 或
go build -o passgate . && ./passgate

# 环境变量：PORT（默认 8080）、REDIS_ADDR（默认 127.0.0.1:6379）
```

```bash
# 方式二：Docker Compose 一键启动（app + Redis + Swagger UI）
docker compose up --build
# 服务:       http://localhost:8080
# Swagger UI: http://localhost:8081/docs/   （OpenAPI 文档：openapi.yaml）
```

启动后可见种子日志：示例年卡 家庭卡=P000001 单人卡=P000002 亲子卡=P000003。

## 本地验证方式

```bash
bash test/run.sh        # 一键：构建 → 重置 Redis → 重启服务 → 运行全部 112 项端到端断言
# 或服务已启动且为全新种子库时：
python3 test/e2e.py
```

> 说明：端到端场景固定在营业日 **2026-10-09**（国庆假期 10-01~08 之后，单人卡节假日不可用），
> 因此用例结果不受实际运行日期影响。

验证结论（实际执行）：`bash test/run.sh` 全量通过，输出 `RESULT: PASS=112 FAIL=0`，覆盖
正常放行、幂等重试、抖动去重、20 并发同/不同 request_id 均只扣 1 次、16 类失败原因、
人工放行留痕、冻结/解冻与预约一致性、申诉返还、退款/升级重算、信息变更、免刷脸人工确认、
出园释放容量、运营统计与日志查询，以及本期新增的
**离线日志合并、重复刷闸识别与权益自动回补+短信通知、未绑定同行人三种复核处置、
无法对账记录 PARTIAL、批次重放幂等、闸机离线质量排行（34 项新断言）**。

## 演示数据与测试账号

系统不设登录鉴权（演示），操作人通过请求头 `X-Operator: <姓名>` 或请求体 `operator` 字段标识，
所有敏感操作（冻结、人工放行、申诉处理、规则调整）均记录操作人。建议的演示身份：

| 角色 | 标识 | 用途 |
|---|---|---|
| 现场工作人员 | `staff-chen` | 人工核验放行、设置免刷脸、更新照片 |
| 客服 | `cs-wang` | 冻结/解冻、退款、升级、申诉处理、换绑 |
| 运营 | `ops-li` | 售卖规则调整、节假日配置 |

种子数据（空库启动自动写入）：

- 景点：`attr-lake` 镜湖(容量5000)、`attr-summit` 云顶山(2000)、`attr-garden` 百花园(3000)
- 闸机：`dev-east-01/02`（东门）、`dev-west-01`（西门）、`dev-south-01`（南门），60 秒无心跳判离线
- 年卡：`P000001` 家庭卡（张三，含配偶/孩子/**免刷脸老人张父**）、`P000002` 单人卡（王五）、`P000003` 亲子卡（赵六+孩子）
- 售卖规则：单人 399 元/100 人次/节假日不可用；亲子 699 元/200 人次/同行 1 人；家庭 1299 元/400 人次/同行 3 人
- 2026 年法定节假日已配置（国庆 2026-10-01~08 等）
- 演示短信验证码固定为 `123456`（换绑手机号时使用）

## 核心 API

### 年卡生命周期
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/passes` | 购卡（实名+人脸照片+证件+手机号+成员；一证一卡；按售卖规则快照发卡） |
| GET | `/api/passes/:id` | 年卡详情（实时剩余额度、成员、二维码 token） |
| GET | `/api/passes?phone=` | 按手机号查卡 |
| POST | `/api/passes/:id/freeze` `/unfreeze` | 冻结/解冻（**不影响已预约未入园订单**） |
| POST | `/api/passes/:id/refund` | 退款：按剩余有效期 × 80% 折算，权益清零，未用预约取消 |
| POST | `/api/passes/:id/upgrade` | 升级：补差价，**有效期与额度按新卡重新计算** |
| POST | `/api/passes/:id/phone` | 换绑手机号（验证码 123456） |
| POST | `/api/passes/:id/members` / DELETE `.../members/:mid` | 家庭成员临时更换（消耗更换次数） |
| POST | `/api/passes/:id/members/:mid/photo` | 更新人脸照片（孩子长大，版本+1） |
| POST | `/api/passes/:id/members/:mid/face-exempt` | 老人免刷脸标记 |
| POST | `/api/passes/:id/qr-refresh` | 刷新二维码（旧码立即失效） |
| GET | `/api/passes/:id/changes` | 变更日志（全部敏感操作留痕） |

### 入园核验
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/verify` | 闸机核验，返回 `ALLOW / MANUAL_REVIEW / QUOTA_INSUFFICIENT / SUSPECTED_LENDING` + 原因码 |
| GET | `/api/verifications/:id` | 核验详情（客服查看失败原因与人工处理链路） |
| POST | `/api/verifications/:id/manual-review` | 人工放行/拒绝（必须填操作人与说明，放行走同一原子扣减） |
| POST | `/api/exit` | 出园，释放在园容量 |

`/api/verify` 请求示例：

```json
{
  "request_id": "gate-east-01-000123",   // 闸机幂等键，重试必须不变
  "device_id": "dev-east-01",
  "qr_token": "QR...",                    // 或 pass_id / id_card（主入园人证件）
  "face_score": 88.5,
  "photo_clear": true,
  "companions": ["110101199203054321"],
  "attraction": "attr-lake",
  "timestamp": "2026-09-25T10:30:00+08:00",
  "offline_sync": false                   // true 表示闸机离线补传
}
```

失败/转人工原因码：`DEVICE_UNKNOWN` `DEVICE_OFFLINE` `PASS_NOT_FOUND` `PASS_FROZEN` `PASS_REFUNDED`
`PASS_EXPIRED` `PASS_NOT_STARTED` `ATTRACTION_NOT_ALLOWED` `HOLIDAY_BLOCKED` `TIMESLOT_BLOCKED`
`CAPACITY_FULL` `COMPANION_EXCEEDED` `COMPANION_NOT_BOUND` `FACE_MISMATCH` `FACE_EXEMPT_REVIEW`
`PHOTO_UNCLEAR` `ID_EXPIRED` `HIGH_FREQUENCY` `MULTI_LOCATION` `QUOTA_EXHAUSTED`（放行去重：`DUPLICATE_ENTRY`）。

### 设备 / 预约 / 申诉 / 运营
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/devices` `/api/devices/:id/heartbeat` | 设备注册、心跳 |
| GET | `/api/devices` | 设备列表（含在线状态） |
| POST | `/api/reservations` `/api/reservations/:id/cancel` | 预约/取消（冻结卡不可新预约，已有预约保留） |
| POST | `/api/appeals` `/api/appeals/:id/resolve` | 申诉；客服通过自动返还误扣额度 |
| GET/PUT | `/api/holidays` | 节假日配置 |
| GET/PUT | `/api/admin/sales-rules[/:type]` | 售卖规则查询与调整（仅影响新售卡） |
| GET | `/api/admin/stats/overview` `/stats/peaks` `/gate-logs` | 按卡类型售卖/权益使用率/异常率、入园峰值、闸机流水 |

### 闸机离线权益回补（本期扩展）
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/offline/batches` | 闸机恢复后**整批上报离线日志**并触发合并：识别重复扣次→自动回补→短信通知；未绑定同行人生成复核单；累计设备离线指标 |
| GET | `/api/offline/batches[?limit]` `/api/offline/batches/:id` | 离线合并批次列表/详情（含每个游客分组的 head_count / charged_total / refund_quota / status） |
| GET | `/api/offline/reviews[?status=PENDING]` | 未绑定同行人入园的现场复核单 |
| POST | `/api/offline/reviews/:id/resolve` | 现场处置：`REGISTER` 补登记（不追扣，受成员更换名额约束）/ `CHARGE` 追扣 1 人次 / `ANOMALY` 标记异常放行；处置后短信通知游客 |
| GET | `/api/passes/:id/offline-reviews` | 年卡视角的复核单 |
| GET | `/api/notifications[?pass_id=]` | 游客通知（回补/复核结果；短信内容同时留库 `sms:out`） |
| GET | `/api/admin/devices/offline-ranking` | **闸机离线排行**：离线次数/累计时长/离线放行数/已合并批次/回补额度/待复核/异常放行/风险分，供维护计划与高峰入口调度参考 |

`POST /api/offline/batches` 请求示例：

```json
{
  "device_id": "dev-offline-02",
  "offline_from": "2026-10-09T09:00:00+08:00",
  "offline_to":   "2026-10-09T11:30:00+08:00",
  "operator": "staff-chen",
  "events": [
    {
      "request_id": "M-E1",
      "event_id": "M-E1",
      "principal_id_card": "110101199001011234",
      "companions": ["110101199203054321"],
      "companion_names": {"110101199203054321": "李四"},
      "face_score": 92.0,
      "local_time": "2026-10-09T10:30:00+08:00"
    }
  ]
}
```

新增原因码：`OFFLINE_DUP_REFUND`（离线重复扣次已回补）、`OFFLINE_UNBOUND_PERSON`（离线期间未绑定同行人）、
`OFFLINE_ANOMALY_RELEASE`（离线异常放行）。批次 `status`：`PROCESSED` / `PARTIAL`（含无法对账 `NO_PASS`
或额度不足 `UNRESOLVED` 分组）；分组 `status`：`MERGED_NO_DUP` / `REFUNDED` / `UNRESOLVED` / `NO_PASS`。

## 关键设计

- **幂等防重（网络抖动/闸机重试）**：`request_id` 先 `SET NX` 占位，重试直接返回首次结果；
  另有 90 秒「同卡同景点放行去重窗口」，重复刷闸按首次放行处理不重复扣次；
  扣减由 **Lua 脚本原子完成**（去重判定+额度+容量+写去重标记），多入口多设备并发安全。
- **疑似转借**：10 分钟窗口内核验 >4 次（`HIGH_FREQUENCY`）或出现在 ≥2 个景点（`MULTI_LOCATION`）→
  `SUSPECTED_LENDING`；一证一卡源头约束。
- **人工放行可解释**：人工处理强制操作人+说明，写 `ManualReview`、更新核验记录、写闸机日志
  `MANUAL_RELEASE`，客服 `GET /api/verifications/:id` 可见完整链路；同一核验仅可处理一次。
- **权益一致性**：额度以 Redis 计数器为权威；申诉通过自动返还；退款清零；升级按新卡重置并重算有效期；
  冻结/解冻只改状态，不触碰预约与额度。
- **在线离线两条核验路径**：
  - 在线：心跳超时判离线，正常请求转人工；闸机离线期间自行放行年卡用户。
  - 离线补传（`/api/verify` 的 `offline_sync=true`）：单条补传，凭 `request_id` 幂等，不做容量与在线去重判定。
- **离线日志合并与权益回补**（`internal/service/offline.go`）：
  1. 整批日志按 `request_id` 幂等（已合并跳过，整批重复返回 409）；
  2. 按「年卡 + 景点 + 日期 + 主入园人证件」聚合为入园事件组，事件按闸机本地时间排序；
  3. 每条离线日志按**闸机当时放行人数**（主+当次已绑定同行）审计出本地扣次，
     组内按人去重得到**实际应扣人次**（最早入园者保留），两者之差即重复扣次；
     每组由 **Lua 脚本原子完成「回放本地总扣次 → 回补重复部分」**（净扣=实际入园人次，
     计数器瞬时状态不外暴），核验记录标 `OFFLINE_DUP_REFUND`、写 `OFFLINE_REFUND` 变更日志，
     并向持卡人发短信（`sms:out` 留痕）；
  4. 未绑定同行人**不计入扣次**，生成 `PENDING` 复核单，现场三选一处置且仅可处置一次；
  5. 无法匹配年卡的离线放行落 `NO_PASS`、组内实际入园人次超出余额落 `UNRESOLVED`，
     批次标记 `PARTIAL` 转人工核查，绝不静默吞掉；
  6. 组级扣减失败时统一回滚已入账净额并释放本批幂等占位，整批可安全重试；
     离线合并不写 90 秒在线去重标记，恢复后的在线核验不受影响。
- **闸机离线质量度量**：每次合并累计设备维度的离线次数、离线时长、离线放行条数、回补额度、
  待复核与异常放行数，`/api/admin/devices/offline-ranking` 按风险分降序输出，
  为后续维护计划（优先维修高风险闸机）与高峰入口调度（薄弱入口增配人力/备用闸机）提供数据依据。

## 目录结构

```
main.go                  入口（依赖装配、种子、HTTP 服务）
internal/model/          实体定义
internal/store/          Redis 封装（JSON 存取、ID 序列、分布式锁）
internal/service/        业务层：pass.go 年卡生命周期 / verify.go 核验引擎 /
                         ops.go 设备·预约·申诉·统计 / offline.go 离线合并·回补·复核·离线指标
internal/handler/        Fiber 路由与错误映射
internal/seed/           演示数据
test/e2e.py              112 项端到端断言
test/run.sh              一键验证（重置环境后全量跑）
openapi.yaml             OpenAPI 3.x 接口文档（docker compose 启动后 Swagger UI 可浏览）
Dockerfile / docker-compose.yml  一键容器化启动（app + Redis + Swagger UI）
```
