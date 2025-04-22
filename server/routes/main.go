package routes

import (
	"anexis/server/models"
	"github.com/gin-gonic/gin"
	gossr "github.com/natewong1313/go-react-ssr"
)

func Index(engine *gossr.Engine) func(*gin.Context) {
	return func(ctx *gin.Context) {
		ctx.Writer.Write(engine.RenderRoute(gossr.RenderConfig{
			File:     "pages/Home.tsx",
			Title:    "Annexis | Home",
			MetaTags: map[string]string{},
			Props: &models.IndexRouteProps{
				User: "Doni",
			},
		}))
	}
}
