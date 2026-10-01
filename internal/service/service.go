package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"passgate/internal/model"
	"passgate/internal/store"
)

// 业务参数（演示环境取值，生产可配置化）
const (
	FacePassScore      = 60.0             // 人脸比对通过分
	DedupWindowSeconds = 90               // 同一游客同一景点放行去重窗口（网络抖动防重）
	HighFreqThreshold  = 4                // 高频异常：窗口内核验次数上限
	HighFreqWindowSec  = 600              // 高频统计窗口：10 分钟
	DeviceOfflineAfter = 60 * time.Second // 设备心跳超时判定离线
	RefundRate         = 0.8              // 退款折算系数
	VerifyCodeMock     = "123456"         // 演示环境模拟短信验证码
)

// 业务错误（handler 映射为 4xx）
var (
	ErrNotFound          = errors.New("resource not found")
	ErrInvalidInput      = errors.New("invalid input")
	ErrConflict          = errors.New("conflict")
	ErrPassNotActive     = errors.New("pass is not active")
	ErrPassNotFrozen     = errors.New("pass is not frozen")
	ErrAlreadyProcessed  = errors.New("already processed")
	ErrVerifyCode        = errors.New("wrong sms verify code")
	ErrMemberChangeNone  = errors.New("no member change quota left")
	ErrDuplicateHolder   = errors.New("holder already owns an active pass")
	ErrOffSale           = errors.New("pass type is off sale")
	ErrDailyLimit        = errors.New("daily sales limit reached")
	ErrQuotaInsufficient = errors.New("remaining quota insufficient")
)

// Service 业务服务
type Service struct {
	St *store.Store
}

func New(st *store.Store) *Service { return &Service{St: st} }

// ---------- Redis key 约定 ----------
func keyPass(id string) string                  { return "pass:" + id }
func keyMember(id string) string                { return "member:" + id }
func keyPassMembers(id string) string           { return "pass:" + id + ":members" }
func keyRemaining(id string) string             { return "pass:" + id + ":remaining" }
func keyUsed(id string) string                  { return "pass:" + id + ":used" }
func keyQRToken(id string) string               { return "pass:" + id + ":qrtoken" }
func keyIdxHolder(idCard string) string         { return "idx:holder:idcard:" + idCard }
func keyIdxMember(idCard string) string         { return "idx:member:idcard:" + idCard }
func keyIdxPhone(phone string) string           { return "idx:phone:" + phone }
func keyIdxQR(token string) string              { return "idx:qrtoken:" + token }
func keyVerif(id string) string                 { return "verif:" + id }
func keyVerifReq(reqID string) string           { return "verif:req:" + reqID }
func keyPassVerifs(id string) string            { return "pass:" + id + ":verifs" }
func keyGateLog(id string) string               { return "glog:" + id }
func keyGateLogList() string                    { return "glog:list" }
func keyPassGateLogs(id string) string          { return "pass:" + id + ":glogs" }
func keyDevice(id string) string                { return "device:" + id }
func keyDeviceList() string                     { return "device:list" }
func keyReservation(id string) string           { return "rsv:" + id }
func keyPassRsv(id string) string               { return "pass:" + id + ":rsv" }
func keyAppeal(id string) string                { return "appeal:" + id }
func keyAppealList() string                     { return "appeal:list" }
func keyPassChanges(id string) string           { return "pass:" + id + ":changes" }
func keyAttraction(id string) string            { return "attraction:" + id }
func keyAttractionList() string                 { return "attraction:list" }
func keyCap(attraction, date string) string     { return "cap:" + attraction + ":" + date }
func keySalesRule(t model.PassType) string      { return "salesrule:" + string(t) }
func keyHolidays() string                       { return "holidays" }
func keyDedup(passID, attraction string) string { return "dedup:" + passID + ":" + attraction }
func keyHighFreq(passID string) string          { return "hf:" + passID }
func keyMultiLoc(passID string) string          { return "loc:" + passID }
func keyManualReview(id string) string          { return "mreview:" + id }
func keyVerifReview(verifID string) string      { return "verif:" + verifID + ":review" }

// ---------- 离线合并相关 key ----------
func keyOfflineBatch(id string) string        { return "offbatch:" + id }
func keyOfflineBatchList() string             { return "offbatch:list" }
func keyDeviceBatches(deviceID string) string { return "device:" + deviceID + ":batches" }
func keyOfflineEvent(id string) string        { return "offevent:" + id }
func keyOfflineReview(id string) string       { return "offreview:" + id }
func keyOfflineReviewList() string            { return "offreview:list" }
func keyPassReviews(passID string) string     { return "pass:" + passID + ":offreviews" }
func keyNotification(id string) string        { return "notif:" + id }
func keyPassNotifs(passID string) string      { return "pass:" + passID + ":notifs" }
func keyNotificationList() string             { return "notif:list" }

// ---------- 时间辅助 ----------
func dateOf(t time.Time) string { return t.Format("2006-01-02") }

func inSlots(slots []string, t time.Time) bool {
	if len(slots) == 0 {
		return true
	}
	hm := t.Format("15:04")
	for _, s := range slots {
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			continue
		}
		if hm >= parts[0] && hm <= parts[1] {
			return true
		}
	}
	return false
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------- 通用读取 ----------
func (s *Service) getPass(ctx context.Context, id string) (*model.Pass, error) {
	var p model.Pass
	if err := s.St.GetJSON(ctx, keyPass(id), &p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

func (s *Service) savePass(ctx context.Context, p *model.Pass) error {
	p.UpdatedAt = time.Now()
	return s.St.SetJSON(ctx, keyPass(p.ID), p)
}

func (s *Service) listMembers(ctx context.Context, passID string) ([]model.Member, error) {
	ids, err := s.St.R.SMembers(ctx, keyPassMembers(passID)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Member, 0, len(ids))
	for _, id := range ids {
		var m model.Member
		if err := s.St.GetJSON(ctx, keyMember(id), &m); err == nil {
			out = append(out, m)
		}
	}
	// 稳定排序：holder 在前
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Role == "holder" && out[i].Role != "holder" {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (s *Service) remainingQuota(ctx context.Context, passID string) (int, error) {
	n, err := s.St.R.Get(ctx, keyRemaining(passID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func (s *Service) usedQuota(ctx context.Context, passID string) (int, error) {
	n, err := s.St.R.Get(ctx, keyUsed(passID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func (s *Service) addChangeLog(ctx context.Context, passID, action, detail, operator string) {
	id, _ := s.St.NextID(ctx, "changelog", "C")
	cl := model.ChangeLog{ID: id, PassID: passID, Action: action, Detail: detail, Operator: operator, CreatedAt: time.Now()}
	_ = s.St.SetJSON(ctx, "changelog:"+id, cl)
	s.St.R.LPush(ctx, keyPassChanges(passID), id)
}

func (s *Service) isHoliday(ctx context.Context, date string) (bool, error) {
	return s.St.R.SIsMember(ctx, keyHolidays(), date).Result()
}

// ---------- 统计 ----------
func (s *Service) statIncr(ctx context.Context, key string, by int64) {
	s.St.R.IncrBy(ctx, key, by)
}

func (s *Service) recordEntryStats(ctx context.Context, date string, hour int, passType model.PassType, attraction string, charged int) {
	s.statIncr(ctx, "stats:entry:"+date, 1)
	s.statIncr(ctx, fmt.Sprintf("stats:entry:%s:%02d", date, hour), 1)
	s.statIncr(ctx, "stats:entry:type:"+date+":"+string(passType), 1)
	s.statIncr(ctx, "stats:entry:attr:"+date+":"+attraction, 1)
	if charged > 0 {
		s.statIncr(ctx, "stats:quota:used:"+string(passType), int64(charged))
	}
}

func (s *Service) recordAnomalyStats(ctx context.Context, date string, passType model.PassType, reason string) {
	s.statIncr(ctx, "stats:anomaly:"+date+":"+reason, 1)
	s.statIncr(ctx, "stats:anomaly:total:"+date, 1)
	if passType != "" {
		s.statIncr(ctx, "stats:anomaly:type:"+date+":"+string(passType), 1)
	}
}
