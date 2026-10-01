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
    # 场景固定在国庆假期后的营业日 2026-10-09（单人卡节假日不可用，避免用例撞节假日）
    return "2026-10-09"


def real_date():
    return datetime.datetime.now().astimezone().strftime("%Y-%m-%d")


def local_ts(hour=10, minute=30, date=None):
    now = datetime.datetime.now().astimezone()
    tz = now.strftime("%z")
    tz = tz[:3] + ":" + tz[3:]
    d = date or local_date()
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
check("2026 节假日已配置", st == 200 and "2026-10-01" in r["data"])

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
today = local_date()
redis_cli("SET", f"cap:attr-garden:{today}", "3000")  # 百花园容量 3000
st, r = verify(pass_id=pcap["id"], attraction="attr-garden", ts=local_ts(10, 30))
check("容量超限 CAPACITY_FULL", r["data"]["reason"] == "CAPACITY_FULL", r)
redis_cli("DEL", f"cap:attr-garden:{today}")

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
                                          "date": local_date(), "slot": "10:00-12:00"})
rsv_id = r["data"]["id"]
check("预约成功", st == 200 and r["data"]["status"] == "BOOKED", r)
req("POST", f"/api/passes/{pr['id']}/freeze", {"reason": "涉嫌转借核查", "operator": "cs-wang"})
st, r = req("GET", f"/api/passes/{pr['id']}/reservations")
check("冻结不影响已预约未入园订单", r["data"][0]["status"] == "BOOKED", r)
st, r = req("POST", "/api/reservations", {"pass_id": pr["id"], "attraction": "attr-lake",
                                          "date": local_date(), "slot": "14:00-16:00"})
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
                                  "date": local_date(), "slot": "10:00-12:00"})
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
check("升级：有效期重算为新卡完整周期", r["data"]["valid_from"] == real_date() and r["data"]["valid_until"] == expect_until, r["data"])
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
pk = buy_pass("family")
st, r = verify(pass_id=pk["id"], attraction="attr-lake", ts=local_ts(11, 0))
assert r["data"]["decision"] == "ALLOW", r
st, r = req("GET", f"/api/attractions/attr-lake/capacity?date={local_date()}")
cap_after_enter = r["data"]["in_park"]
st, r = req("POST", "/api/exit", {"pass_id": pk["id"], "attraction": "attr-lake", "count": 1,
                                  "device_id": "dev-east-01", "date": local_date()})
st, r = req("GET", f"/api/attractions/attr-lake/capacity?date={local_date()}")
check("出园释放在园容量", r["data"]["in_park"] == cap_after_enter - 1, r)

section("L. 运营统计与日志")
st, r = req("GET", f"/api/admin/stats/overview?date={local_date()}")
d = r["data"]
check("总览：按卡类型统计", st == 200 and len(d["by_type"]) == 3 and d["entries_total"] > 0, d)
check("总览：异常原因分布", "PHOTO_UNCLEAR" in d["anomaly_by_reason"] or len(d["anomaly_by_reason"]) > 0, d)
check("总览：人工放行计数", d["manual_releases"] >= 2, d)
st, r = req("GET", f"/api/admin/stats/peaks?date={local_date()}")
check("峰值：24 小时分布", st == 200 and len(r["data"]["by_hour"]) == 24, r)
st, r = req("GET", "/api/admin/gate-logs?limit=50")
check("闸机日志查询", st == 200 and len(r["data"]) > 0, r)
st, r = req("GET", f"/api/passes/{pm['id']}/changes")
check("变更日志留痕", st == 200 and any(c["action"] == "CREATE" for c in r["data"]), r)
st, r = req("GET", "/api/appeals?status=APPROVED")
check("申诉列表查询", st == 200 and len(r["data"]) >= 1, r)

# ============================================================
section("M. 闸机离线日志合并：重复扣次回补 / 未绑定同行人复核 / 离线排行")

# 专用离线闸机（attr-lake）
st, r = req("POST", "/api/devices",
            {"id": "dev-offline-02", "name": "离线测试闸机", "gate_id": "gate-east", "attraction": "attr-lake"})
check("注册离线测试闸机", st == 200 and r["data"]["id"] == "dev-offline-02", r)


def offline_batch(device, events, frm="09:00", to="11:30", operator="staff-chen"):
    hf, mf = frm.split(":")
    ht, mt = to.split(":")
    body = {
        "device_id": device,
        "offline_from": local_ts(int(hf), int(mf)),
        "offline_to": local_ts(int(ht), int(mt)),
        "operator": operator,
        "events": events,
    }
    return req("POST", "/api/offline/batches", body)


def off_event(req_id, principal, companions=None, hour=10, minute=30, names=None, pass_id=None):
    e = {
        "request_id": req_id,
        "event_id": req_id,
        "principal_id_card": principal,
        "local_time": local_ts(hour, minute),
        "face_score": 92.0,
    }
    if pass_id:
        e["pass_id"] = pass_id
    if companions:
        e["companions"] = companions
    if names:
        e["companion_names"] = names
    return e


# --- M1：同一游客重复刷闸 → 识别重复扣次 → 自动回补 + 通知 ---
bound_spouse = new_idcard()
pm1 = buy_pass("family", members=[{"name": "配偶", "id_card": bound_spouse, "relation": "spouse"}])
check("家庭卡初始额度 400", remaining(pm1["id"]) == 400)
evs = [
    off_event("M-E1", pm1["holder_id_card"], [bound_spouse], 10, 30),
    off_event("M-E2", pm1["holder_id_card"], [bound_spouse], 10, 32),  # 重复入园（含同行）
    off_event("M-E3", pm1["holder_id_card"], None, 10, 35),            # 再次重复刷闸
]
st, r = offline_batch("dev-offline-02", evs)
b1 = r["data"]
check("离线批次合并成功", st == 200 and b1["status"] == "PROCESSED" and b1["event_count"] == 3
      and b1["allowed_count"] == 3, r)
g1 = [g for g in b1["groups"] if g["pass_id"] == pm1["id"]][0]
check("同游客同景点聚合为 1 组", len(b1["groups"]) == 1 and len(g1["event_ids"]) == 3, b1["groups"])
check("应扣 2 人次(主+配偶)，实扣 5 人次", g1["head_count"] == 2 and g1["charged_total"] == 5, g1)
check("重复扣次 3 人次自动回补(组)", g1["status"] == "REFUNDED" and g1["refund_quota"] == 3, g1)
check("批次回补合计 3", b1["refund_total"] == 3, b1)
check("游客额度=400-5+3=398", remaining(pm1["id"]) == 398, remaining(pm1["id"]))
st, r = req("GET", f"/api/notifications?pass_id={pm1['id']}")
notifs = r["data"]
n1 = [n for n in notifs if n["type"] == "OFFLINE_REFUND"]
check("回补短信通知已发送", st == 200 and len(n1) == 1 and "重复扣次 3 人次" in n1[0]["content"]
      and "已自动回补" in n1[0]["content"] and n1[0]["phone"] == pm1["phone"], n1)
sms_out = redis_cli("LRANGE", "sms:out", "0", "-1")
check("短信出站留库(收件手机号)", pm1["phone"] in sms_out)
st, r = req("GET", f"/api/passes/{pm1['id']}/changes")
check("回补变更日志留痕 OFFLINE_REFUND", any(c["action"] == "OFFLINE_REFUND" for c in r["data"]), r)
st, r = req("GET", f"/api/passes/{pm1['id']}/verifications")
dup_verifs = [v for v in r["data"] if v.get("reason") == "OFFLINE_DUP_REFUND"]
check("核验记录标记 OFFLINE_DUP_REFUND 且记录回补额度",
      len(dup_verifs) >= 1 and sum(v.get("refunded_quota", 0) for v in dup_verifs) == 3
      and all(v["offline_sync"] for v in dup_verifs), r["data"])
# 离线合并不写在线去重标记：恢复后在线核验可正常扣次
st, r = verify(pass_id=pm1["id"], req_id="M-ONLINE-1", ts=local_ts(12, 0))
check("恢复后在线核验正常放行扣次", r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1
      and remaining(pm1["id"]) == 397, r)

# --- M2：未绑定同行人 → 现场复核（补登记 / 扣权益 / 异常放行）---
ub1, ub2, ub3 = new_idcard(), new_idcard(), new_idcard()
pm2 = buy_pass("family")
pm3 = buy_pass("family")
pm4 = buy_pass("family")
# pm2：未绑定同行人 ub1（不计入合并扣次）
st, r = offline_batch("dev-offline-02", [
    off_event("M-U1", pm2["holder_id_card"], [ub1], 10, 40, names={ub1: "临时同行甲"})])
check("含未绑定同行人的批次 PROCESSED", st == 200 and r["data"]["status"] == "PROCESSED", r)
st, r = offline_batch("dev-offline-02", [off_event("M-U2", pm3["holder_id_card"], [ub2], 10, 41)])
st, r = offline_batch("dev-offline-02", [off_event("M-U3", pm4["holder_id_card"], [ub3], 10, 42)])

def find_review(pass_id, idcard):
    st, r = req("GET", f"/api/offline/reviews?status=PENDING")
    assert st == 200, r
    hit = [x for x in r["data"] if x["pass_id"] == pass_id and x["unbound_id_card"] == idcard]
    assert hit, f"review not found for {pass_id}/{idcard}: {r['data']}"
    return hit[0]


rv1 = find_review(pm2["id"], ub1)
check("复核单：未绑定同行人待处理", rv1["status"] == "PENDING" and rv1["unbound_name"] == "临时同行甲"
      and rv1["device_id"] == "dev-offline-02" and rv1["attraction"] == "attr-lake", rv1)
rem_before_register = remaining(pm2["id"])  # 合并已扣持卡人 1 人次 = 399
st, r = req("POST", f"/api/offline/reviews/{rv1['id']}/resolve",
            {"action": "REGISTER", "name": "补登记同行人", "relation": "other",
             "operator": "staff-chen", "note": "现场核实为持卡人亲友，补登记，不追扣"})
check("复核-补登记成功", st == 200 and r["data"]["status"] == "REGISTERED", r)
st, r = req("GET", f"/api/passes/{pm2['id']}")
check("补登记后成员已绑定", any(m["id_card"] == ub1 for m in r["data"]["members"]), r)
check("补登记不追扣权益", remaining(pm2["id"]) == rem_before_register)

rv2 = find_review(pm3["id"], ub2)
rem3 = remaining(pm3["id"])
st, r = req("POST", f"/api/offline/reviews/{rv2['id']}/resolve",
            {"action": "CHARGE", "operator": "staff-chen", "note": "确认该同行人入园，按 1 人次追扣"})
check("复核-扣权益成功", st == 200 and r["data"]["status"] == "CHARGED" and r["data"]["charged_quota"] == 1, r)
check("追扣后额度 -1", remaining(pm3["id"]) == rem3 - 1)

rv3 = find_review(pm4["id"], ub3)
rem4 = remaining(pm4["id"])
st, r = req("POST", f"/api/offline/reviews/{rv3['id']}/resolve",
            {"action": "ANOMALY", "operator": "staff-chen", "note": "无法说明同行关系，标记异常放行待查"})
check("复核-标记异常放行", st == 200 and r["data"]["status"] == "ANOMALY", r)
check("异常放行不扣额度", remaining(pm4["id"]) == rem4)
st, r = req("POST", f"/api/offline/reviews/{rv3['id']}/resolve",
            {"action": "ANOMALY", "operator": "staff-chen", "note": "重复处理"})
check("复核单重复处置被拒", st == 409, r)
st, r = req("GET", f"/api/notifications?pass_id={pm2['id']}")
check("复核结果通知游客", any(n["type"] == "REVIEW_RESOLVED" for n in r["data"]), r)
st, r = req("GET", f"/api/passes/{pm4['id']}/offline-reviews")
check("年卡视角复核单可查", st == 200 and any(x["id"] == rv3["id"] for x in r["data"]), r)

# --- M3：无法匹配年卡的离线放行 → PARTIAL + 异常待查 ---
st, r = offline_batch("dev-offline-02", [
    off_event("M-NOPASS", "ID_CARD_NOT_EXIST_999", None, 10, 50)])
check("无法对账的离线记录批次 PARTIAL", st == 200 and r["data"]["status"] == "PARTIAL", r)
gnp = [g for g in r["data"]["groups"] if g["status"] == "NO_PASS"]
check("分组标记 NO_PASS", len(gnp) == 1 and gnp[0]["principal_id"] == "ID_CARD_NOT_EXIST_999", r["data"])

# --- M4：批次重放幂等（全部 request_id 已合并 → 409，不重复扣次/统计）---
st, r = offline_batch("dev-offline-02", evs)
check("离线批次重复上报幂等拒绝", st == 409, r)
check("幂等重放不改变额度", remaining(pm1["id"]) == 397, remaining(pm1["id"]))

# --- M5：闸机离线排行（维护计划/高峰调度参考）---
st, r = req("GET", "/api/admin/devices/offline-ranking")
ranking = r["data"]
check("离线排行覆盖全部闸机", st == 200 and len(ranking) >= 6, r)
scores = [d["risk_score"] for d in ranking]
check("排行按风险分降序", all(scores[i] >= scores[i + 1] for i in range(len(scores) - 1)), scores)
top = ranking[0]
check("最易离线闸机=dev-offline-02 且指标完整",
      top["device_id"] == "dev-offline-02" and top["offline_episodes"] >= 5
      and top["merged_batches"] >= 5 and top["offline_entries"] >= 5
      and top["refunded_quota"] == 3 and top["anomaly_releases"] >= 1
      and top["last_offline_from"] != "" and top["risk_score"] > 0, top)
st, r = req("GET", "/api/offline/batches?limit=10")
check("离线批次列表可查", st == 200 and len(r["data"]) >= 5, r)
bid = r["data"][0]["id"]
st, r = req("GET", f"/api/offline/batches/{bid}")
check("离线批次详情可查", st == 200 and r["data"]["id"] == bid and len(r["data"]["groups"]) >= 1, r)

print(f"\n========================================")
print(f"RESULT: PASS={PASS} FAIL={FAIL}")
print(f"========================================")
sys.exit(1 if FAIL > 0 else 0)
