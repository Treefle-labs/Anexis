package api

import (
	"log"

	"cloudbeast.doni/m/controllers"
	"cloudbeast.doni/m/middleware"
	"github.com/gin-gonic/gin"
)

func SetupRouter(router *gin.Engine) {
    router.POST("/upload", controllers.UploadFile)
    router.GET("/file/:id", controllers.DownloadFile)
    // Autres routes
    authGroup := router.Group("/api")
    authGroup.Use(middleware.ValidateJWT)
    {
        authGroup.GET("/auth", func(ctx *gin.Context) {
            user, _ := ctx.GetQuery("username")
            log.Default().Print(user)
        })
    }
}
