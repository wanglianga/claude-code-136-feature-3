#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""passgate 端到端验证脚本：覆盖购卡、核验决策、幂等并发、冻结/退款/升级、申诉、运营统计。

前置：服务已在 127.0.0.1:8080 运行（空库启动会自动写入种子数据）。
运行：python3 test/e2e.py
"""
import datetime
import json
import subprocess
import sys
import time
import urllib.request
import urllib.error
import uuid
from concurrent.futures import ThreadPoolExecutor

BASE = "http://127.0.0.1:8080"
PASS = 0
FAIL = 0


def check(name, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  ✓ {name}")
    else:
        FAIL += 1
        print(f"  ✗ {name}  {extra}")


def section(title):
    print(f"\n== {title} ==")


def req(method, path, body=None, headers=None):
    url = BASE + path
    data = None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    if body is not None:
        data = json.dumps(body).encode()
    r = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode() or "{}")
        except Exception:
            return e.code, {}


def redis_cli(*args, stdin=None):
    cmd = ["redis-cli"] + list(args)
    res = subprocess.run(cmd, input=stdin, capture_output=True, text=True)
    return res.stdout.strip()


def redis_set_json(key, obj):
    redis_cli("-x", "SET", key, stdin=json.dumps(obj))


def redis_get_json(key):
    out = redis_cli("GET", key)
    return json.loads(out) if out else None


def local_date():
    return datetime.datetime.now().astimezone().strftime("%Y-%m-%d")


# 正常业务流统一使用动态计算的「最近工作日」（>= 今天、非节假日、周一至周五），
# 使测试与运行当天是否节假日解耦，且保证在新购卡（valid_from=今天）的有效期内；
# 节假日专项断言显式使用 "2026-10-01"（种子配置的国庆节假日）。A 段取到节假日后会覆盖该值。
WORKDAY = None


def local_ts(hour=10, minute=30, date=None):
    now = datetime.datetime.now().astimezone()
    tz = now.strftime("%z")
    tz = tz[:3] + ":" + tz[3:]
    d = date or WORKDAY
    return f"{d}T{hour:02d}:{minute:02d}:00{tz}"


_uid = 0


def new_idcard():
    global _uid
    _uid += 1
    return "ID" + uuid.uuid4().hex[:14] + str(_uid)


def buy_pass(ptype="single", members=None, expiry="2032-01-01", phone=None):
    body = {
        "type": ptype,
        "holder_name": "测试用户" + str(_uid),
        "holder_id_card": new_idcard(),
        "id_card_expiry": expiry,
        "phone": phone or ("139" + str(uuid.uuid4().int)[:8]),
        "photo_url": "https://img.example.com/p.jpg",
    }
    if members:
        body["members"] = members
    st, r = req("POST", "/api/passes", body)
    assert st == 200, f"buy_pass failed: {r}"
    return r["data"]


def verify(pass_id=None, qr=None, idcard=None, device="dev-east-01", attraction="attr-lake",
           face=90.0, companions=None, req_id=None, ts=None, photo_clear=None, offline_sync=False):
    body = {
        "request_id": req_id or ("REQ-" + uuid.uuid4().hex[:12]),
        "device_id": device,
        "attraction": attraction,
        "face_score": face,
        "offline_sync": offline_sync,
    }
    if pass_id:
        body["pass_id"] = pass_id
    if qr:
        body["qr_token"] = qr
    if idcard:
        body["id_card"] = idcard
    if companions:
        body["companions"] = companions
    if ts:
        body["timestamp"] = ts
    if photo_clear is not None:
        body["photo_clear"] = photo_clear
    st, r = req("POST", "/api/verify", body)
    return st, r


def remaining(pass_id):
    st, r = req("GET", f"/api/passes/{pass_id}")
    return r["data"]["remaining_quota"]


# ============================================================
section("A. 健康检查与种子数据")
st, r = req("GET", "/healthz")
check("健康检查", st == 200 and r.get("status") == "ok")
st, r = req("GET", "/api/devices")
check("种子设备 4 台且在线", st == 200 and len(r["data"]) == 4 and all(d["online"] for d in r["data"]))
st, r = req("GET", "/api/admin/sales-rules")
check("售卖规则 3 类", st == 200 and len(r["data"]) == 3)
st, r = req("GET", "/api/holidays")
holidays = set(r.get("data", []))
check("2026 节假日已配置", st == 200 and "2026-10-01" in holidays)

# 选取 >= 今天的最近工作日（周一至周五且非节假日），作为全部正常流的入园日期
_d = datetime.datetime.now().astimezone().date()
while _d.weekday() >= 5 or _d.strftime("%Y-%m-%d") in holidays:
    _d += datetime.timedelta(days=1)
WORKDAY = _d.strftime("%Y-%m-%d")

section("B. 购卡与售卖规则")
p1 = buy_pass("single")
check("购单人卡成功", p1["id"].startswith("P") and p1["remaining_quota"] == 100 and p1["qr_token"].startswith("QR"))
st, r = req("POST", "/api/passes", {
    "type": "single", "holder_name": "重复", "holder_id_card": p1["holder_id_card"],
    "id_card_expiry": "2032-01-01", "phone": "13911112222"})
check("一证一卡：同证件重复购卡被拒", st == 409, r)
# 停售后禁售
st, rules = req("GET", "/api/admin/sales-rules")
rule_single = [x for x in rules["data"] if x["pass_type"] == "single"][0]
rule_off = dict(rule_single, on_sale=False)
st, r = req("PUT", "/api/admin/sales-rules/single", rule_off, {"X-Operator": "ops-li"})
check("运营停售单人卡", st == 200 and r["data"]["on_sale"] is False)
st, r = req("POST", "/api/passes", {
    "type": "single", "holder_name": "买不到", "holder_id_card": new_idcard(),
    "id_card_expiry": "2032-01-01", "phone": "13933334444"})
check("停售后购卡被拒", st == 409, r)
st, r = req("PUT", "/api/admin/sales-rules/single", rule_single, {"X-Operator": "ops-li"})
check("恢复售卖", st == 200 and r["data"]["on_sale"] is True)

section("C. 核验放行 / 幂等 / 并发去重")
pc = buy_pass("single")
st, r = verify(pass_id=pc["id"], req_id="C-0001", ts=local_ts(10, 30))
check("正常放行", st == 200 and r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
v_id = r["data"]["verification_id"]
st, r2 = verify(pass_id=pc["id"], req_id="C-0001", ts=local_ts(10, 30))
check("同 request_id 重试返回首次结果不重复扣", r2["data"]["verification_id"] == v_id and remaining(pc["id"]) == 99, r2)
st, r3 = verify(pass_id=pc["id"], req_id="C-0002", ts=local_ts(10, 31))
check("抖动重刷(不同request_id)按重复入园处理", r3["data"]["decision"] == "ALLOW" and r3["data"]["reason"] == "DUPLICATE_ENTRY"
      and r3["data"]["charged_quota"] == 0 and remaining(pc["id"]) == 99, r3)

# 并发：20 个相同 request_id
pc2 = buy_pass("single")
def concurrent_same(_):
    return verify(pass_id=pc2["id"], req_id="C-CONC", ts=local_ts(10, 30))
with ThreadPoolExecutor(max_workers=20) as ex:
    results = list(ex.map(concurrent_same, range(20)))
charged = sum(x[1]["data"]["charged_quota"] for x in results if x[0] == 200)
check("20 并发同 request_id 仅扣 1 次", charged == 1 and remaining(pc2["id"]) == 99,
      f"charged_sum={charged} remaining={remaining(pc2['id'])}")

# 并发：20 个不同 request_id 同景点（模拟闸机重试风暴）
pc3 = buy_pass("single")
def concurrent_diff(i):
    return verify(pass_id=pc3["id"], req_id=f"C-CONC-{i}", ts=local_ts(10, 30))
with ThreadPoolExecutor(max_workers=20) as ex:
    results = list(ex.map(concurrent_diff, range(20)))
charged = sum(x[1]["data"]["charged_quota"] for x in results if x[0] == 200)
check("20 并发不同 request_id 同景点仅扣 1 次(去重窗口)", charged == 1 and remaining(pc3["id"]) == 99,
      f"charged_sum={charged} remaining={remaining(pc3['id'])}")

# 二维码核验
fam = None
st, r = req("GET", "/api/passes/P000001")
if st == 200:
    fam = r["data"]
st, r = verify(qr=fam["qr_token"], face=88.0, ts=local_ts(10, 30))
check("二维码核验放行", st == 200 and r["data"]["decision"] == "ALLOW", r)
# 刷证件（成员证件定位年卡）；清掉高频计数避免误判，换景点避免去重窗口
redis_cli("DEL", "hf:P000001", "loc:P000001")
st, r = verify(idcard="110101199203054321", face=80.0, attraction="attr-garden", ts=local_ts(10, 32))
check("刷成员证件核验放行", st == 200 and r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)

section("D. 核验失败原因覆盖")
st, r = verify(pass_id=pc["id"], device="dev-not-exist", ts=local_ts(10, 30))
check("未注册设备 DEVICE_UNKNOWN", r["data"]["reason"] == "DEVICE_UNKNOWN", r)

# 设备离线：注册新设备并把心跳改到 5 分钟前
st, r = req("POST", "/api/devices", {"id": "dev-offline-01", "name": "离线闸机", "gate_id": "gate-east", "attraction": "attr-lake"})
dev = redis_get_json("device:dev-offline-01")
dev["last_heartbeat"] = (datetime.datetime.now() - datetime.timedelta(minutes=5)).astimezone().isoformat()
redis_set_json("device:dev-offline-01", dev)
st, r = verify(pass_id=pc["id"], device="dev-offline-01", ts=local_ts(10, 30))
check("设备离线 DEVICE_OFFLINE", r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "DEVICE_OFFLINE", r)
# 离线补传用全新卡，避免去重窗口/高频计数干扰
pc_off = buy_pass("single")
st, r = verify(pass_id=pc_off["id"], device="dev-offline-01", ts=local_ts(10, 30), offline_sync=True, req_id="C-OFFLINE-SYNC")
check("离线补传(offline_sync)正常放行", r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
st, r = verify(pass_id=pc_off["id"], device="dev-offline-01", ts=local_ts(10, 30), offline_sync=True, req_id="C-OFFLINE-SYNC")
check("离线补传重复提交不重复扣次", remaining(pc_off["id"]) == 99, r)

st, r = verify(pass_id="P999999", ts=local_ts(10, 30))
check("年卡不存在 PASS_NOT_FOUND", r["data"]["reason"] == "PASS_NOT_FOUND", r)

pfrozen = buy_pass("single")
req("POST", f"/api/passes/{pfrozen['id']}/freeze", {"reason": "失主挂失", "operator": "cs-wang"})
st, r = verify(pass_id=pfrozen["id"], ts=local_ts(10, 30))
check("冻结卡 PASS_FROZEN", r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "PASS_FROZEN", r)

pexp = buy_pass("single")
pj = redis_get_json(f"pass:{pexp['id']}")
pj["valid_until"] = "2020-01-01"
redis_set_json(f"pass:{pexp['id']}", pj)
st, r = verify(pass_id=pexp["id"], ts=local_ts(10, 30))
check("年卡过期 PASS_EXPIRED", r["data"]["decision"] == "QUOTA_INSUFFICIENT" and r["data"]["reason"] == "PASS_EXPIRED", r)

st, r = req("GET", "/api/passes/P000003")  # 亲子卡不含 attr-summit
pcard = r["data"]
st, r = verify(pass_id=pcard["id"], attraction="attr-summit", ts=local_ts(10, 30))
check("景点不适用 ATTRACTION_NOT_ALLOWED", r["data"]["reason"] == "ATTRACTION_NOT_ALLOWED", r)

psingle = buy_pass("single")  # 单人卡节假日不可用
st, r = verify(pass_id=psingle["id"], ts=local_ts(10, 30, date="2026-10-01"))
check("节假日限制 HOLIDAY_BLOCKED", r["data"]["reason"] == "HOLIDAY_BLOCKED", r)
st, r = verify(pass_id=psingle["id"], ts=local_ts(23, 30))
check("时段限制 TIMESLOT_BLOCKED", r["data"]["reason"] == "TIMESLOT_BLOCKED", r)

st, r = verify(pass_id=fam["id"], companions=["ID_NOT_BOUND_1"], ts=local_ts(10, 33))
check("同行人未绑定 COMPANION_NOT_BOUND", r["data"]["reason"] == "COMPANION_NOT_BOUND", r)
st, r = verify(pass_id=pcard["id"], companions=["440101201909096666", "ID_X"], ts=local_ts(10, 30))
check("同行超限 COMPANION_EXCEEDED", r["data"]["reason"] == "COMPANION_EXCEEDED", r)

pphoto = buy_pass("single")
st, r = verify(pass_id=pphoto["id"], photo_clear=False, ts=local_ts(10, 30))
check("照片不清 PHOTO_UNCLEAR", r["data"]["reason"] == "PHOTO_UNCLEAR", r)
st, r = verify(pass_id=pphoto["id"], face=30.0, ts=local_ts(10, 30))
check("人脸低分 FACE_MISMATCH", r["data"]["reason"] == "FACE_MISMATCH", r)

pold = buy_pass("single", expiry="2020-06-01")
st, r = verify(pass_id=pold["id"], ts=local_ts(10, 30))
check("证件过期 ID_EXPIRED", r["data"]["reason"] == "ID_EXPIRED", r)

phf = buy_pass("single")
redis_cli("SET", f"hf:{phf['id']}", "4")
st, r = verify(pass_id=phf["id"], ts=local_ts(10, 30))
check("高频异常 SUSPECTED_LENDING/HIGH_FREQUENCY",
      r["data"]["decision"] == "SUSPECTED_LENDING" and r["data"]["reason"] == "HIGH_FREQUENCY", r)

pml = buy_pass("family")
st, r = verify(pass_id=pml["id"], attraction="attr-lake", ts=local_ts(10, 30))
assert r["data"]["decision"] == "ALLOW", r
st, r = verify(pass_id=pml["id"], attraction="attr-garden", ts=local_ts(10, 35))
check("多地异常 SUSPECTED_LENDING/MULTI_LOCATION",
      r["data"]["decision"] == "SUSPECTED_LENDING" and r["data"]["reason"] == "MULTI_LOCATION", r)

pcap = buy_pass("family")
redis_cli("SET", f"cap:attr-garden:{WORKDAY}", "3000")  # 百花园容量 3000（核验日为 WORKDAY）
st, r = verify(pass_id=pcap["id"], attraction="attr-garden", ts=local_ts(10, 30))
check("容量超限 CAPACITY_FULL", r["data"]["reason"] == "CAPACITY_FULL", r)
redis_cli("DEL", f"cap:attr-garden:{WORKDAY}")

pquota = buy_pass("single")
redis_cli("SET", f"pass:{pquota['id']}:remaining", "0")
st, r = verify(pass_id=pquota["id"], ts=local_ts(10, 30))
check("权益耗尽 QUOTA_EXHAUSTED", r["data"]["decision"] == "QUOTA_INSUFFICIENT" and r["data"]["reason"] == "QUOTA_EXHAUSTED", r)

section("E. 人工放行与客服可解释")
pm = buy_pass("single")
st, r = verify(pass_id=pm["id"], photo_clear=False, ts=local_ts(10, 30))
vid = r["data"]["verification_id"]
check("制造人工核验(PHOTO_UNCLEAR)", r["data"]["decision"] == "MANUAL_REVIEW", r)
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "现场比对身份证照片一致，人工放行"})
check("人工放行成功并扣减额度", st == 200 and r["data"]["charged_quota"] == 1 and remaining(pm["id"]) == 99, r)
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "重复处理"})
check("重复人工处理被拒", st == 409, r)
st, r = req("GET", f"/api/verifications/{vid}")
check("客服可查完整链路(原因+人工处理记录)",
      r["data"]["manual_action"] == "RELEASED" and r["manual_review"]["operator"] == "staff-chen"
      and r["manual_review"]["note"].startswith("现场比对"), r)
st, r = req("GET", f"/api/passes/{pm['id']}/gate-logs")
check("闸机日志含 MANUAL_RELEASE", any(g["action"] == "MANUAL_RELEASE" for g in r["data"]), r)

section("F. 冻结/解冻与预约一致性")
pr = buy_pass("single")
st, r = req("POST", "/api/reservations", {"pass_id": pr["id"], "attraction": "attr-lake",
                                          "date": WORKDAY, "slot": "10:00-12:00"})
rsv_id = r["data"]["id"]
check("预约成功", st == 200 and r["data"]["status"] == "BOOKED", r)
req("POST", f"/api/passes/{pr['id']}/freeze", {"reason": "涉嫌转借核查", "operator": "cs-wang"})
st, r = req("GET", f"/api/passes/{pr['id']}/reservations")
check("冻结不影响已预约未入园订单", r["data"][0]["status"] == "BOOKED", r)
st, r = req("POST", "/api/reservations", {"pass_id": pr["id"], "attraction": "attr-lake",
                                          "date": WORKDAY, "slot": "14:00-16:00"})
check("冻结期间不可新预约", st == 409, r)
req("POST", f"/api/passes/{pr['id']}/unfreeze", {"operator": "cs-wang"})
st, r = verify(pass_id=pr["id"], ts=local_ts(10, 30))
check("解冻后正常放行", r["data"]["decision"] == "ALLOW", r)
st, r = req("GET", f"/api/passes/{pr['id']}/reservations")
check("入园后预约核销为 USED", r["data"][0]["status"] == "USED", r)

section("G. 申诉与权益返还")
pa = buy_pass("single")
st, r = verify(pass_id=pa["id"], req_id="G-0001", ts=local_ts(10, 30))
vid = r["data"]["verification_id"]
check("扣次成功(剩99)", remaining(pa["id"]) == 99)
st, r = req("POST", "/api/appeals", {"pass_id": pa["id"], "verification_id": vid,
                                     "type": "WRONG_DEDUCTION", "description": "闸机离线重复扣次，申请返还"})
aid = r["data"]["id"]
check("申诉创建", st == 200 and r["data"]["status"] == "PENDING", r)
st, r = req("POST", f"/api/appeals/{aid}/resolve",
            {"approve": True, "resolution": "核实为闸机抖动误扣，返还 1 人次", "handler": "cs-wang"})
check("申诉通过自动返还额度", st == 200 and r["data"]["compensate_quota"] == 1 and remaining(pa["id"]) == 100, r)
st, r = req("POST", f"/api/appeals/{aid}/resolve",
            {"approve": True, "resolution": "重复处理", "handler": "cs-wang"})
check("申诉重复处理被拒", st == 409, r)

section("H. 退款与升级重算")
pref = buy_pass("single")
req("POST", "/api/reservations", {"pass_id": pref["id"], "attraction": "attr-lake",
                                  "date": WORKDAY, "slot": "10:00-12:00"})
st, r = req("POST", f"/api/passes/{pref['id']}/refund", {"operator": "cs-wang"})
check("退款：状态+退款金额", st == 200 and r["data"]["status"] == "refunded" and r["data"]["refund_amount"] > 0, r)
check("退款后权益清零", remaining(pref["id"]) == 0)
st, r = req("GET", f"/api/passes/{pref['id']}/reservations")
check("退款后未使用预约取消", r["data"][0]["status"] == "CANCELLED", r)
st, r = verify(pass_id=pref["id"], ts=local_ts(10, 30))
check("退款卡核验 PASS_REFUNDED", r["data"]["reason"] == "PASS_REFUNDED", r)

pup = buy_pass("single")
# 先把有效期改短（模拟已用 60 天、只剩 100 天的旧卡），升级后应重算为完整新周期
pj = redis_get_json(f"pass:{pup['id']}")
d60 = (datetime.datetime.now() - datetime.timedelta(days=60)).strftime("%Y-%m-%d")
d100 = (datetime.datetime.now() + datetime.timedelta(days=100)).strftime("%Y-%m-%d")
pj["valid_from"] = d60
pj["valid_until"] = d100
redis_set_json(f"pass:{pup['id']}", pj)
expect_until = (datetime.datetime.now() + datetime.timedelta(days=365)).strftime("%Y-%m-%d")
st, r = req("POST", f"/api/passes/{pup['id']}/upgrade", {"to_type": "family", "operator": "cs-wang"})
check("升级：类型变更+补差价", st == 200 and r["data"]["type"] == "family" and r["price_diff"] >= 0, r)
check("升级：有效期重算为新卡完整周期", r["data"]["valid_from"] == local_date() and r["data"]["valid_until"] == expect_until, r["data"])
check("升级：额度按新卡重置", r["data"]["remaining_quota"] == 400, r["data"])
st, r2 = verify(pass_id=pup["id"], attraction="attr-summit", ts=local_ts(10, 30))
check("升级后可进家庭卡景点", r2["data"]["decision"] == "ALLOW", r2)

section("I. 信息变更")
ph = buy_pass("single", phone="13700001111")
st, r = req("POST", f"/api/passes/{ph['id']}/phone",
            {"old_phone": "13700001111", "new_phone": "13700009999", "code": "000000"})
check("换手机号：错误验证码被拒", st == 403, r)
st, r = req("POST", f"/api/passes/{ph['id']}/phone",
            {"old_phone": "13700001111", "new_phone": "13700009999", "code": "123456"})
check("换手机号成功", st == 200 and r["data"]["phone"] == "13700009999", r)
st, r = req("GET", "/api/passes?phone=13700009999")
check("新手机号可查到卡", any(x["id"] == ph["id"] for x in r["data"]), r)
st, r = req("GET", "/api/passes?phone=13700001111")
check("旧手机号查不到卡", len(r["data"]) == 0, r)

pmem = buy_pass("parent_child", members=[{"name": "孩子", "id_card": new_idcard(), "relation": "child"}])
st, r = req("POST", f"/api/passes/{pmem['id']}/members",
            {"name": "外婆", "id_card": new_idcard(), "relation": "parent", "operator": "staff-chen"})
check("添加成员成功(消耗更换次数)", st == 200, r)
st, r = req("POST", f"/api/passes/{pmem['id']}/members",
            {"name": "外公", "id_card": new_idcard(), "relation": "parent", "operator": "staff-chen"})
check("更换次数用尽被拒", st == 409, r)
st, r = req("GET", f"/api/passes/{pmem['id']}")
mid = [m for m in r["data"]["members"] if m["name"] == "外婆"][0]["id"]
st, r = req("DELETE", f"/api/passes/{pmem['id']}/members/{mid}")
check("移除成员", st == 200, r)
st, r = req("GET", f"/api/passes/{pmem['id']}")
child = [m for m in r["data"]["members"] if m["role"] == "companion"][0]
st, r = req("POST", f"/api/passes/{pmem['id']}/members/{child['id']}/photo",
            {"photo_url": "https://img.example.com/new-photo.jpg", "operator": "staff-chen"})
check("孩子长大更新照片(版本+1)", st == 200 and r["data"]["photo_version"] == 2, r)

pq = buy_pass("single")
old_qr = pq["qr_token"]
st, r = req("POST", f"/api/passes/{pq['id']}/qr-refresh", {"operator": "cs-wang"})
new_qr = r["data"]["qr_token"]
st, r = verify(qr=old_qr, ts=local_ts(10, 30))
check("旧二维码失效", r["data"]["reason"] == "PASS_NOT_FOUND", r)
st, r = verify(qr=new_qr, ts=local_ts(10, 30))
check("新二维码放行", r["data"]["decision"] == "ALLOW", r)

section("J. 老人免刷脸（人工确认放行）")
# 种子家庭卡成员 张父(P000001-M3) 为免刷脸；换 attr-summit 避开去重窗口
st, r = verify(idcard="110101195501011239", face=-1.0, attraction="attr-summit", ts=local_ts(10, 40))
check("免刷脸成员转人工 FACE_EXEMPT_REVIEW",
      r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "FACE_EXEMPT_REVIEW", r)
vid = r["data"]["verification_id"]
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "老人证件人证一致，人工放行"})
check("人工确认放行老人", st == 200 and r["data"]["charged_quota"] == 1, r)
# 工作人员设置/取消免刷脸
st, r = req("GET", "/api/passes/P000001")
zhangfu = [m for m in r["data"]["members"] if m["name"] == "张父"][0]
st, r = req("POST", f"/api/passes/P000001/members/{zhangfu['id']}/face-exempt",
            {"exempt": False, "operator": "staff-chen"})
check("取消免刷脸标记", st == 200 and r["data"]["face_exempt"] is False, r)
req("POST", f"/api/passes/P000001/members/{zhangfu['id']}/face-exempt",
    {"exempt": True, "operator": "staff-chen"})

section("K. 出园与容量")
# 家庭卡节假日可用，入园/出园都按真实当天（出园接口按服务端日期释放容量），保证入/出园同日
pk = buy_pass("family")
st, r = verify(pass_id=pk["id"], attraction="attr-lake", ts=local_ts(11, 0, local_date()))
assert r["data"]["decision"] == "ALLOW", r
st, r = req("GET", "/api/attractions/attr-lake/capacity")
cap_after_enter = r["data"]["in_park"]
st, r = req("POST", "/api/exit", {"pass_id": pk["id"], "attraction": "attr-lake", "count": 1, "device_id": "dev-east-01"})
st, r = req("GET", "/api/attractions/attr-lake/capacity")
check("出园释放在园容量", r["data"]["in_park"] == cap_after_enter - 1, r)

section("L. 运营统计与日志")
st, r = req("GET", "/api/admin/stats/overview?date="+WORKDAY)
d = r["data"]
check("总览：按卡类型统计", st == 200 and len(d["by_type"]) == 3 and d["entries_total"] > 0, d)
check("总览：异常原因分布", "PHOTO_UNCLEAR" in d["anomaly_by_reason"] or len(d["anomaly_by_reason"]) > 0, d)
check("总览：人工放行计数", d["manual_releases"] >= 2, d)
st, r = req("GET", "/api/admin/stats/peaks?date="+WORKDAY)
check("峰值：24 小时分布", st == 200 and len(r["data"]["by_hour"]) == 24, r)
st, r = req("GET", "/api/admin/gate-logs?limit=50")
check("闸机日志查询", st == 200 and len(r["data"]) > 0, r)
st, r = req("GET", f"/api/passes/{pm['id']}/changes")
check("变更日志留痕", st == 200 and any(c["action"] == "CREATE" for c in r["data"]), r)
st, r = req("GET", "/api/appeals?status=APPROVED")
check("申诉列表查询", st == 200 and len(r["data"]) >= 1, r)

# ============================================================
section("M. 闸机离线日志合并 / 重复扣次回补 / 现场复核")
# 场景：东门断网，闸机离线放行年卡用户；恢复后上传离线日志批次
req("POST", "/api/devices/dev-east-01/heartbeat")
po = buy_pass("family")
st, r = verify(pass_id=po["id"], req_id="M-ONLINE-1", attraction="attr-lake", ts=local_ts(10, 30))
assert r["data"]["decision"] == "ALLOW", r
online_vid = r["data"]["verification_id"]
check("断网前在线入园扣 1（剩 399）", remaining(po["id"]) == 399, r)
st, r = req("GET", f"/api/verifications/{online_vid}")
check("在线核验记录主入园人证件", r["data"]["principal_id_card"] == po["holder_id_card"], r["data"])

unbound_card = new_idcard().replace("ID", "UB")
offline_batch = {
    "device_id": "dev-east-01",
    "client_token": "M-CLIENT-TOKEN-1",
    "operator": "gate-sync",
    "offline_from": local_ts(10, 31),
    "offline_to": local_ts(11, 30),
    "entries": [
        # E1：同一游客 11:00 在同景点被离线闸机重复放行 → REVERSED 回补
        {"local_seq": "M-E1", "request_id": "M-OFF-E1", "pass_id": po["id"], "attraction": "attr-lake",
         "released_at": local_ts(11, 0), "headcount": 1},
        # E2：同行人未绑定 → REVIEW
        {"local_seq": "M-E2", "request_id": "M-OFF-E2", "pass_id": po["id"], "attraction": "attr-lake",
         "released_at": local_ts(11, 10), "companions": [unbound_card], "headcount": 2},
        # E3：无法定位年卡 → REVIEW
        {"local_seq": "M-E3", "id_card": "999999199001010001", "name": "无法识别游客",
         "attraction": "attr-lake", "released_at": local_ts(11, 20), "headcount": 1},
        # E4：request_id 与断网前在线核验相同 → DUPLICATE_REQ
        {"local_seq": "M-E4", "request_id": "M-ONLINE-1", "pass_id": po["id"], "attraction": "attr-lake",
         "released_at": local_ts(11, 30), "headcount": 1},
    ],
}
st, r = req("POST", "/api/offline/batches", offline_batch)
check("上传离线批次 200", st == 200, r)
b = r["data"]
check("批次 CONFLICT（含待复核）", b["status"] == "CONFLICT", b)
check("批次计数 total=4 merged=0 dup=1 reversed=1 review=2",
      (b["total"], b["merged_count"], b["duplicate_count"], b["reversed_count"], b["review_count"]) == (4, 0, 1, 1, 2), b)
check("回补人次合计=1", b["refund_quota_sum"] == 1, b)
emap = {e["local_seq"]: e for e in b["entries"]}
check("E1 REVERSED 并带回补单", emap["M-E1"]["status"] == "REVERSED" and emap["M-E1"]["refund_id"], emap["M-E1"])
check("E2 未绑定同行人 REVIEW", emap["M-E2"]["status"] == "REVIEW" and emap["M-E2"]["review_id"], emap["M-E2"])
check("E3 无法定位年卡 REVIEW", emap["M-E3"]["status"] == "REVIEW" and emap["M-E3"]["review_id"], emap["M-E3"])
check("E4 同 request_id DUPLICATE_REQ", emap["M-E4"]["status"] == "DUPLICATE_REQ", emap["M-E4"])
check("重复扣次已自动回补（仍剩 399）", remaining(po["id"]) == 399, f"rem={remaining(po['id'])}")

# 回补单留痕
st, r = req("GET", f"/api/offline/refunds?pass_id={po['id']}")
rf = r["data"][0]
check("回补单：额度 1 / 设备 / 首次入园关联",
      rf["quota"] == 1 and rf["device_id"] == "dev-east-01" and rf["duplicate_of"] == online_vid
      and rf["verification_id"] == emap["M-E1"]["verification_id"] and rf["status"] == "DONE", rf)
st, r = req("GET", f"/api/offline/refunds/{rf['id']}")
check("回补单可按 ID 查询", st == 200 and r["data"]["id"] == rf["id"], r)
# 被回补的离线核验记录留痕
st, r = req("GET", f"/api/verifications/{emap['M-E1']['verification_id']}")
check("重复离线核验标记 reversed_by + DUPLICATE_ENTRY",
      r["data"]["reversed_by"] == rf["id"] and r["data"]["reason"] == "DUPLICATE_ENTRY"
      and r["data"]["offline_batch_id"] == b["id"] and r["data"]["offline_local_seq"] == "M-E1", r["data"])
# 通知游客
st, r = req("GET", f"/api/notifications?pass_id={po['id']}&type=QUOTA_REFUND")
check("回补短信通知已发送", len(r["data"]) == 1 and r["data"][0]["status"] == "SENT"
      and r["data"][0]["channel"] == "sms" and po["id"] in r["data"][0]["content"], r)
st, r = req("GET", f"/api/passes/{po['id']}/changes")
check("年卡变更日志含 OFFLINE_REFUND", any(c["action"] == "OFFLINE_REFUND" for c in r["data"]), r)
st, r = req("GET", f"/api/passes/{po['id']}/gate-logs")
check("闸机流水含离线放行 OFFLINE_ALLOW / OFFLINE_REVIEW",
      any(g["action"] == "OFFLINE_ALLOW" for g in r["data"]) and any(g["action"] == "OFFLINE_REVIEW" for g in r["data"]), r)

# 批次详情与列表
st, r = req("GET", f"/api/offline/batches/{b['id']}")
check("批次详情含 4 条明细", st == 200 and len(r["data"]["entries"]) == 4, r)
st, r = req("GET", "/api/offline/batches?device_id=dev-east-01")
check("按设备查批次列表", st == 200 and any(x["id"] == b["id"] for x in r["data"]), r)
# 整包重传（闸机重试同一批）→ 返回同一批次，不重复合并
st, r = req("POST", "/api/offline/batches", offline_batch)
check("整包重传幂等（同一批次，计数不变）",
      r["data"]["id"] == b["id"] and r["data"]["total"] == 4 and r["data"]["reversed_count"] == 1, r)

# 现场复核：E2 补登记同行人并补扣
rv_e2 = emap["M-E2"]["review_id"]
st, r = req("GET", "/api/offline/reviews?status=PENDING")
pending_ids = [x["id"] for x in r["data"]]
check("待复核任务 2 条", rv_e2 in pending_ids and emap["M-E3"]["review_id"] in pending_ids, r)
st, r = req("POST", f"/api/offline/reviews/{rv_e2}/resolve", {
    "action": "REGISTER", "operator": "staff-chen", "note": "现场核实为家属，补登记并补扣",
    "registrations": [{"name": "离线补登记家属", "id_card": unbound_card, "relation": "other"}]})
check("E2 补登记：按放行人数补扣 2（剩 397）",
      st == 200 and r["data"]["status"] == "RESOLVED" and r["data"]["charged_quota"] == 2
      and remaining(po["id"]) == 397, r)
st, r = req("GET", f"/api/passes/{po['id']}")
check("补登记的同行人已入成员列表", any(m["id_card"] == unbound_card for m in r["data"]["members"]), r)
# 现场复核：E3 标记异常放行，不补扣
st, r = req("POST", f"/api/offline/reviews/{emap['M-E3']['review_id']}/resolve",
            {"action": "ABNORMAL", "operator": "staff-chen", "note": "无法核实身份，标记异常放行留痕"})
check("E3 标记异常放行（不补扣，仍剩 397）",
      st == 200 and r["data"]["action"] == "ABNORMAL" and remaining(po["id"]) == 397, r)
st, r = req("POST", f"/api/offline/reviews/{rv_e2}/resolve",
            {"action": "ABNORMAL", "operator": "staff-chen", "note": "重复处置"})
check("复核任务不可重复处理", st == 409, r)
st, r = req("GET", "/api/offline/reviews?status=PENDING")
check("待复核清零", len(r["data"]) == 0, r)

# 正常离线放行（无重复）：MERGED 扣次；容量净额只 +1（重复那条回补）
pm = buy_pass("family")
st, cap_before = req("GET", "/api/attractions/attr-summit/capacity?date="+WORKDAY)
cap_before = cap_before["data"]["in_park"]
st, r = req("POST", "/api/offline/batches", {
    "device_id": "dev-west-01", "client_token": "M-CLIENT-TOKEN-2",
    "entries": [
        {"local_seq": "M-W1", "pass_id": pm["id"], "attraction": "attr-summit",
         "released_at": local_ts(11, 5), "headcount": 1},
        {"local_seq": "M-W2", "pass_id": pm["id"], "attraction": "attr-summit",
         "released_at": local_ts(11, 15), "headcount": 1},
    ]})
check("正常+重复批次：MERGED=1 REVERSED=1 状态 MERGED",
      r["data"]["merged_count"] == 1 and r["data"]["reversed_count"] == 1 and r["data"]["status"] == "MERGED", r)
check("无重复时正常入账扣 1（剩 399）", remaining(pm["id"]) == 399, f"rem={remaining(pm['id'])}")
st, cap_after = req("GET", "/api/attractions/attr-summit/capacity?date="+WORKDAY)
check("容量净额只增 1（重复放行已回补容量）", cap_after["data"]["in_park"] == cap_before + 1,
      f"{cap_before} -> {cap_after['data']['in_park']}")

# 复核 CHARGE：确认入园但补扣权益
pc2 = buy_pass("family")
charge_card = new_idcard().replace("ID", "UC")
st, r = req("POST", "/api/offline/batches", {
    "device_id": "dev-south-01", "client_token": "M-CLIENT-TOKEN-3",
    "entries": [
        {"local_seq": "M-S1", "pass_id": pc2["id"], "attraction": "attr-garden",
         "released_at": local_ts(11, 8), "companions": [charge_card], "headcount": 2},
    ]})
rv_charge = r["data"]["entries"][0]["review_id"]
check("未绑定同行人推复核", r["data"]["entries"][0]["status"] == "REVIEW", r)
st, r = req("POST", f"/api/offline/reviews/{rv_charge}/resolve",
            {"action": "CHARGE", "operator": "staff-chen", "note": "确认实际入园 2 人，补扣权益", "pass_id": pc2["id"]})
check("CHARGE 补扣 2 人次（剩 398）",
      st == 200 and r["data"]["charged_quota"] == 2 and remaining(pc2["id"]) == 398, r)

# 边界：权益耗尽后重复离线放行——识别为重复但实扣 0，不超额回补、额度不为负
pe = buy_pass("single")
st, r = verify(pass_id=pe["id"], req_id="M-EDGE-ON", attraction="attr-lake", ts=local_ts(10, 30))
assert r["data"]["decision"] == "ALLOW", r
redis_cli("SET", f"pass:{pe['id']}:remaining", "0")
st, r = req("POST", "/api/offline/batches", {
    "device_id": "dev-east-01", "client_token": "M-CLIENT-TOKEN-4",
    "entries": [
        {"local_seq": "M-E0", "pass_id": pe["id"], "attraction": "attr-lake",
         "released_at": local_ts(11, 2), "headcount": 1},
    ]})
ee = r["data"]["entries"][0]
check("权益耗尽时重复放行识别 REVERSED 但不回补", ee["status"] == "REVERSED" and not ee.get("refund_id"), ee)
check("额度不出现负数/超额回补", remaining(pe["id"]) == 0, f"rem={remaining(pe['id'])}")

# 设备离线事件与可靠性统计
st, r = req("GET", "/api/admin/offline-incidents?device_id=dev-east-01")
incs = r["data"]
inc_b = next((x for x in incs if x["batch_id"] == b["id"]), None)
check("东门离线事件已关闭并关联批次",
      inc_b is not None and inc_b["status"] == "CLOSED" and inc_b["offline_logs"] == 4
      and inc_b["duration_sec"] >= 0, r)
st, r = req("GET", "/api/admin/device-reliability")
rels = {d["device_id"]: d for d in r["data"]}
check("可靠性：东门离线次数/回补人次沉淀",
      rels["dev-east-01"]["offline_count"] >= 1 and rels["dev-east-01"]["refund_quota"] >= 1
      and len(rels["dev-east-01"]["recent_incidents"]) >= 1, r)
check("可靠性：D 段离线闸机有未关闭事件",
      rels["dev-offline-01"]["offline_count"] >= 1 and rels["dev-offline-01"]["open_incidents"] >= 1, r)
counts = [d["offline_count"] for d in r["data"]]
check("可靠性列表按离线次数倒序（最易离线在前）", counts == sorted(counts, reverse=True), counts)

print(f"\n========================================")
print(f"RESULT: PASS={PASS} FAIL={FAIL}")
print(f"========================================")
sys.exit(1 if FAIL > 0 else 0)
