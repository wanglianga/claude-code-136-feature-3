package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
	"passgate/internal/model"
)

// 离线合并业务参数
const (
	// OfflineReentryWindow 同一游客（主入园人证件）在同一景点两次离线/在线放行的判重窗口：
	// 窗口内除首次入园外的放行均视为闸机重复放行（重复扣次），自动回补。
	OfflineReentryWindow = 2 * time.Hour
)

// ---------- key 约定 ----------
func keyOBatch(id string) string          { return "obatch:" + id }
func keyOBatchList() string               { return "obatch:list" }
func keyDeviceBatches(id string) string   { return "device:" + id + ":batches" }
func keyOBatchToken(tok string) string    { return "obatch:token:" + tok }
func keyOEntry(device, seq string) string { return "oentry:" + device + ":" + seq }
func keyRefund(id string) string          { return "refund:" + id }
func keyRefundList() string               { return "refund:list" }
func keyPassRefunds(id string) string     { return "pass:" + id + ":refunds" }
func keyNotif(id string) string           { return "notif:" + id }
func keyNotifList() string                { return "notif:list" }
func keyPassNotifs(id string) string      { return "pass:" + id + ":notifs" }
func keyReview(id string) string          { return "review:" + id }
func keyReviewList() string               { return "review:list" }
func keyReviewStatus(st string) string    { return "review:status:" + st }
func keyIncident(id string) string        { return "offinc:" + id }
func keyIncidentList() string             { return "offinc:list" }
func keyDeviceIncidents(id string) string { return "device:" + id + ":incidents" }
func keyDeviceOpenInc(id string) string   { return "device:" + id + ":incident_open" }

// offlineChargeScript 离线合并专用扣减：不走在线 90 秒去重（补传时标记早已过期），
// 按闸机实际放行人数扣减；额度不足时扣到 0 为止（人已实际入园，差额由 shortfall 留痕）。
// KEYS: remaining, used, cap；ARGV: need → {charged, shortfall}
var offlineChargeScript = redis.NewScript(`
local remaining = tonumber(redis.call('GET', KEYS[1]) or '-1')
local need = tonumber(ARGV[1])
if remaining < 0 then return {0, need} end
local charged = need
local shortfall = 0
if remaining < need then
  charged = remaining
  shortfall = need - remaining
end
if charged > 0 then
  redis.call('DECRBY', KEYS[1], charged)
  redis.call('INCRBY', KEYS[2], charged)
  redis.call('INCRBY', KEYS[3], charged)
end
return {charged, shortfall}
`)

// refundScript 回补：remaining += quota；used/cap 向下截断不为负。
// KEYS: remaining, used, cap；ARGV: quota → 实际回补量（=quota，remaining 无上限）
var refundScript = redis.NewScript(`
local quota = tonumber(ARGV[1])
redis.call('INCRBY', KEYS[1], quota)
local used = tonumber(redis.call('GET', KEYS[2]) or '0')
local fromUsed = used
if fromUsed > quota then fromUsed = quota end
if fromUsed > 0 then redis.call('DECRBY', KEYS[2], fromUsed) end
local cap = tonumber(redis.call('GET', KEYS[3]) or '0')
local fromCap = cap
if fromCap > quota then fromCap = quota end
if fromCap > 0 then redis.call('DECRBY', KEYS[3], fromCap) end
return quota
`)

// OfflineBatchInput 闸机恢复后上传的离线日志包
type OfflineBatchInput struct {
	DeviceID    string                    `json:"device_id"`
	OfflineFrom string                    `json:"offline_from"` // RFC3339，可空（取最早一条放行时间）
	OfflineTo   string                    `json:"offline_to"`   // RFC3339，可空（取最晚一条放行时间）
	ClientToken string                    `json:"client_token"` // 闸机为本次上传生成的幂等键（重传同一包）
	Operator    string                    `json:"operator"`
	Entries     []model.OfflineEntryInput `json:"entries"`
}

// MergeOfflineLogs 合并一台闸机离线期间本地放行的日志：
//  1. 逐行幂等（闸机本地流水号 / request_id / 整包 client_token）
//  2. 识别同一游客窗口内重复扣次 → 自动回补权益并通知游客
//  3. 未绑定同行人 / 无法定位年卡 → 推现场复核（补登记 / 扣权益 / 标记异常放行）
//  4. 关闭离线事件，沉淀设备离线次数与时长，供运营维护与高峰调度
func (s *Service) MergeOfflineLogs(ctx context.Context, in OfflineBatchInput) (*model.OfflineBatch, error) {
	if in.DeviceID == "" {
		return nil, fmt.Errorf("%w: device_id required", ErrInvalidInput)
	}
	if len(in.Entries) == 0 {
		return nil, fmt.Errorf("%w: entries required", ErrInvalidInput)
	}
	dev, err := s.GetDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: device %s not registered", ErrInvalidInput, in.DeviceID)
	}
	// 整包幂等：闸机重传同一日志包直接返回首次合并结果
	if in.ClientToken != "" {
		if existID, _ := s.St.R.Get(ctx, keyOBatchToken(in.ClientToken)).Result(); existID != "" {
			return s.GetOfflineBatch(ctx, existID)
		}
	}
	// 同一设备串行合并，避免两包交叉导致判重窗口错乱
	token, ok, err := s.St.Lock(ctx, "omerge:"+in.DeviceID, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "omerge:"+in.DeviceID, token)

	batchID, err := s.St.NextID(ctx, "obatch", "OB")
	if err != nil {
		return nil, err
	}
	op := in.Operator
	if op == "" {
		op = "gate-sync"
	}
	batch := &model.OfflineBatch{
		ID: batchID, DeviceID: dev.ID, GateID: dev.GateID, Attraction: dev.Attraction,
		MergedAt: time.Now(), Status: model.BatchStatusMerged, Operator: op,
		Entries: []model.OfflineEntryResult{},
	}

	// 按实际放行时间排序，保证「首次入园」判重最早者胜出
	entries := make([]model.OfflineEntryInput, len(in.Entries))
	copy(entries, in.Entries)
	sort.SliceStable(entries, func(i, j int) bool {
		ti := parseEntryTime(entries[i].ReleasedAt)
		tj := parseEntryTime(entries[j].ReleasedAt)
		if ti.Equal(tj) {
			return entries[i].LocalSeq < entries[j].LocalSeq
		}
		return ti.Before(tj)
	})
	for _, e := range entries {
		if e.LocalSeq == "" {
			batch.Entries = append(batch.Entries, model.OfflineEntryResult{
				Status: model.EntryInvalid, Detail: "缺少闸机本地流水号 local_seq",
				ReleasedAt: time.Now(), Attraction: e.Attraction,
			})
			batch.Total++
			continue
		}
		batch.Total++
		res := s.mergeOneOfflineEntry(ctx, batch, dev, e)
		batch.Entries = append(batch.Entries, res)
		switch res.Status {
		case model.EntryMerged:
			batch.MergedCount++
		case model.EntryDuplicateReq:
			batch.DuplicateCount++
		case model.EntryReversed:
			batch.ReversedCount++
			batch.RefundQuotaSum += res.ChargedQuota
		case model.EntryReview:
			batch.ReviewCount++
		}
	}

	// 批次时间区间
	batch.OfflineFrom = parseEntryTime(in.OfflineFrom)
	batch.OfflineTo = parseEntryTime(in.OfflineTo)
	if batch.OfflineFrom.IsZero() {
		batch.OfflineFrom = batch.Entries[0].ReleasedAt
	}
	if batch.OfflineTo.IsZero() {
		batch.OfflineTo = batch.Entries[len(batch.Entries)-1].ReleasedAt
	}
	if batch.ReviewCount > 0 {
		batch.Status = model.BatchStatusConflict
	}

	if err := s.St.SetJSON(ctx, keyOBatch(batchID), batch); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyOBatchList(), batchID)
	s.St.R.LPush(ctx, keyDeviceBatches(dev.ID), batchID)
	if in.ClientToken != "" {
		s.St.R.Set(ctx, keyOBatchToken(in.ClientToken), batchID, 7*24*time.Hour)
	}

	// 关闭本次离线事件（心跳超时期间在线转人工也可能已登记 OPEN 事件）
	s.closeIncidentForBatch(ctx, dev, batch)

	// 批次级统计
	date := dateOf(batch.MergedAt)
	s.statIncr(ctx, "stats:offline:batch:"+date, 1)
	s.statIncr(ctx, "stats:offline:entry:"+date, int64(batch.MergedCount))
	s.statIncr(ctx, "stats:offline:reversed:"+date, int64(batch.ReversedCount))
	s.statIncr(ctx, "stats:offline:review:"+date, int64(batch.ReviewCount))
	return batch, nil
}

// mergeOneOfflineEntry 合并单条离线放行日志，返回其合并结论
func (s *Service) mergeOneOfflineEntry(ctx context.Context, batch *model.OfflineBatch, dev *model.Device, e model.OfflineEntryInput) model.OfflineEntryResult {
	releasedAt := parseEntryTime(e.ReleasedAt)
	if releasedAt.IsZero() {
		releasedAt = batch.MergedAt
	}
	attr := e.Attraction
	if attr == "" {
		attr = dev.Attraction
	}
	gate := e.GateID
	if gate == "" {
		gate = dev.GateID
	}
	res := model.OfflineEntryResult{
		LocalSeq: e.LocalSeq, RequestID: e.RequestID, Attraction: attr,
		ReleasedAt: releasedAt, Headcount: e.Headcount, PrincipalIDCard: e.IDCard,
	}

	// 1) 闸机本地流水号幂等：同一设备同一流水号只合并一次
	seen, _ := s.St.R.SetNX(ctx, keyOEntry(dev.ID, e.LocalSeq), batch.ID, 0).Result()
	if !seen {
		existBatch, _ := s.St.R.Get(ctx, keyOEntry(dev.ID, e.LocalSeq)).Result()
		res.Status = model.EntryDuplicateReq
		res.Detail = "该本地流水号已在批次 " + existBatch + " 合并，跳过"
		return res
	}
	// 2) request_id 已在服务端存在（在线核验或此前补传）→ 同一放行，不重复扣次
	reqID := e.RequestID
	if reqID == "" {
		reqID = fmt.Sprintf("OFFLINE-%s-%s", dev.ID, e.LocalSeq)
	}
	if existVerifID, gerr := s.St.R.Get(ctx, keyVerifReq(reqID)).Result(); gerr == nil && existVerifID != "" {
		res.Status = model.EntryDuplicateReq
		res.VerificationID = existVerifID
		res.Detail = "request_id " + reqID + " 已存在核验 " + existVerifID + "，判为同一放行"
		return res
	}

	// 3) 定位年卡（pass_id 优先，其次主入园人证件索引）
	p, _ := s.findPassByVerifyKeys(ctx, e.PassID, "", e.IDCard)
	if p == nil {
		// 无法定位年卡：留痕并推现场复核
		v := s.buildOfflineVerification(batch, dev, e, "", "", gate, attr, releasedAt, reqID)
		v.Decision = model.DecisionManual
		v.Reason = model.ReasonPassNotFound
		v.Detail = "离线放行：证件/年卡无法匹配，转现场复核"
		v.PrincipalIDCard = e.IDCard
		s.persistOfflineVerification(ctx, v, "OFFLINE_REVIEW")
		res.VerificationID = v.ID
		review := s.createReview(ctx, batch, v, dev, e, gate, attr, releasedAt,
			model.ReasonPassNotFound, "离线放行记录无法定位年卡（证件 "+maskIDCard(e.IDCard)+"）")
		res.ReviewID = review.ID
		res.Status = model.EntryReview
		res.Detail = "无法定位年卡，已推现场复核 " + review.ID
		return res
	}
	res.PassID = p.ID

	// 主入园人证件：显式证件优先，否则视为持卡人
	principalCard := e.IDCard
	if principalCard == "" {
		principalCard = p.HolderIDCard
	}
	res.PrincipalIDCard = principalCard
	headcount := e.Headcount
	if headcount <= 0 {
		headcount = 1 + len(e.Companions)
	}
	res.Headcount = headcount

	// 4) 同行人绑定校验：存在未绑定同行人 → 整记录推现场复核（先不扣，现场决定补登记/扣权益/异常）
	members, _ := s.listMembers(ctx, p.ID)
	bound := map[string]bool{}
	for _, m := range members {
		bound[m.IDCard] = true
	}
	unbound := []string{}
	for _, c := range e.Companions {
		if c == principalCard {
			continue
		}
		if !bound[c] {
			unbound = append(unbound, c)
		}
	}
	if len(unbound) > 0 {
		v := s.buildOfflineVerification(batch, dev, e, p.ID, string(p.Type), gate, attr, releasedAt, reqID)
		v.HolderName = p.HolderName
		v.Decision = model.DecisionManual
		v.Reason = model.ReasonCompanionNotBound
		v.Detail = fmt.Sprintf("离线放行：%d 名同行人未绑定（%v），转现场复核", len(unbound), unbound)
		v.PrincipalIDCard = principalCard
		s.persistOfflineVerification(ctx, v, "OFFLINE_REVIEW")
		res.VerificationID = v.ID
		review := s.createReview(ctx, batch, v, dev, e, gate, attr, releasedAt,
			model.ReasonCompanionNotBound, fmt.Sprintf("未绑定同行人证件 %v", unbound))
		review.UnboundIDCards = unbound
		review.PrincipalIDCard = principalCard
		_ = s.St.SetJSON(ctx, keyReview(review.ID), review)
		res.ReviewID = review.ID
		res.Status = model.EntryReview
		res.Detail = "存在未绑定同行人，已推现场复核 " + review.ID
		return res
	}

	// 5) 先落一笔离线放行核验（闸机当时确实放行），再判重决定保留或回补
	v := s.buildOfflineVerification(batch, dev, e, p.ID, string(p.Type), gate, attr, releasedAt, reqID)
	v.HolderName = p.HolderName
	v.Decision = model.DecisionAllow
	v.PrincipalIDCard = principalCard
	v.Companions = e.Companions

	charged, shortfall := s.offlineCharge(ctx, p.ID, attr, dateOf(releasedAt), headcount)
	v.ChargedQuota = charged
	s.recordEntryStats(ctx, dateOf(releasedAt), releasedAt.Hour(), p.Type, attr, charged)
	detail := fmt.Sprintf("闸机离线期间本地放行，恢复后合并入账，扣减权益 %d 人次", charged)
	if shortfall > 0 {
		detail += fmt.Sprintf("；权益不足，%d 人次未扣（已登记，人已入园）", shortfall)
		s.recordAnomalyStats(ctx, dateOf(releasedAt), p.Type, "OFFLINE_QUOTA_SHORTFALL")
	}
	v.Detail = detail
	s.persistOfflineVerification(ctx, v, "OFFLINE_ALLOW")
	res.VerificationID = v.ID
	res.ChargedQuota = charged
	// request_id 占位，阻止后续单条补传重复处理
	s.St.R.Set(ctx, keyVerifReq(reqID), v.ID, 24*time.Hour)

	// 6) 重复扣次识别：同一主入园人 + 同一景点，判重窗口内已存在首次入园
	if first := s.findPriorChargedEntry(ctx, p, principalCard, attr, releasedAt, v.ID); first != nil {
		v.Reason = model.ReasonDuplicateEntry
		if v.ChargedQuota > 0 {
			// 仅回补合并时实际扣减的量（权益不足时 offlineCharge 已扣到 0 为止，不可超额回补）
			refund := s.reverseOfflineCharge(ctx, batch, dev, p, v, first, v.ChargedQuota)
			v.ReversedBy = refund.ID
			v.Detail += fmt.Sprintf("；判为 %s 首次入园的重复放行（窗口 %s），已自动回补 %d 人次（回补单 %s）",
				first.ID, OfflineReentryWindow, refund.Quota, refund.ID)
			res.RefundID = refund.ID
		} else {
			v.Detail += fmt.Sprintf("；判为 %s 首次入园的重复放行（窗口 %s），当时权益已不足未实际扣减，无需回补",
				first.ID, OfflineReentryWindow)
		}
		_ = s.St.SetJSON(ctx, keyVerif(v.ID), v)
		res.Status = model.EntryReversed
		res.Detail = v.Detail
		return res
	}

	res.Status = model.EntryMerged
	res.Detail = detail
	return res
}

// buildOfflineVerification 构造离线核验记录（未落库）
func (s *Service) buildOfflineVerification(batch *model.OfflineBatch, dev *model.Device, e model.OfflineEntryInput,
	passID, passType, gate, attr string, releasedAt time.Time, reqID string) *model.Verification {
	return &model.Verification{
		ID:              "", // persistOfflineVerification 内分配
		RequestID:       reqID,
		PassID:          passID,
		PassType:        passType,
		Name:            e.Name,
		DeviceID:        dev.ID,
		GateID:          gate,
		Attraction:      attr,
		FaceScore:       e.FaceScore,
		Companions:      e.Companions,
		OfflineSync:     true,
		OfflineBatchID:  batch.ID,
		OfflineLocalSeq: e.LocalSeq,
		PrincipalIDCard: e.IDCard,
		CreatedAt:       releasedAt,
	}
}

// persistOfflineVerification 分配 ID 并写核验记录 + 闸机流水
func (s *Service) persistOfflineVerification(ctx context.Context, v *model.Verification, gateAction string) {
	if v.ID == "" {
		v.ID, _ = s.St.NextID(ctx, "verif", "V")
	}
	_ = s.St.SetJSON(ctx, keyVerif(v.ID), v)
	if v.PassID != "" {
		s.St.R.LPush(ctx, keyPassVerifs(v.PassID), v.ID)
	}
	id, _ := s.St.NextID(ctx, "glog", "G")
	gl := model.GateLog{
		ID: id, VerificationID: v.ID, PassID: v.PassID, DeviceID: v.DeviceID, GateID: v.GateID,
		Attraction: v.Attraction, Direction: "ENTRY", Action: gateAction, Reason: v.Reason,
		QuotaCharged: v.ChargedQuota, Operator: "offline-sync", CreatedAt: v.CreatedAt,
	}
	_ = s.St.SetJSON(ctx, keyGateLog(id), &gl)
	s.St.R.LPush(ctx, keyGateLogList(), id)
	s.St.R.LTrim(ctx, keyGateLogList(), 0, 9999)
	if v.PassID != "" {
		s.St.R.LPush(ctx, keyPassGateLogs(v.PassID), id)
	}
}

// findPriorChargedEntry 在判重窗口内寻找该游客在该景点的「首次入园」已扣次记录
func (s *Service) findPriorChargedEntry(ctx context.Context, p *model.Pass, principalCard, attr string, t time.Time, excludeID string) *model.Verification {
	ids, _ := s.St.R.LRange(ctx, keyPassVerifs(p.ID), 0, 499).Result()
	var earliest *model.Verification
	for _, id := range ids {
		var v model.Verification
		if err := s.St.GetJSON(ctx, keyVerif(id), &v); err != nil {
			continue
		}
		if v.ID == excludeID {
			continue // 排除本条（本条已先落库，同刻查询不能把自己当作首次入园）
		}
		if v.Attraction != attr || v.Decision != model.DecisionAllow || v.ChargedQuota <= 0 || v.ReversedBy != "" {
			continue
		}
		if v.CreatedAt.After(t) || t.Sub(v.CreatedAt) > OfflineReentryWindow {
			continue
		}
		pcard := v.PrincipalIDCard
		if pcard == "" {
			pcard = p.HolderIDCard // 特性上线前的在线记录无主入园人字段，按持卡人兼容
		}
		if pcard != principalCard {
			continue
		}
		if earliest == nil || v.CreatedAt.Before(earliest.CreatedAt) {
			vv := v
			earliest = &vv
		}
	}
	return earliest
}

// offlineCharge 执行离线扣减脚本，返回实际扣减量与不足差额
func (s *Service) offlineCharge(ctx context.Context, passID, attr, date string, need int) (int, int) {
	res, err := offlineChargeScript.Run(ctx, s.St.R,
		[]string{keyRemaining(passID), keyUsed(passID), keyCap(attr, date)}, need).Slice()
	if err != nil || len(res) < 2 {
		return 0, need
	}
	charged, _ := res[0].(int64)
	shortfall, _ := res[1].(int64)
	return int(charged), int(shortfall)
}

// reverseOfflineCharge 回补一笔重复扣次：回补权益、写回补单、通知游客、留痕统计
func (s *Service) reverseOfflineCharge(ctx context.Context, batch *model.OfflineBatch, dev *model.Device,
	p *model.Pass, dup *model.Verification, first *model.Verification, quota int) *model.QuotaRefund {
	id, _ := s.St.NextID(ctx, "refund", "RF")
	back, _ := refundScript.Run(ctx, s.St.R,
		[]string{keyRemaining(p.ID), keyUsed(p.ID), keyCap(dup.Attraction, dateOf(dup.CreatedAt))}, quota).Int64()
	r := &model.QuotaRefund{
		ID: id, PassID: p.ID, OfflineBatchID: batch.ID, DeviceID: dev.ID,
		VerificationID: dup.ID, DuplicateOf: first.ID, Attraction: dup.Attraction,
		Quota: int(back), Reason: "OFFLINE_DUPLICATE_ENTRY", Status: model.RefundStatusDone, CreatedAt: time.Now(),
	}
	// 冲抵合并时计入的入园/权益统计（含按小时计数，保证峰值统计不被重复放行虚高）
	d := dateOf(dup.CreatedAt)
	s.statIncr(ctx, "stats:entry:"+d, -1)
	s.statIncr(ctx, fmt.Sprintf("stats:entry:%s:%02d", d, dup.CreatedAt.Hour()), -1)
	s.statIncr(ctx, "stats:entry:type:"+d+":"+string(p.Type), -1)
	s.statIncr(ctx, "stats:entry:attr:"+d+":"+dup.Attraction, -1)
	s.statIncr(ctx, "stats:quota:used:"+string(p.Type), -int64(back))
	s.statIncr(ctx, "stats:offline:refund:quota", int64(back))
	// 通知游客（模拟短信渠道，直接标记发送成功）
	n := s.notifyRefund(ctx, p, r, dev)
	r.NotificationID = n.ID
	_ = s.St.SetJSON(ctx, keyRefund(id), r)
	s.St.R.LPush(ctx, keyRefundList(), id)
	s.St.R.LPush(ctx, keyPassRefunds(p.ID), id)
	s.addChangeLog(ctx, p.ID, "OFFLINE_REFUND",
		fmt.Sprintf("闸机 %s 离线重复放行，核验 %s 与首次入园 %s 重复，自动回补权益 %d 人次（回补单 %s）",
			dev.ID, dup.ID, first.ID, back, id), "offline-sync")
	return r
}

// notifyRefund 生成并「发送」回补通知（演示环境模拟短信网关成功）
func (s *Service) notifyRefund(ctx context.Context, p *model.Pass, r *model.QuotaRefund, dev *model.Device) *model.Notification {
	id, _ := s.St.NextID(ctx, "notif", "N")
	now := time.Now()
	n := &model.Notification{
		ID: id, PassID: p.ID, Phone: p.Phone, Type: model.NotifTypeQuotaRefund,
		Title: "重复扣次权益回补通知",
		Content: fmt.Sprintf("【景区年卡】您于 %s 在 %s 的一次入园因闸机 %s 离线被重复扣减 %d 人次，已自动回补至年卡 %s，如有疑问请联系客服。",
			r.CreatedAt.Format("01-02 15:04"), r.Attraction, dev.Name, r.Quota, p.ID),
		Channel: "sms", Status: model.NotifSent, RefundID: r.ID, CreatedAt: now, SentAt: &now,
	}
	_ = s.St.SetJSON(ctx, keyNotif(id), n)
	s.St.R.LPush(ctx, keyNotifList(), id)
	s.St.R.LPush(ctx, keyPassNotifs(p.ID), id)
	return n
}

// createReview 生成现场复核任务
func (s *Service) createReview(ctx context.Context, batch *model.OfflineBatch, v *model.Verification, dev *model.Device,
	e model.OfflineEntryInput, gate, attr string, releasedAt time.Time, reason, detail string) *model.ReviewTask {
	id, _ := s.St.NextID(ctx, "review", "RV")
	headcount := e.Headcount
	if headcount <= 0 {
		headcount = 1 + len(e.Companions)
	}
	rv := &model.ReviewTask{
		ID: id, OfflineBatchID: batch.ID, VerificationID: v.ID, PassID: v.PassID,
		DeviceID: dev.ID, GateID: gate, Attraction: attr,
		UnboundIDCards: []string{}, PrincipalIDCard: e.IDCard, Name: e.Name,
		Headcount: headcount, Reason: reason, Detail: detail,
		ReleasedAt: releasedAt, Status: model.ReviewStatusPending, CreatedAt: time.Now(),
	}
	_ = s.St.SetJSON(ctx, keyReview(id), rv)
	s.St.R.LPush(ctx, keyReviewList(), id)
	s.St.R.SAdd(ctx, keyReviewStatus(model.ReviewStatusPending), id)
	s.statIncr(ctx, "stats:review:pending", 1)
	// 站内知会现场（复核工作台新任务）
	now := time.Now()
	nid, _ := s.St.NextID(ctx, "notif", "N")
	n := &model.Notification{
		ID: nid, PassID: v.PassID, Type: model.NotifTypeReviewPending,
		Title:   "离线异常入园待现场复核",
		Content: fmt.Sprintf("闸机 %s 离线期间有一条异常放行需复核（%s）：%s", dev.ID, reason, detail),
		Channel: "inapp", Status: model.NotifSent, CreatedAt: now, SentAt: &now,
	}
	_ = s.St.SetJSON(ctx, keyNotif(nid), n)
	s.St.R.LPush(ctx, keyNotifList(), nid)
	return rv
}

// ---------- 复核处置 ----------

// ReviewResolveInput 现场复核处置入参
type ReviewResolveInput struct {
	Action        string                `json:"action"` // REGISTER | CHARGE | ABNORMAL
	Operator      string                `json:"operator"`
	Note          string                `json:"note"`
	PassID        string                `json:"pass_id"`       // 原记录无法定位年卡时，现场补录关联年卡
	ChargeQuota   int                   `json:"charge_quota"`  // 补扣人次，缺省按放行人数
	Registrations []RegisterMemberInput `json:"registrations"` // REGISTER 时补登记的同行人
}

// RegisterMemberInput 现场补登记的同行人信息
type RegisterMemberInput struct {
	Name     string `json:"name"`
	IDCard   string `json:"id_card"`
	Relation string `json:"relation"`
}

// ResolveReviewTask 现场复核：补登记 / 扣权益 / 标记异常放行
func (s *Service) ResolveReviewTask(ctx context.Context, id string, in ReviewResolveInput) (*model.ReviewTask, error) {
	if in.Operator == "" || in.Note == "" {
		return nil, fmt.Errorf("%w: operator / note required", ErrInvalidInput)
	}
	switch in.Action {
	case model.ReviewActionRegister, model.ReviewActionCharge, model.ReviewActionAbnormal:
	default:
		return nil, fmt.Errorf("%w: action must be REGISTER / CHARGE / ABNORMAL", ErrInvalidInput)
	}
	var rv model.ReviewTask
	if err := s.St.GetJSON(ctx, keyReview(id), &rv); err != nil {
		return nil, ErrNotFound
	}
	if rv.Status != model.ReviewStatusPending {
		return nil, ErrAlreadyProcessed
	}
	token, ok, err := s.St.Lock(ctx, "review:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "review:"+id, token)

	passID := rv.PassID
	if in.PassID != "" {
		passID = in.PassID
	}
	now := time.Now()
	rv.Status = model.ReviewStatusResolved
	rv.Action = in.Action
	rv.Operator = in.Operator
	rv.Note = in.Note
	rv.ResolvedAt = &now

	switch in.Action {
	case model.ReviewActionRegister:
		if passID == "" {
			return nil, fmt.Errorf("%w: register requires pass_id", ErrInvalidInput)
		}
		if _, err := s.getPass(ctx, passID); err != nil {
			return nil, fmt.Errorf("%w: pass %s not found", ErrInvalidInput, passID)
		}
		// 补登记同行人（离线例外：不消耗成员更换次数）
		for _, rm := range in.Registrations {
			if rm.Name == "" || rm.IDCard == "" {
				continue
			}
			if _, gerr := s.St.R.Get(ctx, keyIdxMember(rm.IDCard)).Result(); gerr == nil {
				continue // 已绑定（可能属于其他卡），不重复建成员
			}
			members, _ := s.listMembers(ctx, passID)
			m := model.Member{
				ID: fmt.Sprintf("%s-M%d", passID, len(members)+100), PassID: passID, Name: rm.Name,
				IDCard: rm.IDCard, Role: "companion", Relation: rm.Relation,
				IDCardExpiry: "2099-12-31", PhotoVersion: 1, CreatedAt: now,
			}
			_ = s.St.SetJSON(ctx, keyMember(m.ID), &m)
			s.St.R.SAdd(ctx, keyPassMembers(passID), m.ID)
			s.St.R.Set(ctx, keyIdxMember(m.IDCard), m.ID, 0)
		}
		need := in.ChargeQuota
		if need <= 0 {
			need = rv.Headcount
		}
		charged, _ := s.offlineCharge(ctx, passID, rv.Attraction, dateOf(rv.ReleasedAt), need)
		rv.ChargedQuota = charged
		rv.PassID = passID
		if p, _ := s.getPass(ctx, passID); p != nil {
			s.recordEntryStats(ctx, dateOf(rv.ReleasedAt), rv.ReleasedAt.Hour(), p.Type, rv.Attraction, charged)
			s.addChangeLog(ctx, passID, "OFFLINE_REGISTER",
				fmt.Sprintf("离线异常入园 %s 现场补登记同行 %d 人，补扣权益 %d 人次 by %s：%s",
					rv.VerificationID, len(in.Registrations), charged, in.Operator, in.Note), in.Operator)
		}
		s.appendReviewGateLog(ctx, &rv, passID, "OFFLINE_REGISTERED", charged, in.Operator)

	case model.ReviewActionCharge:
		if passID == "" {
			return nil, fmt.Errorf("%w: charge requires pass_id", ErrInvalidInput)
		}
		need := in.ChargeQuota
		if need <= 0 {
			need = rv.Headcount
		}
		charged, _ := s.offlineCharge(ctx, passID, rv.Attraction, dateOf(rv.ReleasedAt), need)
		rv.ChargedQuota = charged
		rv.PassID = passID
		if p, _ := s.getPass(ctx, passID); p != nil {
			s.recordEntryStats(ctx, dateOf(rv.ReleasedAt), rv.ReleasedAt.Hour(), p.Type, rv.Attraction, charged)
			s.addChangeLog(ctx, passID, "OFFLINE_CHARGE",
				fmt.Sprintf("离线异常入园 %s 现场确认补扣权益 %d 人次 by %s：%s",
					rv.VerificationID, charged, in.Operator, in.Note), in.Operator)
		}
		s.appendReviewGateLog(ctx, &rv, passID, "OFFLINE_CHARGED", charged, in.Operator)

	case model.ReviewActionAbnormal:
		s.recordAnomalyStats(ctx, dateOf(rv.ReleasedAt), model.PassType(""), "OFFLINE_ABNORMAL_RELEASE")
		if passID != "" {
			s.addChangeLog(ctx, passID, "OFFLINE_ABNORMAL",
				fmt.Sprintf("离线异常放行 %s 标记异常，不补扣权益 by %s：%s", rv.VerificationID, in.Operator, in.Note), in.Operator)
		}
		s.appendReviewGateLog(ctx, &rv, passID, "OFFLINE_ABNORMAL", 0, in.Operator)
	}

	// 关联核验记录补上处理动作
	if rv.VerificationID != "" {
		var v model.Verification
		if err := s.St.GetJSON(ctx, keyVerif(rv.VerificationID), &v); err == nil {
			v.ManualAction = "REVIEW_" + in.Action
			v.Detail += fmt.Sprintf("；现场复核 %s by %s：%s", in.Action, in.Operator, in.Note)
			_ = s.St.SetJSON(ctx, keyVerif(rv.VerificationID), &v)
		}
	}
	if err := s.St.SetJSON(ctx, keyReview(id), &rv); err != nil {
		return nil, err
	}
	s.St.R.SRem(ctx, keyReviewStatus(model.ReviewStatusPending), id)
	s.St.R.SAdd(ctx, keyReviewStatus(model.ReviewStatusResolved), id)
	s.statIncr(ctx, "stats:review:pending", -1)
	return &rv, nil
}

func (s *Service) appendReviewGateLog(ctx context.Context, rv *model.ReviewTask, passID, action string, charged int, operator string) {
	id, _ := s.St.NextID(ctx, "glog", "G")
	gl := model.GateLog{
		ID: id, VerificationID: rv.VerificationID, PassID: passID, DeviceID: rv.DeviceID, GateID: rv.GateID,
		Attraction: rv.Attraction, Direction: "ENTRY", Action: action, Reason: rv.Reason,
		QuotaCharged: charged, Operator: operator, CreatedAt: time.Now(),
	}
	_ = s.St.SetJSON(ctx, keyGateLog(id), &gl)
	s.St.R.LPush(ctx, keyGateLogList(), id)
	if passID != "" {
		s.St.R.LPush(ctx, keyPassGateLogs(passID), id)
	}
}

// ---------- 查询：批次 / 回补 / 通知 / 复核 ----------

func (s *Service) GetOfflineBatch(ctx context.Context, id string) (*model.OfflineBatch, error) {
	var b model.OfflineBatch
	if err := s.St.GetJSON(ctx, keyOBatch(id), &b); err != nil {
		return nil, ErrNotFound
	}
	return &b, nil
}

func (s *Service) ListOfflineBatches(ctx context.Context, deviceID string, limit int) ([]model.OfflineBatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var key string
	if deviceID != "" {
		key = keyDeviceBatches(deviceID)
	} else {
		key = keyOBatchList()
	}
	ids, err := s.St.R.LRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.OfflineBatch, 0, len(ids))
	for _, id := range ids {
		var b model.OfflineBatch
		if err := s.St.GetJSON(ctx, keyOBatch(id), &b); err == nil {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *Service) GetRefund(ctx context.Context, id string) (*model.QuotaRefund, error) {
	var r model.QuotaRefund
	if err := s.St.GetJSON(ctx, keyRefund(id), &r); err != nil {
		return nil, ErrNotFound
	}
	return &r, nil
}

func (s *Service) ListRefunds(ctx context.Context, passID string, limit int) ([]model.QuotaRefund, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	key := keyRefundList()
	if passID != "" {
		key = keyPassRefunds(passID)
	}
	ids, err := s.St.R.LRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.QuotaRefund, 0, len(ids))
	for _, id := range ids {
		var r model.QuotaRefund
		if err := s.St.GetJSON(ctx, keyRefund(id), &r); err == nil {
			if passID == "" || r.PassID == passID {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (s *Service) ListNotifications(ctx context.Context, passID, typ, status string, limit int) ([]model.Notification, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ids, err := s.St.R.LRange(ctx, keyNotifList(), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Notification, 0, len(ids))
	for _, id := range ids {
		var n model.Notification
		if err := s.St.GetJSON(ctx, keyNotif(id), &n); err == nil {
			if passID != "" && n.PassID != passID {
				continue
			}
			if typ != "" && n.Type != typ {
				continue
			}
			if status != "" && n.Status != status {
				continue
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *Service) GetReviewTask(ctx context.Context, id string) (*model.ReviewTask, error) {
	var rv model.ReviewTask
	if err := s.St.GetJSON(ctx, keyReview(id), &rv); err != nil {
		return nil, ErrNotFound
	}
	return &rv, nil
}

func (s *Service) ListReviewTasks(ctx context.Context, status string, limit int) ([]model.ReviewTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if status == "" {
		ids, err := s.St.R.LRange(ctx, keyReviewList(), 0, int64(limit-1)).Result()
		if err != nil {
			return nil, err
		}
		return loadReviews(ctx, s, ids)
	}
	ids, err := s.St.R.SMembers(ctx, keyReviewStatus(status)).Result()
	if err != nil {
		return nil, err
	}
	// 最新优先
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return loadReviews(ctx, s, ids)
}

func loadReviews(ctx context.Context, s *Service, ids []string) ([]model.ReviewTask, error) {
	out := make([]model.ReviewTask, 0, len(ids))
	for _, id := range ids {
		var rv model.ReviewTask
		if err := s.St.GetJSON(ctx, keyReview(id), &rv); err == nil {
			out = append(out, rv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ---------- 离线事件与设备可靠性 ----------

// EnsureOpenIncident 登记设备离线事件（心跳超时在线核验转人工时调用）；已有 OPEN 事件不重复登记
func (s *Service) EnsureOpenIncident(ctx context.Context, dev *model.Device, startedAt time.Time) *model.OfflineIncident {
	if existID, _ := s.St.R.Get(ctx, keyDeviceOpenInc(dev.ID)).Result(); existID != "" {
		var inc model.OfflineIncident
		if err := s.St.GetJSON(ctx, keyIncident(existID), &inc); err == nil {
			return &inc
		}
	}
	id, _ := s.St.NextID(ctx, "offinc", "OI")
	inc := &model.OfflineIncident{
		ID: id, DeviceID: dev.ID, GateID: dev.GateID, Attraction: dev.Attraction,
		StartedAt: startedAt, Status: "OPEN",
	}
	_ = s.St.SetJSON(ctx, keyIncident(id), inc)
	s.St.R.SAdd(ctx, keyIncidentList(), id)
	s.St.R.SAdd(ctx, keyDeviceIncidents(dev.ID), id)
	s.St.R.Set(ctx, keyDeviceOpenInc(dev.ID), id, 0)
	return inc
}

func (s *Service) closeIncidentForBatch(ctx context.Context, dev *model.Device, batch *model.OfflineBatch) {
	incID, _ := s.St.R.Get(ctx, keyDeviceOpenInc(dev.ID)).Result()
	var inc *model.OfflineIncident
	if incID != "" {
		inc, _ = s.GetIncident(ctx, incID)
	}
	if inc == nil {
		// 无心跳超时登记（闸机直接断网后恢复），按批次区间补登记一次事件
		inc = s.EnsureOpenIncident(ctx, dev, batch.OfflineFrom)
	}
	if batch.OfflineFrom.Before(inc.StartedAt) {
		inc.StartedAt = batch.OfflineFrom
	}
	now := batch.MergedAt
	inc.RecoveredAt = &now
	inc.DurationSec = int64(now.Sub(inc.StartedAt).Seconds())
	if inc.DurationSec < 0 {
		inc.DurationSec = 0
	}
	inc.OfflineLogs = batch.Total
	inc.BatchID = batch.ID
	inc.Status = "CLOSED"
	_ = s.St.SetJSON(ctx, keyIncident(inc.ID), inc)
	s.St.R.Del(ctx, keyDeviceOpenInc(dev.ID))
}

func (s *Service) GetIncident(ctx context.Context, id string) (*model.OfflineIncident, error) {
	var inc model.OfflineIncident
	if err := s.St.GetJSON(ctx, keyIncident(id), &inc); err != nil {
		return nil, ErrNotFound
	}
	return &inc, nil
}

// DeviceReliability 聚合设备离线情况，按离线次数/时长倒序（最容易离线的闸机排最前）
func (s *Service) DeviceReliability(ctx context.Context) ([]model.DeviceReliability, error) {
	devices, err := s.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	incIDs, _ := s.St.R.SMembers(ctx, keyIncidentList()).Result()
	incByDevice := map[string][]model.OfflineIncident{}
	for _, id := range incIDs {
		var inc model.OfflineIncident
		if err := s.St.GetJSON(ctx, keyIncident(id), &inc); err == nil {
			incByDevice[inc.DeviceID] = append(incByDevice[inc.DeviceID], inc)
		}
	}
	refundIDs, _ := s.St.R.LRange(ctx, keyRefundList(), 0, 499).Result()
	refundByDevice := map[string]int64{}
	for _, id := range refundIDs {
		var r model.QuotaRefund
		if err := s.St.GetJSON(ctx, keyRefund(id), &r); err == nil {
			refundByDevice[r.DeviceID] += int64(r.Quota)
		}
	}
	out := make([]model.DeviceReliability, 0, len(devices))
	for _, d := range devices {
		incs := incByDevice[d.ID]
		sort.Slice(incs, func(i, j int) bool { return incs[i].StartedAt.After(incs[j].StartedAt) })
		rel := model.DeviceReliability{
			DeviceID: d.ID, Name: d.Name, GateID: d.GateID, Attraction: d.Attraction, Online: d.Online,
			OfflineCount: int64(len(incs)), RecentIncidents: []model.OfflineIncident{},
			RefundQuota: refundByDevice[d.ID],
		}
		var last *time.Time
		for _, inc := range incs {
			rel.TotalDowntimeSec += inc.DurationSec
			rel.OfflineLogCount += int64(inc.OfflineLogs)
			if inc.Status == "OPEN" {
				rel.OpenIncidents++
			}
			t := inc.StartedAt
			if last == nil || t.After(*last) {
				lt := t
				last = &lt
			}
		}
		rel.LastOfflineAt = last
		if len(incs) > 5 {
			incs = incs[:5]
		}
		rel.RecentIncidents = incs
		out = append(out, rel)
	}
	// 最容易离线：离线次数多 → 累计时长久 → 离线日志多
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OfflineCount != out[j].OfflineCount {
			return out[i].OfflineCount > out[j].OfflineCount
		}
		if out[i].TotalDowntimeSec != out[j].TotalDowntimeSec {
			return out[i].TotalDowntimeSec > out[j].TotalDowntimeSec
		}
		return out[i].OfflineLogCount > out[j].OfflineLogCount
	})
	return out, nil
}

// ListIncidents 离线事件列表（可按设备过滤）
func (s *Service) ListIncidents(ctx context.Context, deviceID string, limit int) ([]model.OfflineIncident, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var ids []string
	if deviceID != "" {
		ids, _ = s.St.R.SMembers(ctx, keyDeviceIncidents(deviceID)).Result()
	} else {
		ids, _ = s.St.R.SMembers(ctx, keyIncidentList()).Result()
	}
	out := make([]model.OfflineIncident, 0, len(ids))
	for _, id := range ids {
		var inc model.OfflineIncident
		if err := s.St.GetJSON(ctx, keyIncident(id), &inc); err == nil {
			out = append(out, inc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------- 辅助 ----------

func parseEntryTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func maskIDCard(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:3] + "********" + s[len(s)-3:]
}
