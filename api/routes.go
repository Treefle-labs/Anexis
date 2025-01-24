package api

import (
	"log"

	"cloudbeast.doni/m/controllers"
	"cloudbeast.doni/m/middleware"
	"cloudbeast.doni/m/routes"
	"github.com/gin-gonic/gin"
    gossr "github.com/natewong1313/go-react-ssr"
)

func SetupRouter(router *gin.Engine, engine *gossr.Engine) {
    router.GET("/ping", controllers.PingRoute)
    router.POST("/upload", controllers.UploadFile)
    router.GET("/file/:id", controllers.DownloadFile)
    router.GET("/staticFile/:file", routes.Static)
    router.GET("/", routes.Index(engine))
    // Autres routes
    auth := router.Group("/auth")
    auth.Use(middleware.ValidateJWT)
    {
        auth.GET("/user", func(ctx *gin.Context) {
            user, _ := ctx.GetQuery("username")
            log.Default().Print(user)
        })
    }
}
