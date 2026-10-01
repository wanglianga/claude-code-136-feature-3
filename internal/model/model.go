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
	ReasonDuplicateEntry    = "DUPLICATE_ENTRY" // 网络抖动重复请求，视为同一次入园
	// 离线合并相关原因码
	ReasonOfflineDupRefund = "OFFLINE_DUP_REFUND"      // 离线重复扣次，权益已回补
	ReasonOfflineUnbound   = "OFFLINE_UNBOUND_PERSON"  // 离线期间未绑定同行人入园
	ReasonOfflineAnomaly   = "OFFLINE_ANOMALY_RELEASE" // 离线异常放行（无法正常对账）
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
	ID            string    `json:"id"`
	RequestID     string    `json:"request_id"` // 闸机侧幂等键
	PassID        string    `json:"pass_id"`
	PassType      string    `json:"pass_type"`
	HolderName    string    `json:"holder_name"`
	DeviceID      string    `json:"device_id"`
	GateID        string    `json:"gate_id"`
	Attraction    string    `json:"attraction"`
	Decision      Decision  `json:"decision"`
	Reason        string    `json:"reason"`
	Detail        string    `json:"detail"` // 人类可读说明，客服解释用
	FaceScore     float64   `json:"face_score"`
	Companions    []string  `json:"companions"`
	ChargedQuota  int       `json:"charged_quota"`
	DuplicateOf   string    `json:"duplicate_of,omitempty"`
	OfflineSync   bool      `json:"offline_sync"`
	ManualAction  string    `json:"manual_action,omitempty"`  // RELEASED | REJECTED
	RefundedQuota int       `json:"refunded_quota,omitempty"` // 离线合并判定重复扣次后回补的额度
	CreatedAt     time.Time `json:"created_at"`
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

// OfflineEvent 闸机离线期间本地放行的一条原始记录（恢复后随批次上报合并）
type OfflineEvent struct {
	EventID       string    `json:"event_id"` // 闸机本地生成的唯一 ID（与 request_id 同义）
	RequestID     string    `json:"request_id"`
	DeviceID      string    `json:"device_id"`
	GateID        string    `json:"gate_id"`
	Attraction    string    `json:"attraction"`
	PrincipalID   string    `json:"principal_id"` // 主入园人证件号（离线放行时登记）
	PrincipalName string    `json:"principal_name"`
	PassID        string    `json:"pass_id"`    // 闸机能本地识别时填写（二维码缓存）
	Companions    []string  `json:"companions"` // 同行人证件号（含未绑定者）
	FaceScore     float64   `json:"face_score"`
	LocalTime     time.Time `json:"local_time"` // 闸机本地放行时间
	SyncedAt      time.Time `json:"synced_at"`
}

// OfflineGroup 合并时按「同一游客 + 同一景点 + 同一离线时段」聚合出的入园事件组
type OfflineGroup struct {
	Key           string   `json:"key"`
	PassID        string   `json:"pass_id"`
	PrincipalID   string   `json:"principal_id"`
	PrincipalName string   `json:"principal_name"`
	Attraction    string   `json:"attraction"`
	EventIDs      []string `json:"event_ids"`
	Times         []string `json:"times"`
	HeadCount     int      `json:"head_count"`       // 该组实际应扣人次（主+已绑定同行）
	ChargedTotal  int      `json:"charged_total"`    // 补传链路已扣总额
	RefundQuota   int      `json:"refund_quota"`     // 判定重复后回补额度
	Status        string   `json:"status"`           // MERGED_NO_DUP | REFUNDED | UNRESOLVED | NO_PASS
	UnboundIDCard []string `json:"unbound_id_cards"` // 组内出现的未绑定同行人证件
}

// OfflineBatch 一次离线日志合并批次（一台闸机恢复后上报一批）
type OfflineBatch struct {
	ID           string         `json:"id"`
	DeviceID     string         `json:"device_id"`
	GateID       string         `json:"gate_id"`
	Attraction   string         `json:"attraction"`
	OfflineFrom  time.Time      `json:"offline_from"`
	OfflineTo    time.Time      `json:"offline_to"`
	Status       string         `json:"status"` // PROCESSED | PARTIAL
	EventCount   int            `json:"event_count"`
	AllowedCount int            `json:"allowed_count"`
	Groups       []OfflineGroup `json:"groups"`
	RefundTotal  int            `json:"refund_total"`
	ReviewIDs    []string       `json:"review_ids"`
	Operator     string         `json:"operator"`
	CreatedAt    time.Time      `json:"created_at"`
}

// 复核单处置动作
const (
	ReviewActionRegister = "REGISTER" // 补登记：事后绑定同行人（若有更换名额），本次不追扣
	ReviewActionCharge   = "CHARGE"   // 扣权益：承认入园事实，按 1 人次追扣
	ReviewActionAnomaly  = "ANOMALY"  // 标记异常放行：不扣不登记，留痕待查
)

// OfflineReview 离线期间未绑定同行人入园的现场复核单
type OfflineReview struct {
	ID            string     `json:"id"`
	BatchID       string     `json:"batch_id"`
	EventID       string     `json:"event_id"`
	DeviceID      string     `json:"device_id"`
	GateID        string     `json:"gate_id"`
	Attraction    string     `json:"attraction"`
	PassID        string     `json:"pass_id"`
	HolderName    string     `json:"holder_name"`
	PrincipalID   string     `json:"principal_id"`
	PrincipalName string     `json:"principal_name"`
	UnboundIDCard string     `json:"unbound_id_card"`
	UnboundName   string     `json:"unbound_name"`
	EnteredAt     time.Time  `json:"entered_at"`
	Status        string     `json:"status"` // PENDING | REGISTERED | CHARGED | ANOMALY
	Action        string     `json:"action,omitempty"`
	ChargedQuota  int        `json:"charged_quota,omitempty"`
	Operator      string     `json:"operator,omitempty"`
	Note          string     `json:"note,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// VisitorNotification 游客通知（短信/站内信，演示环境落库可查）
type VisitorNotification struct {
	ID        string    `json:"id"`
	PassID    string    `json:"pass_id"`
	Phone     string    `json:"phone"`
	Channel   string    `json:"channel"` // SMS | INBOX
	Type      string    `json:"type"`    // OFFLINE_REFUND | REVIEW_PENDING | REVIEW_RESOLVED
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// DeviceOfflineStat 闸机离线指标（维护计划与高峰调度参考）
type DeviceOfflineStat struct {
	DeviceID        string  `json:"device_id"`
	Name            string  `json:"name"`
	GateID          string  `json:"gate_id"`
	Attraction      string  `json:"attraction"`
	Online          bool    `json:"online"`
	OfflineEpisodes int64   `json:"offline_episodes"` // 离线时段次数
	OfflineSeconds  int64   `json:"offline_seconds"`  // 累计离线时长
	LastOfflineFrom string  `json:"last_offline_from"`
	LastOfflineTo   string  `json:"last_offline_to"`
	OfflineEntries  int64   `json:"offline_entries"` // 离线期间本地放行条数
	MergedBatches   int64   `json:"merged_batches"`
	RefundedQuota   int64   `json:"refunded_quota"`
	PendingReviews  int64   `json:"pending_reviews"`
	AnomalyReleases int64   `json:"anomaly_releases"`
	RiskScore       float64 `json:"risk_score"` // 综合风险分，越高越该优先维护
}
