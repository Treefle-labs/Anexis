package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
)


func Index(ctx *gin.Context) {
	ctx.HTML(http.StatusOK, "index.html", gin.H{})
}