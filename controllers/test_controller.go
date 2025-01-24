package controllers

import "github.com/gin-gonic/gin"

func PingRoute(ctx *gin.Context) {
	ctx.String(200, "pong")
}