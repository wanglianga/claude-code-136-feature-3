package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
	"passgate/internal/model"
)

// ---------- 入参 ----------

// OfflineEventInput 闸机上报的一条离线放行日志
type OfflineEventInput struct {
	RequestID      string            `json:"request_id"` // 闸机本地幂等键（缺省用 event_id）
	EventID        string            `json:"event_id"`
	PassID         string            `json:"pass_id"` // 闸机本地缓存识别出的年卡
	PrincipalID    string            `json:"principal_id_card"`
	PrincipalName  string            `json:"principal_name"`
	Companions     []string          `json:"companions"` // 全部同行人证件（含未绑定）
	CompanionNames map[string]string `json:"companion_names"`
	FaceScore      float64           `json:"face_score"`
	LocalTime      string            `json:"local_time"` // RFC3339 闸机本地时间
}

// OfflineBatchInput 闸机恢复后上报的一批离线日志
type OfflineBatchInput struct {
	DeviceID    string              `json:"device_id"`
	OfflineFrom string              `json:"offline_from"`
	OfflineTo   string              `json:"offline_to"`
	Operator    string              `json:"operator"`
	Events      []OfflineEventInput `json:"events"`
}

// 离线合并过程中的内部类型
type passCtx struct {
	pass  *model.Pass
	bound map[string]bool
}

type pendingEvent struct {
	ev           OfflineEventInput
	rec          model.OfflineEvent
	pc           *passCtx
	verifID      string
	keep         int    // 该事件实际计入权威扣减的人次
	localCharged int    // 闸机本地流水记录的扣次（审计）
	refundQ      int    // 判定为重复扣次的人次（审计）
	reason       string // 合并后核验原因码
	detail       string
}

type offlineGroupAgg struct {
	passID        string
	attraction    string
	principalID   string
	principalName string
	date          string
	evs           []*pendingEvent
	unbound       map[string]string
}

type rollbackEntry struct {
	passID string
	n      int
}

// reconcileChargeScript 离线对账（单事件复核追扣用）：只扣额度。
// KEYS: remaining, used；ARGV: need；返回 {code, charged} code:1=成功 3=无额度记录 4=不足
var reconcileChargeScript = redis.NewScript(`
local remaining = tonumber(redis.call('GET', KEYS[1]) or '-1')
if remaining < 0 then return {3, 0} end
local need = tonumber(ARGV[1])
if remaining < need then return {4, 0} end
redis.call('DECRBY', KEYS[1], need)
redis.call('INCRBY', KEYS[2], need)
return {1, need}
`)

// reconcileGroupScript 离线合并组级对账：原子完成「回放闸机本地总扣次 → 回补重复部分」。
// 净扣减 = totalNeed - refundQ = headCount（实际入园人次）。
// KEYS: remaining, used；ARGV: totalNeed, refundQ；返回 code：1=成功 3=无额度记录 4=不足
var reconcileGroupScript = redis.NewScript(`
local remaining = tonumber(redis.call('GET', KEYS[1]) or '-1')
if remaining < 0 then return 3 end
local need = tonumber(ARGV[1])
local refund = tonumber(ARGV[2])
-- 净扣减 = 实际入园人次；瞬态的 totalNeed 扣减在本脚本内立即回补，外部不可见
if remaining < need - refund then return 4 end
redis.call('DECRBY', KEYS[1], need)
redis.call('INCRBY', KEYS[2], need)
if refund > 0 then
  redis.call('INCRBY', KEYS[1], refund)
  redis.call('DECRBY', KEYS[2], refund)
end
return 1
`)

// MergeOfflineLogs 合并一台闸机离线期间的放行日志：
//  1. 逐组识别「同一游客同一景点同一次入园」的重复刷闸；
//  2. 按实际入园人次扣减，重复扣减部分自动回补并短信通知游客；
//  3. 未绑定同行人生成现场复核单（补登记 / 扣权益 / 标记异常放行）；
//  4. 累计闸机离线指标，供维护计划与高峰调度参考。
func (s *Service) MergeOfflineLogs(ctx context.Context, in OfflineBatchInput) (batch *model.OfflineBatch, err error) {
	if in.DeviceID == "" || len(in.Events) == 0 {
		return nil, fmt.Errorf("%w: device_id / events required", ErrInvalidInput)
	}
	dev, err := s.GetDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: device %s not registered", ErrInvalidInput, in.DeviceID)
	}
	// 失败恢复：记录本批占用的 request_id 与已入账净额；处理中途失败时统一回滚，允许整批安全重试
	var claimed []string
	var rollbackCharges []rollbackEntry
	defer func() {
		if err == nil {
			return
		}
		for _, rc := range rollbackCharges {
			s.St.R.IncrBy(ctx, keyRemaining(rc.passID), int64(rc.n))
			s.St.R.DecrBy(ctx, keyUsed(rc.passID), int64(rc.n))
		}
		if len(claimed) > 0 {
			keys := make([]string, len(claimed))
			for i, req := range claimed {
				keys[i] = keyVerifReq(req)
			}
			s.St.R.Del(ctx, keys...)
		}
	}()
	now := time.Now()
	from, to := parseOfflineTime(in.OfflineFrom, now), parseOfflineTime(in.OfflineTo, now)
	if to.Before(from) {
		from, to = to, from
	}
	batchID, err := s.St.NextID(ctx, "offbatch", "OB")
	if err != nil {
		return nil, err
	}
	batch = &model.OfflineBatch{
		ID: batchID, DeviceID: dev.ID, GateID: dev.GateID, Attraction: dev.Attraction,
		OfflineFrom: from, OfflineTo: to, Operator: in.Operator, CreatedAt: now,
	}

	cache := map[string]*passCtx{}
	groups := map[string]*offlineGroupAgg{}
	var skipped int

	// ---------- 第 1 步：解析年卡、按游客+景点+日期分组、request_id 幂等 ----------
	for _, ev := range in.Events {
		if ev.RequestID == "" {
			ev.RequestID = ev.EventID
		}
		if ev.RequestID == "" {
			ev.RequestID = fmt.Sprintf("OFF-%s-%d", dev.ID, now.UnixNano())
		}
		verifID, _ := s.St.NextID(ctx, "verif", "V")
		placed, err := s.St.R.SetNX(ctx, keyVerifReq(ev.RequestID), verifID, 24*time.Hour).Result()
		if err != nil {
			return nil, err
		}
		if !placed {
			// 该离线日志此前已合并/补传过，幂等跳过
			skipped++
			continue
		}
		claimed = append(claimed, ev.RequestID)
		lt := parseOfflineTime(ev.LocalTime, now)
		rec := model.OfflineEvent{
			EventID: ev.EventID, RequestID: ev.RequestID, DeviceID: dev.ID, GateID: dev.GateID,
			Attraction: dev.Attraction, PrincipalID: ev.PrincipalID, PrincipalName: ev.PrincipalName,
			PassID: ev.PassID, Companions: ev.Companions, FaceScore: ev.FaceScore,
			LocalTime: lt, SyncedAt: now,
		}
		// 定位年卡：事件自带 pass_id 优先，否则按主入园人证件反查成员索引
		var pc *passCtx
		if ev.PassID != "" {
			if p, e := s.getPass(ctx, ev.PassID); e == nil {
				pc = s.loadPassCtx(ctx, cache, p)
				rec.PassID = p.ID
			}
		}
		if pc == nil && ev.PrincipalID != "" {
			if p, e := s.findPassByVerifyKeys(ctx, "", "", ev.PrincipalID); e == nil {
				pc = s.loadPassCtx(ctx, cache, p)
				rec.PassID = p.ID
				if rec.PrincipalName == "" {
					rec.PrincipalName = p.HolderName
				}
			}
		}
		passID, principalID := "", ev.PrincipalID
		principalName := ev.PrincipalName
		if pc != nil {
			passID = pc.pass.ID
			if principalName == "" {
				principalName = pc.pass.HolderName
			}
		}
		date := lt.Format("2006-01-02")
		key := passID + "|" + dev.Attraction + "|" + date + "|" + principalID
		g, ok := groups[key]
		if !ok {
			g = &offlineGroupAgg{
				passID: passID, attraction: dev.Attraction, principalID: principalID,
				principalName: principalName, date: date, unbound: map[string]string{},
			}
			groups[key] = g
		}
		pn := &pendingEvent{ev: ev, rec: rec, pc: pc, verifID: verifID}
		g.evs = append(g.evs, pn)
		for _, c := range ev.Companions {
			if c == principalID {
				continue
			}
			if pc == nil || !pc.bound[c] {
				name := ""
				if ev.CompanionNames != nil {
					name = ev.CompanionNames[c]
				}
				g.unbound[c] = name
			}
		}
	}

	// ---------- 第 2 步：纯计算——分组、闸机本地扣次审计、实际入园人次、回补额、处置类型 ----------
	type chargePlan struct {
		pass                 *model.Pass
		g                    *offlineGroupAgg
		gv                   model.OfflineGroup
		head, total, refundQ int
	}
	var plans []chargePlan
	groupViews := make([]model.OfflineGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.evs, func(i, j int) bool { return g.evs[i].rec.LocalTime.Before(g.evs[j].rec.LocalTime) })
		gv := model.OfflineGroup{
			Key:    g.passID + "|" + g.attraction + "|" + g.date + "|" + g.principalID,
			PassID: g.passID, Attraction: g.attraction, PrincipalID: g.principalID,
			PrincipalName: g.principalName,
		}
		for _, pn := range g.evs {
			gv.EventIDs = append(gv.EventIDs, pn.rec.RequestID)
			gv.Times = append(gv.Times, pn.rec.LocalTime.Format("15:04:05"))
		}
		for id := range g.unbound {
			gv.UnboundIDCard = append(gv.UnboundIDCard, id)
		}
		sort.Strings(gv.UnboundIDCard)

		if g.passID == "" {
			// 无法定位年卡：闸机离线放出的人员无法对账，落异常待人工核查
			gv.Status = "NO_PASS"
			for _, pn := range g.evs {
				pn.keep, pn.localCharged, pn.refundQ = 0, 0, 0
				pn.reason, pn.detail = model.ReasonPassNotFound,
					"离线放行记录无法匹配年卡（证件 "+pn.ev.PrincipalID+"），待人工核查"
			}
			batch.Status = "PARTIAL"
			groupViews = append(groupViews, gv)
			continue
		}
		pc := g.evs[0].pc
		// 每条离线放行日志按「当次闸机放行人数」记本地扣次（含主入园人 + 当次已绑定同行）；
		// 组级实际入园人次按人去重（同一同行人在组内多次出现只算 1 人次）。
		boundSeen := map[string]bool{}
		var totalNeed int
		for _, pn := range g.evs {
			need := 1
			for _, c := range pn.ev.Companions {
				if c != g.principalID && pc.bound[c] {
					need++
					boundSeen[c] = true
				}
			}
			pn.localCharged = need
			totalNeed += need
		}
		headCount := 1 + len(boundSeen)
		gv.HeadCount, gv.ChargedTotal = headCount, totalNeed
		gv.RefundQuota = totalNeed - headCount

		// 贪心保留 headCount 人次（按时间最早的入园），其余判为重复扣次（仅审计，不再入权威计数器）
		retained := 0
		for _, pn := range g.evs {
			keep := pn.localCharged
			if retained < headCount {
				if avail := headCount - retained; keep > avail {
					keep = avail
				}
			} else {
				keep = 0
			}
			pn.keep, pn.refundQ = keep, pn.localCharged-keep
			retained += keep
			if pn.refundQ > 0 {
				pn.reason = model.ReasonOfflineDupRefund
				pn.detail = fmt.Sprintf("离线日志合并：同一游客 %s 在 %s 的重复刷闸，闸机本地扣 %d 人次，回补 %d 人次",
					principalDisplay(g), g.attraction, pn.localCharged, pn.refundQ)
			} else {
				pn.detail = fmt.Sprintf("离线日志合并：%s 入园 %s，扣减权益 %d 人次",
					principalDisplay(g), g.attraction, keep)
			}
		}
		// 额度预检：实际入园人次都不足时，该组转未决，不做部分扣减
		rem, _ := s.remainingQuota(ctx, g.passID)
		if headCount > rem {
			gv.Status = "UNRESOLVED"
			batch.Status = "PARTIAL"
			for _, pn := range g.evs {
				pn.keep, pn.localCharged, pn.refundQ = 0, 0, 0
				pn.reason = model.ReasonQuotaExhausted
				pn.detail = fmt.Sprintf("离线对账：该游客实际入园 %d 人次，当前剩余 %d，待客服处理", headCount, rem)
			}
			groupViews = append(groupViews, gv)
			continue
		}
		if gv.RefundQuota > 0 {
			gv.Status = "REFUNDED"
		} else {
			gv.Status = "MERGED_NO_DUP"
		}
		plans = append(plans, chargePlan{
			pass: pc.pass, g: g, gv: gv,
			head: headCount, total: totalNeed, refundQ: gv.RefundQuota,
		})
		groupViews = append(groupViews, gv)
	}

	// ---------- 第 3 步：组级原子对账（回放总扣次 totalNeed 并回补 refundQ，净扣 head）；
	//                任一失败由 defer 统一按净额回滚已扣组、释放本批幂等占位 ----------
	for _, pl := range plans {
		code, e := s.runReconcileGroup(ctx, pl.pass.ID, pl.total, pl.refundQ)
		if e != nil {
			return nil, e
		}
		if code != 1 {
			return nil, ErrQuotaInsufficient
		}
		rollbackCharges = append(rollbackCharges, rollbackEntry{passID: pl.pass.ID, n: pl.head})
	}

	// ---------- 第 4 步：全部扣减成功，落库（事件/核验/统计/通知/复核单/批次/指标） ----------
	for _, g := range groups {
		pc := g.evs[0].pc
		var gv *model.OfflineGroup
		for i := range groupViews {
			if groupViews[i].Key == g.passID+"|"+g.attraction+"|"+g.date+"|"+g.principalID {
				gv = &groupViews[i]
			}
		}
		for _, pn := range g.evs {
			s.persistOfflineEvent(ctx, &pn.rec)
			decision := model.DecisionAllow
			if pn.keep == 0 && (pn.reason == model.ReasonPassNotFound || pn.reason == model.ReasonQuotaExhausted) {
				if pn.reason == model.ReasonPassNotFound {
					decision = model.DecisionManual
				} else {
					decision = model.DecisionQuota
				}
			}
			v := s.buildOfflineVerif(pn, dev, decision, pn.reason, pn.detail, pn.keep)
			if pn.refundQ > 0 {
				v.ChargedQuota = pn.localCharged
				v.RefundedQuota = pn.refundQ
			}
			s.finishOfflineVerif(ctx, v)
		}
		if gv == nil || g.passID == "" || gv.Status == "UNRESOLVED" {
			continue
		}
		if p, _ := s.getPass(ctx, g.passID); p != nil {
			s.recordEntryStats(ctx, g.date, g.evs[0].rec.LocalTime.Hour(), p.Type, g.attraction, gv.HeadCount)
		}
		if gv.RefundQuota > 0 {
			s.recordOfflineRefundAudit(ctx, g.passID, pc.pass, batchID, g.attraction, gv.RefundQuota, gv.ChargedTotal)
			s.notifyOfflineRefund(ctx, g.passID, pc.pass, *gv, from, to)
			batch.RefundTotal += gv.RefundQuota
		}
		// 未绑定同行人 → 现场复核单
		for _, idCard := range gv.UnboundIDCard {
			rv := s.createOfflineReview(ctx, batchID, dev, pc.pass, g, idCard, g.unbound[idCard], g.evs[0])
			batch.ReviewIDs = append(batch.ReviewIDs, rv.ID)
		}
	}

	sort.Slice(groupViews, func(i, j int) bool { return groupViews[i].Key < groupViews[j].Key })
	batch.Groups = groupViews
	batch.EventCount = len(in.Events)
	batch.AllowedCount = len(in.Events) - skipped
	if batch.Status == "" {
		batch.Status = "PROCESSED"
	}
	// 整批日志此前均已合并过（request_id 全部命中幂等）：不再产生空批次与重复指标
	if skipped == len(in.Events) {
		return nil, fmt.Errorf("%w: batch already merged (%d events skipped)", ErrConflict, skipped)
	}

	if err := s.St.SetJSON(ctx, keyOfflineBatch(batchID), batch); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyOfflineBatchList(), batchID)
	s.St.R.LPush(ctx, keyDeviceBatches(dev.ID), batchID)

	// 设备离线指标
	pipe := s.St.R.TxPipeline()
	statKey := "dev:stat:" + dev.ID
	pipe.HIncrBy(ctx, statKey, "offline_episodes", 1)
	pipe.HIncrBy(ctx, statKey, "offline_seconds", int64(to.Sub(from).Seconds()))
	pipe.HIncrBy(ctx, statKey, "offline_entries", int64(batch.AllowedCount))
	pipe.HIncrBy(ctx, statKey, "merged_batches", 1)
	pipe.HIncrBy(ctx, statKey, "refunded_quota", int64(batch.RefundTotal))
	pipe.HSet(ctx, statKey, "last_offline_from", from.Format(time.RFC3339),
		"last_offline_to", to.Format(time.RFC3339))
	if len(batch.ReviewIDs) > 0 {
		pipe.HIncrBy(ctx, statKey, "pending_reviews", int64(len(batch.ReviewIDs)))
	}
	_, _ = pipe.Exec(ctx)

	return batch, nil
}

func principalDisplay(g *offlineGroupAgg) string {
	if g.principalName != "" {
		return g.principalName
	}
	return g.principalID
}

func (s *Service) loadPassCtx(ctx context.Context, cache map[string]*passCtx, p *model.Pass) *passCtx {
	if c, ok := cache[p.ID]; ok {
		return c
	}
	members, _ := s.listMembers(ctx, p.ID)
	bound := map[string]bool{}
	for _, m := range members {
		bound[m.IDCard] = true
	}
	c := &passCtx{pass: p, bound: bound}
	cache[p.ID] = c
	return c
}

func (s *Service) runReconcileCharge(ctx context.Context, passID string, need int) (int64, int64, error) {
	raw, err := reconcileChargeScript.Run(ctx, s.St.R, []string{keyRemaining(passID), keyUsed(passID)}, need).Slice()
	if err != nil {
		return 0, 0, err
	}
	code, _ := raw[0].(int64)
	charged, _ := raw[1].(int64)
	return code, charged, nil
}

// runReconcileGroup 组级原子对账：回放本地总扣次并立即回补重复部分
func (s *Service) runReconcileGroup(ctx context.Context, passID string, totalNeed, refundQ int) (int64, error) {
	raw, err := reconcileGroupScript.Run(ctx, s.St.R,
		[]string{keyRemaining(passID), keyUsed(passID)}, totalNeed, refundQ).Result()
	if err != nil {
		return 0, err
	}
	code, _ := raw.(int64)
	return code, nil
}

func (s *Service) persistOfflineEvent(ctx context.Context, rec *model.OfflineEvent) {
	_ = s.St.SetJSON(ctx, keyOfflineEvent(rec.RequestID), rec)
}

func (s *Service) buildOfflineVerif(pn *pendingEvent, dev *model.Device, decision model.Decision,
	reason, detail string, charged int) *model.Verification {
	passID, passType, holderName := "", "", ""
	var companions []string
	if pn.pc != nil {
		passID = pn.pc.pass.ID
		passType = string(pn.pc.pass.Type)
		holderName = pn.pc.pass.HolderName
		for _, c := range pn.ev.Companions {
			if pn.pc.bound[c] {
				companions = append(companions, c)
			}
		}
	}
	return &model.Verification{
		ID: pn.verifID, RequestID: pn.rec.RequestID, PassID: passID, PassType: passType,
		HolderName: holderName, DeviceID: dev.ID, GateID: dev.GateID, Attraction: dev.Attraction,
		Decision: decision, Reason: reason, Detail: detail, FaceScore: pn.ev.FaceScore,
		Companions: companions, ChargedQuota: charged, OfflineSync: true, CreatedAt: pn.rec.LocalTime,
	}
}

// finishOfflineVerif 离线合并生成的核验记录落库（跳过在线去重标记）
func (s *Service) finishOfflineVerif(ctx context.Context, v *model.Verification) {
	_ = s.St.SetJSON(ctx, keyVerif(v.ID), v)
	if v.PassID != "" {
		s.St.R.LPush(ctx, keyPassVerifs(v.PassID), v.ID)
	}
	action := "DENY"
	if v.Decision == model.DecisionAllow {
		action = "ALLOW"
	}
	s.writeGateLog(ctx, v, action, "offline-sync")
	if v.Decision != model.DecisionAllow && v.Reason != "" {
		var pt model.PassType
		if v.PassType != "" {
			pt = model.PassType(v.PassType)
		}
		s.recordAnomalyStats(ctx, dateOf(v.CreatedAt), pt, v.Reason)
	}
}

// recordOfflineRefundAudit 离线合并回补审计：重复部分从未进入权威计数器（净额只扣 headCount），
// 因此这里不动 remaining/used，只写变更日志与运营回补统计，供对账与通知使用。
func (s *Service) recordOfflineRefundAudit(ctx context.Context, passID string, p *model.Pass,
	batchID, attraction string, refundQuota, localCharged int) {
	s.statIncr(ctx, "stats:offline:refund:"+dateOf(time.Now()), int64(refundQuota))
	s.addChangeLog(ctx, passID, "OFFLINE_REFUND",
		fmt.Sprintf("离线批次 %s 合并：%s 闸机本地扣 %d 人次，识别重复刷闸回补 %d 人次（权威计数器按实际入园净额入账）by system",
			batchID, attraction, localCharged, refundQuota), "system")
}

// ---------- 复核单 ----------

func (s *Service) createOfflineReview(ctx context.Context, batchID string, dev *model.Device,
	p *model.Pass, g *offlineGroupAgg, idCard, name string, firstEv *pendingEvent) *model.OfflineReview {
	id, _ := s.St.NextID(ctx, "offreview", "OR")
	rv := &model.OfflineReview{
		ID: id, BatchID: batchID, EventID: firstEv.rec.RequestID, DeviceID: dev.ID, GateID: dev.GateID,
		Attraction: dev.Attraction, PassID: p.ID, HolderName: p.HolderName,
		PrincipalID: g.principalID, PrincipalName: g.principalName,
		UnboundIDCard: idCard, UnboundName: name, EnteredAt: firstEv.rec.LocalTime,
		Status: "PENDING", CreatedAt: time.Now(),
	}
	_ = s.St.SetJSON(ctx, keyOfflineReview(id), rv)
	s.St.R.LPush(ctx, keyOfflineReviewList(), id)
	s.St.R.LPush(ctx, keyPassReviews(p.ID), id)
	return rv
}

// OfflineReviewInput 现场复核处置入参
type OfflineReviewInput struct {
	Action   string `json:"action"` // REGISTER | CHARGE | ANOMALY
	Name     string `json:"name"`   // REGISTER 时补登记人员姓名
	Relation string `json:"relation"`
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

// ResolveOfflineReview 现场处置：补登记 / 扣权益 / 标记异常放行
func (s *Service) ResolveOfflineReview(ctx context.Context, id string, in OfflineReviewInput) (*model.OfflineReview, error) {
	if in.Operator == "" || in.Note == "" {
		return nil, fmt.Errorf("%w: operator / note required", ErrInvalidInput)
	}
	if in.Action != model.ReviewActionRegister && in.Action != model.ReviewActionCharge && in.Action != model.ReviewActionAnomaly {
		return nil, fmt.Errorf("%w: action must be REGISTER / CHARGE / ANOMALY", ErrInvalidInput)
	}
	token, ok, err := s.St.Lock(ctx, "offreview:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "offreview:"+id, token)

	var rv model.OfflineReview
	if err := s.St.GetJSON(ctx, keyOfflineReview(id), &rv); err != nil {
		return nil, ErrNotFound
	}
	if rv.Status != "PENDING" {
		return nil, ErrAlreadyProcessed
	}
	now := time.Now()
	rv.ResolvedAt = &now
	rv.Action = in.Action
	rv.Operator = in.Operator
	rv.Note = in.Note

	switch in.Action {
	case model.ReviewActionRegister:
		name := in.Name
		if name == "" {
			name = rv.UnboundName
		}
		if name == "" {
			name = "离线补登记人员"
		}
		m, err := s.AddMember(ctx, rv.PassID, MemberInput{
			Name: name, IDCard: rv.UnboundIDCard, Relation: defaultRelation(in.Relation),
		}, in.Operator)
		if err != nil {
			return nil, err
		}
		rv.Status = "REGISTERED"
		rv.UnboundName = m.Name
		s.addChangeLog(ctx, rv.PassID, "OFFLINE_REVIEW_REGISTER",
			fmt.Sprintf("离线批次 %s 未绑定同行人 %s（%s）补登记 by %s：%s",
				rv.BatchID, m.Name, rv.UnboundIDCard, in.Operator, in.Note), in.Operator)
	case model.ReviewActionCharge:
		code, _, err := s.runReconcileCharge(ctx, rv.PassID, 1)
		if err != nil {
			return nil, err
		}
		if code != 1 {
			return nil, ErrQuotaInsufficient
		}
		rv.Status = "CHARGED"
		rv.ChargedQuota = 1
		if p, e := s.getPass(ctx, rv.PassID); e == nil {
			s.recordEntryStats(ctx, dateOf(rv.EnteredAt), rv.EnteredAt.Hour(), p.Type, rv.Attraction, 1)
		}
		s.addChangeLog(ctx, rv.PassID, "OFFLINE_REVIEW_CHARGE",
			fmt.Sprintf("离线批次 %s 未绑定同行人 %s 确认入园，追扣 1 人次 by %s：%s",
				rv.BatchID, rv.UnboundIDCard, in.Operator, in.Note), in.Operator)
	case model.ReviewActionAnomaly:
		rv.Status = "ANOMALY"
		s.statIncr(ctx, "stats:offline:anomaly:"+dateOf(now), 1)
		if p, e := s.getPass(ctx, rv.PassID); e == nil {
			s.recordAnomalyStats(ctx, dateOf(now), p.Type, model.ReasonOfflineAnomaly)
		}
		s.addChangeLog(ctx, rv.PassID, "OFFLINE_REVIEW_ANOMALY",
			fmt.Sprintf("离线批次 %s 未绑定同行人 %s 标记异常放行 by %s：%s",
				rv.BatchID, rv.UnboundIDCard, in.Operator, in.Note), in.Operator)
	}

	if err := s.St.SetJSON(ctx, keyOfflineReview(id), &rv); err != nil {
		return nil, err
	}
	s.St.R.HIncrBy(ctx, "dev:stat:"+rv.DeviceID, "pending_reviews", -1)
	if in.Action == model.ReviewActionAnomaly {
		s.St.R.HIncrBy(ctx, "dev:stat:"+rv.DeviceID, "anomaly_releases", 1)
	}
	s.notifyReviewResolved(ctx, &rv)
	return &rv, nil
}

func defaultRelation(r string) string {
	if r == "" {
		return "other"
	}
	return r
}

// ---------- 查询 ----------

func (s *Service) GetOfflineBatch(ctx context.Context, id string) (*model.OfflineBatch, error) {
	var b model.OfflineBatch
	if err := s.St.GetJSON(ctx, keyOfflineBatch(id), &b); err != nil {
		return nil, ErrNotFound
	}
	return &b, nil
}

func (s *Service) ListOfflineBatches(ctx context.Context, limit int) ([]model.OfflineBatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ids, err := s.St.R.LRange(ctx, keyOfflineBatchList(), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.OfflineBatch, 0, len(ids))
	for _, id := range ids {
		var b model.OfflineBatch
		if err := s.St.GetJSON(ctx, keyOfflineBatch(id), &b); err == nil {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *Service) ListOfflineReviews(ctx context.Context, status string) ([]model.OfflineReview, error) {
	ids, err := s.St.R.LRange(ctx, keyOfflineReviewList(), 0, 499).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.OfflineReview, 0, len(ids))
	for _, id := range ids {
		var rv model.OfflineReview
		if err := s.St.GetJSON(ctx, keyOfflineReview(id), &rv); err == nil {
			if status == "" || rv.Status == status {
				out = append(out, rv)
			}
		}
	}
	return out, nil
}

// ---------- 通知 ----------

func (s *Service) notify(ctx context.Context, passID, channel, typ, title, content string) {
	phone := ""
	if p, err := s.getPass(ctx, passID); err == nil {
		phone = p.Phone
	}
	id, _ := s.St.NextID(ctx, "notif", "N")
	n := model.VisitorNotification{
		ID: id, PassID: passID, Phone: phone, Channel: channel, Type: typ,
		Title: title, Content: content, CreatedAt: time.Now(),
	}
	_ = s.St.SetJSON(ctx, keyNotification(id), &n)
	s.St.R.LPush(ctx, keyNotificationList(), id)
	s.St.R.LPush(ctx, keyPassNotifs(passID), id)
	if channel == "SMS" && phone != "" {
		// 演示环境：短信出站只落库（sms:out），生产对接短信网关
		s.St.R.LPush(ctx, "sms:out", fmt.Sprintf("%s|%s|%s", phone, title, content))
	}
}

func (s *Service) notifyOfflineRefund(ctx context.Context, passID string, p *model.Pass,
	g model.OfflineGroup, from, to time.Time) {
	title := "离线重复扣次回补通知"
	content := fmt.Sprintf("【景区年卡】您于离线时段(%s~%s)在 %s 的入园记录合并完成，识别到重复扣次 %d 人次，已自动回补至年卡 %s，请注意查收。",
		from.Format("01-02 15:04"), to.Format("01-02 15:04"), g.Attraction, g.RefundQuota, passID)
	s.notify(ctx, passID, "SMS", "OFFLINE_REFUND", title, content)
}

func (s *Service) notifyReviewResolved(ctx context.Context, rv *model.OfflineReview) {
	title := "离线入园复核结果通知"
	var content string
	switch rv.Status {
	case "REGISTERED":
		content = fmt.Sprintf("【景区年卡】您离线入园时同行人 %s 的补登记已完成，本次未追扣权益。", rv.UnboundIDCard)
	case "CHARGED":
		content = fmt.Sprintf("【景区年卡】您离线入园时同行人 %s 已确认入园，已追扣 1 人次权益。", rv.UnboundIDCard)
	default:
		content = fmt.Sprintf("【景区年卡】您离线入园时同行人 %s 的记录已标记为异常放行，客服将进一步与您联系。", rv.UnboundIDCard)
	}
	s.notify(ctx, rv.PassID, "SMS", "REVIEW_RESOLVED", title, content)
}

func (s *Service) ListNotifications(ctx context.Context, passID string, limit int) ([]model.VisitorNotification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	key := keyNotificationList()
	if passID != "" {
		key = keyPassNotifs(passID)
	}
	ids, err := s.St.R.LRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.VisitorNotification, 0, len(ids))
	for _, id := range ids {
		var n model.VisitorNotification
		if err := s.St.GetJSON(ctx, keyNotification(id), &n); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// ---------- 设备离线指标 ----------

// DeviceOfflineRanking 闸机离线排行（维护计划 / 高峰入口调度参考）
func (s *Service) DeviceOfflineRanking(ctx context.Context) ([]model.DeviceOfflineStat, error) {
	devices, err := s.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.DeviceOfflineStat, 0, len(devices))
	for _, d := range devices {
		st := model.DeviceOfflineStat{
			DeviceID: d.ID, Name: d.Name, GateID: d.GateID, Attraction: d.Attraction, Online: d.Online,
		}
		fields, _ := s.St.R.HGetAll(ctx, "dev:stat:"+d.ID).Result()
		st.OfflineEpisodes = atoi64(fields["offline_episodes"])
		st.OfflineSeconds = atoi64(fields["offline_seconds"])
		st.OfflineEntries = atoi64(fields["offline_entries"])
		st.MergedBatches = atoi64(fields["merged_batches"])
		st.RefundedQuota = atoi64(fields["refunded_quota"])
		st.PendingReviews = atoi64(fields["pending_reviews"])
		st.AnomalyReleases = atoi64(fields["anomaly_releases"])
		st.LastOfflineFrom = fields["last_offline_from"]
		st.LastOfflineTo = fields["last_offline_to"]
		// 风险分 = 离线次数*30 + 离线分钟数 + 离线放行*0.2 + 待复核*10 + 异常放行*20
		st.RiskScore = float64(st.OfflineEpisodes*30) + float64(st.OfflineSeconds)/60.0 +
			float64(st.OfflineEntries)*0.2 + float64(st.PendingReviews)*10 + float64(st.AnomalyReleases)*20
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RiskScore != out[j].RiskScore {
			return out[i].RiskScore > out[j].RiskScore
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out, nil
}

func parseOfflineTime(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

func atoi64(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}
