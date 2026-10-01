package model

import "time"

// PassType 年卡类型
type PassType string

const (
	PassTypeSingle      PassType = "single"       // 单人年卡
	PassTypeFamily      PassType = "family"       // 家庭年卡
	PassTypeParentChild PassType = "parent_child" // 亲子年卡
)

// PassStatus 年卡状态
type PassStatus string

const (
	PassStatusActive   PassStatus = "active"
	PassStatusFrozen   PassStatus = "frozen"
	PassStatusRefunded PassStatus = "refunded"
)

// Decision 核验结论
type Decision string

const (
	DecisionAllow   Decision = "ALLOW"              // 放行
	DecisionManual  Decision = "MANUAL_REVIEW"      // 人工核验
	DecisionQuota   Decision = "QUOTA_INSUFFICIENT" // 权益不足
	DecisionSuspect Decision = "SUSPECTED_LENDING"  // 疑似转借
)

// 核验失败 / 转人工原因码
const (
	ReasonNone              = ""
	ReasonDeviceUnknown     = "DEVICE_UNKNOWN"
	ReasonDeviceOffline     = "DEVICE_OFFLINE"
	ReasonPassNotFound      = "PASS_NOT_FOUND"
	ReasonPassFrozen        = "PASS_FROZEN"
	ReasonPassRefunded      = "PASS_REFUNDED"
	ReasonPassExpired       = "PASS_EXPIRED"
	ReasonPassNotStarted    = "PASS_NOT_STARTED"
	ReasonAttractionDenied  = "ATTRACTION_NOT_ALLOWED"
	ReasonHolidayBlocked    = "HOLIDAY_BLOCKED"
	ReasonTimeslotBlocked   = "TIMESLOT_BLOCKED"
	ReasonCapacityFull      = "CAPACITY_FULL"
	ReasonCompanionExceeded = "COMPANION_EXCEEDED"
	ReasonCompanionNotBound = "COMPANION_NOT_BOUND"
	ReasonFaceMismatch      = "FACE_MISMATCH"
	ReasonFaceExemptReview  = "FACE_EXEMPT_REVIEW"
	ReasonPhotoUnclear      = "PHOTO_UNCLEAR"
	ReasonIDExpired         = "ID_EXPIRED"
	ReasonHighFrequency     = "HIGH_FREQUENCY"
	ReasonMultiLocation     = "MULTI_LOCATION"
	ReasonQuotaExhausted    = "QUOTA_EXHAUSTED"
	ReasonDuplicateEntry    = "DUPLICATE_ENTRY" // 网络抖动重复请求 / 离线重复刷闸，视为同一次入园
)

// Member 年卡成员（持卡人 + 同行成员）
type Member struct {
	ID           string    `json:"id"`
	PassID       string    `json:"pass_id"`
	Name         string    `json:"name"`
	IDCard       string    `json:"id_card"`
	IDCardExpiry string    `json:"id_card_expiry"` // YYYY-MM-DD
	Role         string    `json:"role"`           // holder | companion
	Relation     string    `json:"relation"`       // self | spouse | child | parent | other
	PhotoURL     string    `json:"photo_url"`
	PhotoVersion int       `json:"photo_version"`
	FaceExempt   bool      `json:"face_exempt"` // 老人等无法刷脸成员
	CreatedAt    time.Time `json:"created_at"`
}

// Pass 年卡
type Pass struct {
	ID               string     `json:"id"`
	Type             PassType   `json:"type"`
	Status           PassStatus `json:"status"`
	HolderName       string     `json:"holder_name"`
	HolderIDCard     string     `json:"holder_id_card"`
	Phone            string     `json:"phone"`
	ValidFrom        string     `json:"valid_from"`  // YYYY-MM-DD
	ValidUntil       string     `json:"valid_until"` // YYYY-MM-DD
	TotalQuota       int        `json:"total_quota"`
	CompanionLimit   int        `json:"companion_limit"`
	Attractions      []string   `json:"attractions"`
	HolidayBlocked   bool       `json:"holiday_blocked"`
	AllowedSlots     []string   `json:"allowed_slots"` // ["08:00-18:00"]
	MemberChangeLeft int        `json:"member_change_left"`
	PaidAmount       float64    `json:"paid_amount"`
	FreezeReason     string     `json:"freeze_reason,omitempty"`
	FrozenAt         *time.Time `json:"frozen_at,omitempty"`
	RefundedAt       *time.Time `json:"refunded_at,omitempty"`
	RefundAmount     float64    `json:"refund_amount,omitempty"`
	UpgradedFrom     string     `json:"upgraded_from,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// PassView 年卡详情响应（合并 Redis 计数器中的实时额度）
type PassView struct {
	Pass
	RemainingQuota int      `json:"remaining_quota"`
	UsedQuota      int      `json:"used_quota"`
	Members        []Member `json:"members"`
	QRToken        string   `json:"qr_token"`
}

// Verification 一次闸机核验记录（无论成败均落库，客服可查）
type Verification struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"request_id"` // 闸机侧幂等键
	PassID       string    `json:"pass_id"`
	PassType     string    `json:"pass_type"`
	HolderName   string    `json:"holder_name"`
	DeviceID     string    `json:"device_id"`
	GateID       string    `json:"gate_id"`
	Attraction   string    `json:"attraction"`
	Decision     Decision  `json:"decision"`
	Reason       string    `json:"reason"`
	Detail       string    `json:"detail"` // 人类可读说明，客服解释用
	FaceScore    float64   `json:"face_score"`
	Companions   []string  `json:"companions"`
	ChargedQuota int       `json:"charged_quota"`
	DuplicateOf  string    `json:"duplicate_of,omitempty"`
	OfflineSync  bool      `json:"offline_sync"`
	ManualAction string    `json:"manual_action,omitempty"` // RELEASED | REJECTED
	CreatedAt    time.Time `json:"created_at"`

	// 离线合并扩展字段
	OfflineBatchID  string `json:"offline_batch_id,omitempty"`  // 所属离线合并批次
	OfflineLocalSeq string `json:"offline_local_seq,omitempty"` // 闸机本地流水号（离线日志幂等键）
	PrincipalIDCard string `json:"principal_id_card,omitempty"` // 主入园人证件（离线时无法定位卡也需留痕）
	Name            string `json:"name,omitempty"`              // 主入园人姓名（离线设备读到的姓名，复核辅助）
	// ReversedBy 非空表示该次离线扣减已在合并时判定为重复扣次并回补（值为回补单 ID）
	ReversedBy string `json:"reversed_by,omitempty"`
}

// ManualReview 现场人工处理记录（人工放行必须留痕，客服可解释）
type ManualReview struct {
	ID             string    `json:"id"`
	VerificationID string    `json:"verification_id"`
	PassID         string    `json:"pass_id"`
	Action         string    `json:"action"` // RELEASE | REJECT
	Operator       string    `json:"operator"`
	Note           string    `json:"note"`
	ChargedQuota   int       `json:"charged_quota"`
	CreatedAt      time.Time `json:"created_at"`
}

// GateLog 闸机流水
type GateLog struct {
	ID             string    `json:"id"`
	VerificationID string    `json:"verification_id"`
	PassID         string    `json:"pass_id"`
	DeviceID       string    `json:"device_id"`
	GateID         string    `json:"gate_id"`
	Attraction     string    `json:"attraction"`
	Direction      string    `json:"direction"` // ENTRY | EXIT
	Action         string    `json:"action"`    // ALLOW | DENY | MANUAL_RELEASE | EXIT
	Reason         string    `json:"reason"`
	QuotaCharged   int       `json:"quota_charged"`
	Operator       string    `json:"operator,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Device 闸机设备
type Device struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	GateID        string    `json:"gate_id"`
	Attraction    string    `json:"attraction"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	RegisteredAt  time.Time `json:"registered_at"`
}

// Reservation 预约单（冻结/解冻不影响已预约未入园订单）
type Reservation struct {
	ID         string    `json:"id"`
	PassID     string    `json:"pass_id"`
	Attraction string    `json:"attraction"`
	Date       string    `json:"date"` // YYYY-MM-DD
	Slot       string    `json:"slot"`
	Status     string    `json:"status"` // BOOKED | USED | CANCELLED
	CreatedAt  time.Time `json:"created_at"`
}

// Appeal 客服申诉单
type Appeal struct {
	ID              string     `json:"id"`
	PassID          string     `json:"pass_id"`
	VerificationID  string     `json:"verification_id,omitempty"`
	Type            string     `json:"type"` // WRONG_DEDUCTION | VERIFY_FAIL | FREEZE_DISPUTE | OTHER
	Description     string     `json:"description"`
	Status          string     `json:"status"` // PENDING | APPROVED | REJECTED
	Resolution      string     `json:"resolution,omitempty"`
	CompensateQuota int        `json:"compensate_quota,omitempty"`
	Handler         string     `json:"handler,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
}

// Attraction 景点（容量配置）
type Attraction struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// SalesRule 售卖规则（运营可调，仅影响新售卡；已售卡为快照）
type SalesRule struct {
	PassType          PassType  `json:"pass_type"`
	Price             float64   `json:"price"`
	TotalQuota        int       `json:"total_quota"`
	CompanionLimit    int       `json:"companion_limit"`
	Attractions       []string  `json:"attractions"`
	HolidayBlocked    bool      `json:"holiday_blocked"`
	AllowedSlots      []string  `json:"allowed_slots"`
	ValidDays         int       `json:"valid_days"`
	MemberChangeLimit int       `json:"member_change_limit"`
	OnSale            bool      `json:"on_sale"`
	DailyLimit        int       `json:"daily_limit"` // 每日限售数量，0 表示不限
	UpdatedAt         time.Time `json:"updated_at"`
	UpdatedBy         string    `json:"updated_by"`
}

// ChangeLog 年卡变更日志（换手机号、换成员、冻结、退款、升级等均留痕）
type ChangeLog struct {
	ID        string    `json:"id"`
	PassID    string    `json:"pass_id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Operator  string    `json:"operator"`
	CreatedAt time.Time `json:"created_at"`
}

// ==================== 闸机离线日志合并与权益回补 ====================

// 离线批次状态
const (
	BatchStatusMerged   = "MERGED"   // 已合并（可能仍有待复核任务）
	BatchStatusConflict = "CONFLICT" // 合并完成但存在待现场复核的异常记录
)

// 单条离线日志的合并结论
const (
	EntryMerged       = "MERGED"        // 已入账（首次放行，已扣权益）
	EntryDuplicateReq = "DUPLICATE_REQ" // 与既有核验同 request_id，闸机重复上传，跳过
	EntryReversed     = "REVERSED"      // 同一游客重复入园导致重复扣次，已自动回补
	EntryReview       = "REVIEW"        // 未绑定同行人/无法定位年卡，推现场复核
	EntryInvalid      = "INVALID"       // 记录损坏/设备未注册，无法处理
)

// OfflineEntryInput 闸机离线期间本地记录的单条放行日志
type OfflineEntryInput struct {
	LocalSeq   string   `json:"local_seq"`  // 闸机本地流水号（设备内唯一），合并幂等键
	RequestID  string   `json:"request_id"` // 闸机请求幂等键（若与在线记录一致则判重）
	PassID     string   `json:"pass_id"`    // 离线本地缓存的年卡号
	IDCard     string   `json:"id_card"`    // 主入园人证件（二维码无法离线验真时的兜底标识）
	Name       string   `json:"name"`       // 设备读到的姓名（复核辅助）
	FaceScore  float64  `json:"face_score"` // 离线本地人脸分（仅记录，服务端不再据此放行）
	Companions []string `json:"companions"` // 同行人证件号
	Attraction string   `json:"attraction"`
	GateID     string   `json:"gate_id"`
	ReleasedAt string   `json:"released_at"` // RFC3339，闸机本地实际放行时间
	Headcount  int      `json:"headcount"`   // 闸机实际放行人数（含主入园人/同行人）
}

// OfflineEntryResult 单条日志合并结果（嵌入批次返回）
type OfflineEntryResult struct {
	LocalSeq        string    `json:"local_seq"`
	RequestID       string    `json:"request_id"`
	VerificationID  string    `json:"verification_id,omitempty"`
	PassID          string    `json:"pass_id,omitempty"`
	PrincipalIDCard string    `json:"principal_id_card,omitempty"`
	Attraction      string    `json:"attraction"`
	ReleasedAt      time.Time `json:"released_at"`
	Headcount       int       `json:"headcount"`
	Status          string    `json:"status"` // MERGED / DUPLICATE_REQ / REVERSED / REVIEW / INVALID
	Detail          string    `json:"detail"`
	ChargedQuota    int       `json:"charged_quota"` // 本条实际扣减（REVERSED 后净效果为 0）
	RefundID        string    `json:"refund_id,omitempty"`
	ReviewID        string    `json:"review_id,omitempty"`
}

// OfflineBatch 一次闸机离线日志合并批次（一台闸机一次恢复上传 = 一个批次）
type OfflineBatch struct {
	ID          string    `json:"id"`
	DeviceID    string    `json:"device_id"`
	GateID      string    `json:"gate_id"`
	Attraction  string    `json:"attraction"`
	OfflineFrom time.Time `json:"offline_from"`
	OfflineTo   time.Time `json:"offline_to"`
	MergedAt    time.Time `json:"merged_at"`
	Status      string    `json:"status"`
	Operator    string    `json:"operator"`

	Total          int                  `json:"total"`            // 上传日志条数
	MergedCount    int                  `json:"merged_count"`     // 正常入账
	DuplicateCount int                  `json:"duplicate_count"`  // 重复 request_id / 已存在本地流水
	ReversedCount  int                  `json:"reversed_count"`   // 重复扣次已回补的条数
	ReviewCount    int                  `json:"review_count"`     // 推送现场复核条数
	RefundQuotaSum int                  `json:"refund_quota_sum"` // 自动回补权益人次合计
	Entries        []OfflineEntryResult `json:"entries"`
}

// 现场复核任务状态与处置动作
const (
	ReviewStatusPending  = "PENDING"
	ReviewStatusResolved = "RESOLVED"

	ReviewActionRegister = "REGISTER" // 补登记：绑定同行人后认可入园（不额外扣权益，或按登记结果处理）
	ReviewActionCharge   = "CHARGE"   // 扣权益：认可入园但补扣对应人次
	ReviewActionAbnormal = "ABNORMAL" // 标记异常放行（不补扣，留痕风控）
)

// ReviewTask 离线期间异常入园记录的现场复核任务
type ReviewTask struct {
	ID              string     `json:"id"`
	OfflineBatchID  string     `json:"offline_batch_id"`
	VerificationID  string     `json:"verification_id,omitempty"` // 已能定位年卡时关联核验记录
	PassID          string     `json:"pass_id,omitempty"`
	DeviceID        string     `json:"device_id"`
	GateID          string     `json:"gate_id"`
	Attraction      string     `json:"attraction"`
	UnboundIDCards  []string   `json:"unbound_id_cards"` // 未绑定/无法识别的证件
	PrincipalIDCard string     `json:"principal_id_card,omitempty"`
	Name            string     `json:"name,omitempty"`
	Headcount       int        `json:"headcount"`
	Reason          string     `json:"reason"` // COMPANION_NOT_BOUND | PASS_NOT_FOUND
	Detail          string     `json:"detail"`
	ReleasedAt      time.Time  `json:"released_at"`
	Status          string     `json:"status"`
	Action          string     `json:"action,omitempty"`
	Operator        string     `json:"operator,omitempty"`
	Note            string     `json:"note,omitempty"`
	ChargedQuota    int        `json:"charged_quota,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
}

// 权益回补单状态
const (
	RefundStatusDone = "DONE"
)

// QuotaRefund 重复扣次自动权益回补单（全程留痕，客服与游客可查）
type QuotaRefund struct {
	ID             string    `json:"id"`
	PassID         string    `json:"pass_id"`
	OfflineBatchID string    `json:"offline_batch_id"`
	DeviceID       string    `json:"device_id"`
	VerificationID string    `json:"verification_id"` // 被判定重复的那次离线核验
	DuplicateOf    string    `json:"duplicate_of"`    // 首次入园核验 ID
	Attraction     string    `json:"attraction"`
	Quota          int       `json:"quota"` // 回补人次
	Reason         string    `json:"reason"`
	Status         string    `json:"status"`
	NotificationID string    `json:"notification_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// 通知状态 / 渠道
const (
	NotifPending = "PENDING"
	NotifSent    = "SENT"

	NotifTypeQuotaRefund   = "QUOTA_REFUND"
	NotifTypeReviewPending = "REVIEW_PENDING" // 现场复核任务产生时知会（运营/现场，默认不打扰游客）
)

// Notification 游客/现场通知（演示环境为站内信，模拟短信渠道）
type Notification struct {
	ID        string     `json:"id"`
	PassID    string     `json:"pass_id,omitempty"`
	Phone     string     `json:"phone,omitempty"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Channel   string     `json:"channel"` // sms(模拟) | inapp
	Status    string     `json:"status"`
	RefundID  string     `json:"refund_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
}

// OfflineIncident 一次设备离线事件（心跳超时登记，恢复合并时关闭），设备可靠性依据
type OfflineIncident struct {
	ID          string     `json:"id"`
	DeviceID    string     `json:"device_id"`
	GateID      string     `json:"gate_id"`
	Attraction  string     `json:"attraction"`
	StartedAt   time.Time  `json:"started_at"`
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
	DurationSec int64      `json:"duration_sec"`
	OfflineLogs int        `json:"offline_logs"` // 该次离线合并的日志条数
	BatchID     string     `json:"batch_id,omitempty"`
	Status      string     `json:"status"` // OPEN | CLOSED
}

// DeviceReliability 设备离线可靠性统计（运营维护与高峰调度参考）
type DeviceReliability struct {
	DeviceID         string            `json:"device_id"`
	Name             string            `json:"name"`
	GateID           string            `json:"gate_id"`
	Attraction       string            `json:"attraction"`
	Online           bool              `json:"online"`
	OfflineCount     int64             `json:"offline_count"` // 离线事件次数
	TotalDowntimeSec int64             `json:"total_downtime_sec"`
	LastOfflineAt    *time.Time        `json:"last_offline_at,omitempty"`
	OfflineLogCount  int64             `json:"offline_log_count"` // 离线放行日志总量
	RefundQuota      int64             `json:"refund_quota"`      // 因该设备离线重复扣次回补的人次
	OpenIncidents    int64             `json:"open_incidents"`
	RecentIncidents  []OfflineIncident `json:"recent_incidents"`
}
