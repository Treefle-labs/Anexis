package api

import (
	"log"

	"cloudbeast.doni/m/controllers"
	"cloudbeast.doni/m/middleware"
	"cloudbeast.doni/m/routes"
	"github.com/gin-gonic/gin"
)

func SetupRouter(router *gin.Engine) {
    router.POST("/upload", controllers.UploadFile)
    router.GET("/file/:id", controllers.DownloadFile)
    router.GET("/staticFile/:file", routes.Static)
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
