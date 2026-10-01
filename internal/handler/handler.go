package handler

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v2"
	"passgate/internal/model"
	"passgate/internal/service"
)

// Register 注册全部路由
func Register(app *fiber.App, svc *service.Service) {
	app.Get("/healthz", func(c *fiber.Ctx) error {
		if err := svc.St.Ping(context.Background()); err != nil {
			return c.Status(503).JSON(fiber.Map{"status": "redis_down"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	api := app.Group("/api")

	// ---- 年卡 ----
	api.Post("/passes", func(c *fiber.Ctx) error {
		var in service.CreatePassInput
		if err := c.BodyParser(&in); err != nil {
			return fail(ErrBadRequest)
		}
		view, err := svc.CreatePass(ctx(), in)
		return respond(c, view, err)
	})
	api.Get("/passes", func(c *fiber.Ctx) error {
		phone := c.Query("phone")
		if phone == "" {
			return fail(ErrBadRequest)
		}
		ids, err := svc.FindPassesByPhone(ctx(), phone)
		if err != nil {
			return respond(c, nil, err)
		}
		views := make([]*model.PassView, 0, len(ids))
		for _, id := range ids {
			if v, err := svc.GetPassView(ctx(), id); err == nil {
				views = append(views, v)
			}
		}
		return c.JSON(fiber.Map{"data": views})
	})
	api.Get("/passes/:id", func(c *fiber.Ctx) error {
		view, err := svc.GetPassView(ctx(), c.Params("id"))
		return respond(c, view, err)
	})
	api.Post("/passes/:id/freeze", func(c *fiber.Ctx) error {
		var body struct {
			Reason   string `json:"reason"`
			Operator string `json:"operator"`
		}
		_ = c.BodyParser(&body)
		if body.Reason == "" {
			return fail(ErrBadRequest)
		}
		p, err := svc.Freeze(ctx(), c.Params("id"), body.Reason, operator(c, body.Operator))
		return respond(c, p, err)
	})
	api.Post("/passes/:id/unfreeze", func(c *fiber.Ctx) error {
		var body struct {
			Operator string `json:"operator"`
		}
		_ = c.BodyParser(&body)
		p, err := svc.Unfreeze(ctx(), c.Params("id"), operator(c, body.Operator))
		return respond(c, p, err)
	})
	api.Post("/passes/:id/refund", func(c *fiber.Ctx) error {
		var body struct {
			Operator string `json:"operator"`
		}
		_ = c.BodyParser(&body)
		p, err := svc.Refund(ctx(), c.Params("id"), operator(c, body.Operator))
		return respond(c, p, err)
	})
	api.Post("/passes/:id/upgrade", func(c *fiber.Ctx) error {
		var body struct {
			ToType   model.PassType `json:"to_type"`
			Operator string         `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil || body.ToType == "" {
			return fail(ErrBadRequest)
		}
		view, diff, err := svc.Upgrade(ctx(), c.Params("id"), body.ToType, operator(c, body.Operator))
		if err != nil {
			return respond(c, nil, err)
		}
		return c.JSON(fiber.Map{"data": view, "price_diff": diff})
	})
	api.Post("/passes/:id/phone", func(c *fiber.Ctx) error {
		var body struct {
			OldPhone string `json:"old_phone"`
			NewPhone string `json:"new_phone"`
			Code     string `json:"code"`
			Operator string `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		p, err := svc.ChangePhone(ctx(), c.Params("id"), body.OldPhone, body.NewPhone, body.Code, operator(c, body.Operator))
		return respond(c, p, err)
	})
	api.Post("/passes/:id/members", func(c *fiber.Ctx) error {
		var body struct {
			service.MemberInput
			Operator string `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		m, err := svc.AddMember(ctx(), c.Params("id"), body.MemberInput, operator(c, body.Operator))
		return respond(c, m, err)
	})
	api.Delete("/passes/:id/members/:mid", func(c *fiber.Ctx) error {
		err := svc.RemoveMember(ctx(), c.Params("id"), c.Params("mid"), operator(c, ""))
		return respond(c, fiber.Map{"ok": true}, err)
	})
	api.Post("/passes/:id/members/:mid/photo", func(c *fiber.Ctx) error {
		var body struct {
			PhotoURL string `json:"photo_url"`
			Operator string `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		m, err := svc.UpdateMemberPhoto(ctx(), c.Params("id"), c.Params("mid"), body.PhotoURL, operator(c, body.Operator))
		return respond(c, m, err)
	})
	api.Post("/passes/:id/members/:mid/face-exempt", func(c *fiber.Ctx) error {
		var body struct {
			Exempt   bool   `json:"exempt"`
			Operator string `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		m, err := svc.SetFaceExempt(ctx(), c.Params("id"), c.Params("mid"), body.Exempt, operator(c, body.Operator))
		return respond(c, m, err)
	})
	api.Post("/passes/:id/qr-refresh", func(c *fiber.Ctx) error {
		var body struct {
			Operator string `json:"operator"`
		}
		_ = c.BodyParser(&body)
		token, err := svc.RefreshQRToken(ctx(), c.Params("id"), operator(c, body.Operator))
		return respond(c, fiber.Map{"qr_token": token}, err)
	})
	api.Get("/passes/:id/changes", func(c *fiber.Ctx) error {
		logs, err := svc.ListChangeLogs(ctx(), c.Params("id"))
		return respond(c, logs, err)
	})
	api.Get("/passes/:id/verifications", func(c *fiber.Ctx) error {
		list, err := svc.ListVerifications(ctx(), c.Params("id"))
		return respond(c, list, err)
	})
	api.Get("/passes/:id/reservations", func(c *fiber.Ctx) error {
		list, err := svc.ListReservations(ctx(), c.Params("id"))
		return respond(c, list, err)
	})
	api.Get("/passes/:id/gate-logs", func(c *fiber.Ctx) error {
		list, err := svc.ListGateLogs(ctx(), c.Params("id"), 200)
		return respond(c, list, err)
	})

	// ---- 核验 ----
	api.Post("/verify", func(c *fiber.Ctx) error {
		var req service.VerifyRequest
		if err := c.BodyParser(&req); err != nil {
			return fail(ErrBadRequest)
		}
		res, err := svc.Verify(ctx(), req)
		return respond(c, res, err)
	})
	api.Get("/verifications/:id", func(c *fiber.Ctx) error {
		v, mr, err := svc.GetVerification(ctx(), c.Params("id"))
		if err != nil {
			return respond(c, nil, err)
		}
		return c.JSON(fiber.Map{"data": v, "manual_review": mr})
	})
	api.Post("/verifications/:id/manual-review", func(c *fiber.Ctx) error {
		var in service.ManualReviewInput
		if err := c.BodyParser(&in); err != nil {
			return fail(ErrBadRequest)
		}
		if in.Operator == "" {
			in.Operator = operator(c, "")
		}
		mr, err := svc.ManualReview(ctx(), c.Params("id"), in)
		return respond(c, mr, err)
	})
	api.Post("/exit", func(c *fiber.Ctx) error {
		var req service.ExitRequest
		if err := c.BodyParser(&req); err != nil {
			return fail(ErrBadRequest)
		}
		return respond(c, fiber.Map{"ok": true}, svc.Exit(ctx(), req))
	})

	// ---- 设备 ----
	api.Post("/devices", func(c *fiber.Ctx) error {
		var d model.Device
		if err := c.BodyParser(&d); err != nil {
			return fail(ErrBadRequest)
		}
		out, err := svc.RegisterDevice(ctx(), d)
		return respond(c, out, err)
	})
	api.Post("/devices/:id/heartbeat", func(c *fiber.Ctx) error {
		return respond(c, fiber.Map{"ok": true}, svc.Heartbeat(ctx(), c.Params("id")))
	})
	api.Get("/devices", func(c *fiber.Ctx) error {
		list, err := svc.ListDevices(ctx())
		return respond(c, list, err)
	})

	// ---- 预约 ----
	api.Post("/reservations", func(c *fiber.Ctx) error {
		var body struct {
			PassID     string `json:"pass_id"`
			Attraction string `json:"attraction"`
			Date       string `json:"date"`
			Slot       string `json:"slot"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		r, err := svc.CreateReservation(ctx(), body.PassID, body.Attraction, body.Date, body.Slot)
		return respond(c, r, err)
	})
	api.Post("/reservations/:id/cancel", func(c *fiber.Ctx) error {
		r, err := svc.CancelReservation(ctx(), c.Params("id"))
		return respond(c, r, err)
	})

	// ---- 申诉 ----
	api.Post("/appeals", func(c *fiber.Ctx) error {
		var body struct {
			PassID         string `json:"pass_id"`
			VerificationID string `json:"verification_id"`
			Type           string `json:"type"`
			Description    string `json:"description"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		a, err := svc.CreateAppeal(ctx(), body.PassID, body.VerificationID, body.Type, body.Description)
		return respond(c, a, err)
	})
	api.Get("/appeals", func(c *fiber.Ctx) error {
		list, err := svc.ListAppeals(ctx(), c.Query("status"))
		return respond(c, list, err)
	})
	api.Post("/appeals/:id/resolve", func(c *fiber.Ctx) error {
		var body struct {
			Approve         bool   `json:"approve"`
			Resolution      string `json:"resolution"`
			CompensateQuota int    `json:"compensate_quota"`
			Handler         string `json:"handler"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		if body.Handler == "" {
			body.Handler = operator(c, "")
		}
		a, err := svc.ResolveAppeal(ctx(), c.Params("id"), body.Approve, body.Resolution, body.CompensateQuota, body.Handler)
		return respond(c, a, err)
	})

	// ---- 基础数据 ----
	api.Get("/attractions", func(c *fiber.Ctx) error {
		list, err := svc.ListAttractions(ctx())
		return respond(c, list, err)
	})
	api.Post("/attractions", func(c *fiber.Ctx) error {
		var a model.Attraction
		if err := c.BodyParser(&a); err != nil {
			return fail(ErrBadRequest)
		}
		out, err := svc.CreateAttraction(ctx(), a)
		return respond(c, out, err)
	})
	api.Get("/attractions/:id/capacity", func(c *fiber.Ctx) error {
		n, err := svc.CapacityNow(ctx(), c.Params("id"), c.Query("date"))
		return respond(c, fiber.Map{"attraction": c.Params("id"), "in_park": n}, err)
	})
	api.Get("/holidays", func(c *fiber.Ctx) error {
		list, err := svc.ListHolidays(ctx())
		return respond(c, list, err)
	})
	api.Put("/holidays", func(c *fiber.Ctx) error {
		var body struct {
			Dates    []string `json:"dates"`
			Operator string   `json:"operator"`
		}
		if err := c.BodyParser(&body); err != nil {
			return fail(ErrBadRequest)
		}
		return respond(c, fiber.Map{"ok": true}, svc.SetHolidays(ctx(), body.Dates, operator(c, body.Operator)))
	})

	// ---- 运营 ----
	admin := api.Group("/admin")
	admin.Get("/sales-rules", func(c *fiber.Ctx) error {
		list, err := svc.ListSalesRules(ctx())
		return respond(c, list, err)
	})
	admin.Put("/sales-rules/:type", func(c *fiber.Ctx) error {
		var in model.SalesRule
		if err := c.BodyParser(&in); err != nil {
			return fail(ErrBadRequest)
		}
		rule, err := svc.UpdateSalesRule(ctx(), model.PassType(c.Params("type")), in, operator(c, ""))
		return respond(c, rule, err)
	})
	admin.Get("/stats/overview", func(c *fiber.Ctx) error {
		o, err := svc.StatsOverview(ctx(), c.Query("date"))
		return respond(c, o, err)
	})
	admin.Get("/stats/peaks", func(c *fiber.Ctx) error {
		p, err := svc.StatsPeaks(ctx(), c.Query("date"))
		return respond(c, p, err)
	})
	admin.Get("/gate-logs", func(c *fiber.Ctx) error {
		list, err := svc.ListGateLogs(ctx(), "", c.QueryInt("limit", 100))
		return respond(c, list, err)
	})

	// ---- 闸机离线日志合并 / 权益回补 / 现场复核 ----
	api.Post("/offline/batches", func(c *fiber.Ctx) error {
		var in service.OfflineBatchInput
		if err := c.BodyParser(&in); err != nil {
			return fail(ErrBadRequest)
		}
		if in.Operator == "" {
			in.Operator = operator(c, "")
		}
		b, err := svc.MergeOfflineLogs(ctx(), in)
		return respond(c, b, err)
	})
	api.Get("/offline/batches", func(c *fiber.Ctx) error {
		list, err := svc.ListOfflineBatches(ctx(), c.Query("device_id"), c.QueryInt("limit", 50))
		return respond(c, list, err)
	})
	api.Get("/offline/batches/:id", func(c *fiber.Ctx) error {
		b, err := svc.GetOfflineBatch(ctx(), c.Params("id"))
		return respond(c, b, err)
	})
	api.Get("/offline/reviews", func(c *fiber.Ctx) error {
		list, err := svc.ListReviewTasks(ctx(), c.Query("status"), c.QueryInt("limit", 100))
		return respond(c, list, err)
	})
	api.Get("/offline/reviews/:id", func(c *fiber.Ctx) error {
		rv, err := svc.GetReviewTask(ctx(), c.Params("id"))
		return respond(c, rv, err)
	})
	api.Post("/offline/reviews/:id/resolve", func(c *fiber.Ctx) error {
		var in service.ReviewResolveInput
		if err := c.BodyParser(&in); err != nil {
			return fail(ErrBadRequest)
		}
		if in.Operator == "" {
			in.Operator = operator(c, "")
		}
		rv, err := svc.ResolveReviewTask(ctx(), c.Params("id"), in)
		return respond(c, rv, err)
	})
	api.Get("/offline/refunds", func(c *fiber.Ctx) error {
		list, err := svc.ListRefunds(ctx(), c.Query("pass_id"), c.QueryInt("limit", 100))
		return respond(c, list, err)
	})
	api.Get("/offline/refunds/:id", func(c *fiber.Ctx) error {
		r, err := svc.GetRefund(ctx(), c.Params("id"))
		return respond(c, r, err)
	})
	api.Get("/notifications", func(c *fiber.Ctx) error {
		list, err := svc.ListNotifications(ctx(), c.Query("pass_id"), c.Query("type"), c.Query("status"), c.QueryInt("limit", 100))
		return respond(c, list, err)
	})

	// ---- 设备离线可靠性（运营维护计划 / 高峰入口调度） ----
	admin.Get("/device-reliability", func(c *fiber.Ctx) error {
		list, err := svc.DeviceReliability(ctx())
		return respond(c, list, err)
	})
	admin.Get("/offline-incidents", func(c *fiber.Ctx) error {
		list, err := svc.ListIncidents(ctx(), c.Query("device_id"), c.QueryInt("limit", 100))
		return respond(c, list, err)
	})
}

// ErrBadRequest 请求体错误
var ErrBadRequest = errors.New("bad request body")

func ctx() context.Context { return context.Background() }

// operator 取操作人：body 优先，其次 X-Operator 头，默认 anonymous
func operator(c *fiber.Ctx, fromBody string) string {
	if fromBody != "" {
		return fromBody
	}
	if h := c.Get("X-Operator"); h != "" {
		return h
	}
	return "anonymous"
}

func respond(c *fiber.Ctx, data interface{}, err error) error {
	if err == nil {
		return c.JSON(fiber.Map{"data": data})
	}
	return fail(err)
}

func fail(err error) error { return err }

// ErrorHandler 统一错误映射
func ErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	msg := err.Error()
	var fe *fiber.Error
	if errors.As(err, &fe) {
		code = fe.Code
		return c.Status(code).JSON(fiber.Map{"error": fiber.Map{"message": msg}})
	}
	switch {
	case errors.Is(err, service.ErrNotFound):
		code = fiber.StatusNotFound
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, ErrBadRequest):
		code = fiber.StatusBadRequest
	case errors.Is(err, service.ErrConflict),
		errors.Is(err, service.ErrPassNotActive),
		errors.Is(err, service.ErrPassNotFrozen),
		errors.Is(err, service.ErrAlreadyProcessed),
		errors.Is(err, service.ErrDuplicateHolder),
		errors.Is(err, service.ErrMemberChangeNone),
		errors.Is(err, service.ErrQuotaInsufficient):
		code = fiber.StatusConflict
	case errors.Is(err, service.ErrOffSale), errors.Is(err, service.ErrDailyLimit):
		code = fiber.StatusConflict
	case errors.Is(err, service.ErrVerifyCode):
		code = fiber.StatusForbidden
	}
	return c.Status(code).JSON(fiber.Map{"error": fiber.Map{"message": msg}})
}
