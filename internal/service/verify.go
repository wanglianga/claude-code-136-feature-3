package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"passgate/internal/model"
)

// VerifyRequest 闸机核验请求
type VerifyRequest struct {
	RequestID   string   `json:"request_id"` // 闸机生成的幂等键（网络重试时保持不变）
	DeviceID    string   `json:"device_id"`
	PassID      string   `json:"pass_id"`
	QRToken     string   `json:"qr_token"`
	IDCard      string   `json:"id_card"`     // 主入园人证件号（刷证件入园时必传）
	FaceScore   float64  `json:"face_score"`  // 0-100；-1 表示未采集到人脸
	PhotoClear  *bool    `json:"photo_clear"` // 设备端照片质量判断
	Companions  []string `json:"companions"`  // 同行人证件号列表
	Attraction  string   `json:"attraction"`
	Timestamp   string   `json:"timestamp"`    // RFC3339，闸机本地时间；缺省取服务端时间
	OfflineSync bool     `json:"offline_sync"` // 离线补传标记
}

// VerifyResult 核验响应
type VerifyResult struct {
	VerificationID string         `json:"verification_id"`
	Decision       model.Decision `json:"decision"`
	Reason         string         `json:"reason"`
	Detail         string         `json:"detail"`
	ChargedQuota   int            `json:"charged_quota"`
	RemainingQuota int            `json:"remaining_quota"`
	DuplicateOf    string         `json:"duplicate_of,omitempty"`
}

// runCharge 执行原子扣减脚本，返回 {code, charged, extra}
// code: 1=成功 2=重复放行(extra=原核验ID) 3=无额度记录 4=额度不足 5=容量超限
func (s *Service) runCharge(ctx context.Context, passID, attraction, date string, need, capLimit int, verifID string) (int64, int64, string, error) {
	raw, err := chargeScript.Run(ctx, s.St.R, []string{
		keyRemaining(passID), keyUsed(passID), keyCap(attraction, date), keyDedup(passID, attraction),
	}, need, capLimit, verifID, DedupWindowSeconds).Slice()
	if err != nil {
		return 0, 0, "", err
	}
	if len(raw) < 3 {
		return 0, 0, "", fmt.Errorf("unexpected charge script result: %v", raw)
	}
	code, _ := raw[0].(int64)
	charged, _ := raw[1].(int64)
	extra := fmt.Sprintf("%v", raw[2])
	return code, charged, extra, nil
}

// chargeScript 原子完成：去重检查 → 额度检查 → 容量检查 → 扣额度/占容量/写去重标记
// KEYS: remaining, used, cap, dedup；ARGV: need, capLimit, dedupValue(verifID), dedupTTL
var chargeScript = redis.NewScript(`
local dup = redis.call('GET', KEYS[4])
if dup then return {2, 0, dup} end
local remaining = tonumber(redis.call('GET', KEYS[1]) or '-1')
if remaining < 0 then return {3, 0, 'NO_REMAINING'} end
local need = tonumber(ARGV[1])
if remaining < need then return {4, 0, 'QUOTA_EXHAUSTED'} end
local capLimit = tonumber(ARGV[2])
if capLimit > 0 then
  local inPark = tonumber(redis.call('GET', KEYS[3]) or '0')
  if inPark + need > capLimit then return {5, 0, 'CAPACITY_FULL'} end
end
redis.call('DECRBY', KEYS[1], need)
redis.call('INCRBY', KEYS[2], need)
redis.call('INCRBY', KEYS[3], need)
redis.call('SET', KEYS[4], ARGV[3], 'EX', tonumber(ARGV[4]))
return {1, need, ''}
`)

// Verify 闸机核验主流程。所有结论（含失败原因）落库，客服与现场可解释。
func (s *Service) Verify(ctx context.Context, req VerifyRequest) (*VerifyResult, error) {
	if req.DeviceID == "" || req.Attraction == "" {
		return nil, fmt.Errorf("%w: device_id / attraction required", ErrInvalidInput)
	}
	if req.RequestID == "" {
		id, err := s.St.NextID(ctx, "req", "RQ")
		if err != nil {
			return nil, err
		}
		req.RequestID = id
	}
	// 1) 幂等占位：同一 request_id 的并发/重试请求只处理一次
	verifID, err := s.St.NextID(ctx, "verif", "V")
	if err != nil {
		return nil, err
	}
	ok, err := s.St.R.SetNX(ctx, keyVerifReq(req.RequestID), verifID, 24*time.Hour).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		existID, _ := s.St.R.Get(ctx, keyVerifReq(req.RequestID)).Result()
		var exist model.Verification
		if err := s.St.GetJSON(ctx, keyVerif(existID), &exist); err == nil {
			return s.resultOf(ctx, &exist), nil
		}
		// 占位存在但记录缺失（上次处理崩溃）：允许重试一次
		s.St.R.Del(ctx, keyVerifReq(req.RequestID))
		ok, _ = s.St.R.SetNX(ctx, keyVerifReq(req.RequestID), verifID, 24*time.Hour).Result()
		if !ok {
			return nil, ErrConflict
		}
	}

	ts := time.Now()
	if req.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, req.Timestamp); err == nil {
			ts = parsed
		}
	}
	v := &model.Verification{
		ID: verifID, RequestID: req.RequestID, DeviceID: req.DeviceID,
		Attraction: req.Attraction, FaceScore: req.FaceScore, Companions: req.Companions,
		OfflineSync: req.OfflineSync, CreatedAt: ts,
	}
	date := dateOf(ts)

	// 2) 设备检查
	dev, err := s.GetDevice(ctx, req.DeviceID)
	if err != nil {
		return s.finish(ctx, v, nil, model.DecisionManual, model.ReasonDeviceUnknown,
			"闸机设备未注册："+req.DeviceID), nil
	}
	v.GateID = dev.GateID
	if !req.OfflineSync && time.Since(dev.LastHeartbeat) > DeviceOfflineAfter {
		return s.finish(ctx, v, nil, model.DecisionManual, model.ReasonDeviceOffline,
			fmt.Sprintf("闸机 %s 心跳超时（>%s），转人工核验", dev.ID, DeviceOfflineAfter)), nil
	}

	// 3) 定位年卡
	p, err := s.findPassByVerifyKeys(ctx, req.PassID, req.QRToken, req.IDCard)
	if err != nil {
		return s.finish(ctx, v, nil, model.DecisionManual, model.ReasonPassNotFound,
			"未找到对应年卡（pass_id/二维码/证件均不匹配）"), nil
	}
	v.PassID, v.PassType, v.HolderName = p.ID, string(p.Type), p.HolderName

	// 4) 年卡状态与有效期
	if p.Status == model.PassStatusRefunded {
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonPassRefunded, "年卡已退款，权益已清零"), nil
	}
	if p.Status == model.PassStatusFrozen {
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonPassFrozen,
			"年卡已冻结："+p.FreezeReason), nil
	}
	if date > p.ValidUntil {
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonPassExpired,
			fmt.Sprintf("年卡已于 %s 到期", p.ValidUntil)), nil
	}
	if date < p.ValidFrom {
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonPassNotStarted,
			fmt.Sprintf("年卡 %s 起生效", p.ValidFrom)), nil
	}

	// 5) 景点 / 节假日 / 时段
	if !contains(p.Attractions, req.Attraction) {
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonAttractionDenied,
			"该年卡不包含景点 "+req.Attraction), nil
	}
	if p.HolidayBlocked {
		if hol, _ := s.isHoliday(ctx, date); hol {
			return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonHolidayBlocked,
				"该年卡节假日（"+date+"）不适用"), nil
		}
	}
	if !inSlots(p.AllowedSlots, ts) {
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonTimeslotBlocked,
			fmt.Sprintf("当前时段 %s 不在可入园时段 %v 内", ts.Format("15:04"), p.AllowedSlots)), nil
	}

	// 6) 同行人校验
	if len(req.Companions) > p.CompanionLimit {
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonCompanionExceeded,
			fmt.Sprintf("同行 %d 人，超过该卡上限 %d 人", len(req.Companions), p.CompanionLimit)), nil
	}
	members, err := s.listMembers(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	bound := map[string]bool{}
	var holder *model.Member
	for i := range members {
		bound[members[i].IDCard] = true
		if members[i].Role == "holder" {
			holder = &members[i]
		}
	}
	// 主入园人：默认持卡人；刷证件入园时为该证件对应成员（老人/孩子也可能是主入园人）
	principal := holder
	if req.IDCard != "" {
		principal = nil
		for i := range members {
			if members[i].IDCard == req.IDCard {
				principal = &members[i]
				break
			}
		}
		if principal == nil {
			return s.finish(ctx, v, p, model.DecisionManual, model.ReasonCompanionNotBound,
				"证件 "+req.IDCard+" 未绑定为该年卡成员"), nil
		}
	}
	// 同行人须为已绑定成员，且不含主入园人本人
	companions := make([]string, 0, len(req.Companions))
	for _, c := range req.Companions {
		if principal != nil && c == principal.IDCard {
			continue
		}
		if !bound[c] {
			return s.finish(ctx, v, p, model.DecisionManual, model.ReasonCompanionNotBound,
				"同行人证件 "+c+" 未绑定为该年卡成员"), nil
		}
		companions = append(companions, c)
	}
	req.Companions = companions
	v.Companions = companions

	// 7) 证件有效期（主入园人）
	if principal != nil && principal.IDCardExpiry != "" && principal.IDCardExpiry < date {
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonIDExpired,
			fmt.Sprintf("%s 的证件已于 %s 过期，请人工核验", principal.Name, principal.IDCardExpiry)), nil
	}

	// 8) 人脸核验（主入园人）
	if principal != nil && principal.FaceExempt {
		// 老人等免刷脸成员：刷证件后转人工确认放行
		if req.IDCard == "" || req.IDCard != principal.IDCard {
			return s.finish(ctx, v, p, model.DecisionManual, model.ReasonFaceExemptReview,
				"免刷脸成员（"+principal.Name+"）需出示本人证件，由工作人员人工确认"), nil
		}
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonFaceExemptReview,
			"免刷脸成员（"+principal.Name+"）证件匹配，请工作人员人工确认放行"), nil
	}
	if req.PhotoClear != nil && !*req.PhotoClear {
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonPhotoUnclear,
			"现场采集照片不清晰，无法完成人脸比对"), nil
	}
	if req.FaceScore < FacePassScore {
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonFaceMismatch,
			fmt.Sprintf("人脸比对得分 %.1f 低于阈值 %.0f", req.FaceScore, FacePassScore)), nil
	}

	// 9) 放行去重快速路径：同一游客同一景点在去重窗口内已放行 → 视为同一次入园
	if orig, err := s.St.R.Get(ctx, keyDedup(p.ID, req.Attraction)).Result(); err == nil && orig != "" {
		v.DuplicateOf = orig
		return s.finish(ctx, v, p, model.DecisionAllow, model.ReasonDuplicateEntry,
			"去重窗口内重复核验（网络抖动/重复刷闸），按首次放行处理，不重复扣减权益"), nil
	}

	// 10) 高频 / 多地异常（疑似转借）
	hf, _ := s.St.R.Incr(ctx, keyHighFreq(p.ID)).Result()
	s.St.R.Expire(ctx, keyHighFreq(p.ID), HighFreqWindowSec*time.Second)
	if hf > HighFreqThreshold {
		return s.finish(ctx, v, p, model.DecisionSuspect, model.ReasonHighFrequency,
			fmt.Sprintf("%d 分钟内第 %d 次核验，超过阈值 %d，疑似转借", HighFreqWindowSec/60, hf, HighFreqThreshold)), nil
	}
	s.St.R.SAdd(ctx, keyMultiLoc(p.ID), req.Attraction)
	s.St.R.Expire(ctx, keyMultiLoc(p.ID), HighFreqWindowSec*time.Second)
	if locs, _ := s.St.R.SCard(ctx, keyMultiLoc(p.ID)).Result(); locs >= 2 && hf >= 2 {
		return s.finish(ctx, v, p, model.DecisionSuspect, model.ReasonMultiLocation,
			fmt.Sprintf("%d 分钟内出现在 %d 个不同景点，疑似转借", HighFreqWindowSec/60, locs)), nil
	}

	// 11) 原子扣减：额度 + 容量 + 去重标记（Lua 保证多入口多设备并发安全）
	need := 1 + len(req.Companions)
	attr, _ := s.GetAttraction(ctx, req.Attraction)
	capLimit := 0
	if attr != nil {
		capLimit = attr.Capacity
	}
	code, charged, extra, err := s.runCharge(ctx, p.ID, req.Attraction, date, need, capLimit, verifID)
	if err != nil {
		return nil, err
	}
	switch code {
	case 1: // 放行
		v.ChargedQuota = int(charged)
		s.recordEntryStats(ctx, date, ts.Hour(), p.Type, req.Attraction, v.ChargedQuota)
		s.markReservationUsed(ctx, p.ID, req.Attraction, date)
		return s.finish(ctx, v, p, model.DecisionAllow, model.ReasonNone,
			fmt.Sprintf("核验通过，扣减权益 %d 人次", v.ChargedQuota)), nil
	case 2: // 并发下去重标记已被写入（与快速路径竞争的兜底）
		v.DuplicateOf = extra
		return s.finish(ctx, v, p, model.DecisionAllow, model.ReasonDuplicateEntry,
			"并发重复核验，按首次放行处理，不重复扣减权益"), nil
	case 4:
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonQuotaExhausted,
			fmt.Sprintf("剩余权益不足：需 %d 人次", need)), nil
	case 5:
		return s.finish(ctx, v, p, model.DecisionManual, model.ReasonCapacityFull,
			"景点当前在园人数已达容量上限，请现场疏导"), nil
	default:
		return s.finish(ctx, v, p, model.DecisionQuota, model.ReasonQuotaExhausted, "权益额度记录异常"), nil
	}
}

// finish 统一落库：核验记录 + 闸机日志 + 异常统计
func (s *Service) finish(ctx context.Context, v *model.Verification, p *model.Pass, decision model.Decision, reason, detail string) *VerifyResult {
	v.Decision, v.Reason, v.Detail = decision, reason, detail
	_ = s.St.SetJSON(ctx, keyVerif(v.ID), v)
	if v.PassID != "" {
		s.St.R.LPush(ctx, keyPassVerifs(v.PassID), v.ID)
	}
	action := "DENY"
	if decision == model.DecisionAllow {
		action = "ALLOW"
	}
	s.writeGateLog(ctx, v, action, "")
	if decision != model.DecisionAllow {
		pt := model.PassType("")
		if p != nil {
			pt = p.Type
		}
		s.recordAnomalyStats(ctx, dateOf(v.CreatedAt), pt, reason)
	}
	return s.resultOf(ctx, v)
}

func (s *Service) resultOf(ctx context.Context, v *model.Verification) *VerifyResult {
	remaining := 0
	if v.PassID != "" {
		remaining, _ = s.remainingQuota(ctx, v.PassID)
	}
	return &VerifyResult{
		VerificationID: v.ID, Decision: v.Decision, Reason: v.Reason, Detail: v.Detail,
		ChargedQuota: v.ChargedQuota, RemainingQuota: remaining, DuplicateOf: v.DuplicateOf,
	}
}

func (s *Service) writeGateLog(ctx context.Context, v *model.Verification, action, operator string) {
	id, _ := s.St.NextID(ctx, "glog", "G")
	ts := v.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	gl := model.GateLog{
		ID: id, VerificationID: v.ID, PassID: v.PassID, DeviceID: v.DeviceID, GateID: v.GateID,
		Attraction: v.Attraction, Direction: "ENTRY", Action: action, Reason: v.Reason,
		QuotaCharged: v.ChargedQuota, Operator: operator, CreatedAt: ts,
	}
	_ = s.St.SetJSON(ctx, keyGateLog(id), &gl)
	s.St.R.LPush(ctx, keyGateLogList(), id)
	s.St.R.LTrim(ctx, keyGateLogList(), 0, 9999)
	if v.PassID != "" {
		s.St.R.LPush(ctx, keyPassGateLogs(v.PassID), id)
	}
}

// markReservationUsed 入园成功后核销当天预约
func (s *Service) markReservationUsed(ctx context.Context, passID, attraction, date string) {
	ids, _ := s.St.R.SMembers(ctx, keyPassRsv(passID)).Result()
	for _, id := range ids {
		var r model.Reservation
		if err := s.St.GetJSON(ctx, keyReservation(id), &r); err == nil {
			if r.Status == "BOOKED" && r.Attraction == attraction && r.Date == date {
				r.Status = "USED"
				_ = s.St.SetJSON(ctx, keyReservation(id), &r)
			}
		}
	}
}

// GetVerification 核验详情（客服查看失败原因与处理链路）
func (s *Service) GetVerification(ctx context.Context, id string) (*model.Verification, *model.ManualReview, error) {
	var v model.Verification
	if err := s.St.GetJSON(ctx, keyVerif(id), &v); err != nil {
		return nil, nil, ErrNotFound
	}
	var mr *model.ManualReview
	if rid, err := s.St.R.Get(ctx, keyVerifReview(id)).Result(); err == nil && rid != "" {
		var m model.ManualReview
		if err := s.St.GetJSON(ctx, keyManualReview(rid), &m); err == nil {
			mr = &m
		}
	}
	return &v, mr, nil
}

// ListVerifications 年卡核验历史
func (s *Service) ListVerifications(ctx context.Context, passID string) ([]model.Verification, error) {
	ids, err := s.St.R.LRange(ctx, keyPassVerifs(passID), 0, 199).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Verification, 0, len(ids))
	for _, id := range ids {
		var v model.Verification
		if err := s.St.GetJSON(ctx, keyVerif(id), &v); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// ManualReviewInput 人工处理入参
type ManualReviewInput struct {
	Action   string `json:"action"`   // RELEASE | REJECT
	Operator string `json:"operator"` // 工作人员
	Note     string `json:"note"`     // 处理说明（必填，客服解释依据）
}

// ManualReview 现场人工处理：放行走同一原子扣减路径，全程留痕
func (s *Service) ManualReview(ctx context.Context, verifID string, in ManualReviewInput) (*model.ManualReview, error) {
	if in.Operator == "" || in.Note == "" {
		return nil, fmt.Errorf("%w: operator / note required", ErrInvalidInput)
	}
	if in.Action != "RELEASE" && in.Action != "REJECT" {
		return nil, fmt.Errorf("%w: action must be RELEASE or REJECT", ErrInvalidInput)
	}
	v, _, err := s.GetVerification(ctx, verifID)
	if err != nil {
		return nil, err
	}
	if v.Decision == model.DecisionAllow {
		return nil, fmt.Errorf("%w: verification already allowed", ErrConflict)
	}
	if v.ManualAction != "" {
		return nil, ErrAlreadyProcessed
	}
	token, ok, err := s.St.Lock(ctx, "verif:"+verifID, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "verif:"+verifID, token)

	mr := &model.ManualReview{
		VerificationID: verifID, PassID: v.PassID, Action: in.Action,
		Operator: in.Operator, Note: in.Note, CreatedAt: time.Now(),
	}
	if in.Action == "RELEASE" {
		if v.PassID == "" {
			return nil, fmt.Errorf("%w: cannot release without pass", ErrInvalidInput)
		}
		need := 1 + len(v.Companions)
		date := dateOf(v.CreatedAt)
		code, charged, _, err := s.runCharge(ctx, v.PassID, v.Attraction, date, need, 0, verifID)
		if err != nil {
			return nil, err
		}
		switch code {
		case 1:
			mr.ChargedQuota = int(charged)
		case 2:
			mr.ChargedQuota = 0 // 去重窗口内已扣过，不重复扣
		default:
			return nil, ErrQuotaInsufficient
		}
		v.ManualAction = "RELEASED"
		v.Detail += fmt.Sprintf("；人工放行 by %s：%s", in.Operator, in.Note)
		p, _ := s.getPass(ctx, v.PassID)
		if p != nil {
			s.recordEntryStats(ctx, date, v.CreatedAt.Hour(), p.Type, v.Attraction, mr.ChargedQuota)
		}
		s.statIncr(ctx, "stats:manual:"+date, 1)
		s.writeGateLog(ctx, v, "MANUAL_RELEASE", in.Operator)
	} else {
		v.ManualAction = "REJECTED"
		v.Detail += fmt.Sprintf("；人工拒绝 by %s：%s", in.Operator, in.Note)
	}
	id, _ := s.St.NextID(ctx, "mreview", "M")
	mr.ID = id
	if err := s.St.SetJSON(ctx, keyManualReview(id), mr); err != nil {
		return nil, err
	}
	s.St.R.Set(ctx, keyVerifReview(verifID), id, 0)
	_ = s.St.SetJSON(ctx, keyVerif(v.ID), v)
	return mr, nil
}

// ExitRequest 出园请求
type ExitRequest struct {
	PassID     string `json:"pass_id"`
	Attraction string `json:"attraction"`
	Count      int    `json:"count"`
	DeviceID   string `json:"device_id"`
	Date       string `json:"date"` // 可选 YYYY-MM-DD，闸机补传出园时上报；缺省取服务端当天
}

// Exit 出园：释放在园容量
func (s *Service) Exit(ctx context.Context, req ExitRequest) error {
	if req.PassID == "" || req.Attraction == "" {
		return fmt.Errorf("%w: pass_id / attraction required", ErrInvalidInput)
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	date := dateOf(time.Now())
	if validDate(req.Date) {
		date = req.Date
	}
	// 容量不为负
	script := redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local n = tonumber(ARGV[1])
if cur - n < 0 then redis.call('SET', KEYS[1], 0) else redis.call('DECRBY', KEYS[1], n) end
return 1`)
	if err := script.Run(ctx, s.St.R, []string{keyCap(req.Attraction, date)}, req.Count).Err(); err != nil {
		return err
	}
	v := &model.Verification{
		ID: "EXIT-" + req.PassID + "-" + fmt.Sprint(time.Now().UnixNano()), PassID: req.PassID,
		DeviceID: req.DeviceID, Attraction: req.Attraction, Decision: model.DecisionAllow,
		CreatedAt: time.Now(),
	}
	id, _ := s.St.NextID(ctx, "glog", "G")
	gl := model.GateLog{
		ID: id, VerificationID: v.ID, PassID: req.PassID, DeviceID: req.DeviceID,
		Attraction: req.Attraction, Direction: "EXIT", Action: "EXIT", CreatedAt: time.Now(),
	}
	_ = s.St.SetJSON(ctx, keyGateLog(id), &gl)
	s.St.R.LPush(ctx, keyGateLogList(), id)
	s.St.R.LPush(ctx, keyPassGateLogs(req.PassID), id)
	return nil
}

var _ = errors.Is
