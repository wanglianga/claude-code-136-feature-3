package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	swaggerFiles "github.com/swaggo/files"
	"passgate/internal/handler"
	"passgate/internal/seed"
	"passgate/internal/service"
	"passgate/internal/store"
)

//go:embed docs/openapi.yaml
var specRaw []byte

// swaggerIndex 自绘 Swagger UI 入口：静态资源来自内嵌的 swagger-ui-dist，spec 指向本服务 /openapi.yaml
const swaggerIndex = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <title>passgate API 文档</title>
  <link rel="stylesheet" href="/swagger/swagger-ui.css">
  <link rel="icon" href="/swagger/favicon-32x32.png">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="/swagger/swagger-ui-bundle.js"></script>
  <script>
    window.onload = function() {
      window.ui = SwaggerUIBundle({
        url: "/openapi.yaml",
        dom_id: "#swagger-ui",
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
        layout: "BaseLayout"
      });
    };
  </script>
</body>
</html>`

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	redisAddr := getenv("REDIS_ADDR", "127.0.0.1:6379")
	st := store.New(redisAddr, "", 0)
	ctx := context.Background()
	if err := st.Ping(ctx); err != nil {
		log.Fatalf("无法连接 Redis(%s): %v", redisAddr, err)
	}
	svc := service.New(st)
	if err := seed.SeedIfEmpty(ctx, svc); err != nil {
		log.Printf("[seed] 失败: %v", err)
	}

	app := fiber.New(fiber.Config{
		AppName:      "passgate 景区年卡核验服务",
		ErrorHandler: handler.ErrorHandler,
	})
	handler.Register(app, svc)

	// OpenAPI 3.x 文档与 Swagger UI（资源内嵌进二进制，离线可用）
	app.Get("/openapi.yaml", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/yaml; charset=utf-8")
		return c.Send(specRaw)
	})
	app.Get("/swagger/index.html", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(swaggerIndex)
	})
	assetServer := http.StripPrefix("/swagger/", http.FileServer(swaggerFiles.HTTP))
	app.Get("/swagger/*", adaptor.HTTPHandler(assetServer))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.Redirect("/swagger/index.html")
	})

	port := getenv("PORT", "8080")
	log.Printf("listening on :%s (redis=%s)", port, redisAddr)
	log.Printf("Swagger UI: http://127.0.0.1:%s/swagger/index.html", port)
	log.Fatal(app.Listen(":" + port))
}
