package api

import (
	"log"

	"anexis/server/controllers"
	"anexis/server/middleware"
	"anexis/server/routes"
	"github.com/gin-gonic/gin"
)

func SetupRouter(router *gin.Engine) {
	router.GET("/ping", controllers.PingRoute)
	router.POST("/upload", controllers.UploadFile)
	router.GET("/file/:id", controllers.DownloadFile)
	router.GET("/staticFile/:file", routes.Static)
	// router.GET("/", routes.Index(engine))
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
