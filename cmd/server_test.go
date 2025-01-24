package main_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"cloudbeast.doni/m/api"
	"github.com/gin-gonic/gin"
	gossr "github.com/natewong1313/go-react-ssr"
	"github.com/stretchr/testify/assert"
)

func TestPingRoute(t *testing.T) {
	router := gin.Default()

    engineConfig := &gossr.Config{
		AssetRoute:         "/static",
		FrontendDir:        "../frontend/src",
		GeneratedTypesPath: "../frontend/src/generated.d.ts",
		PropsStructsPath:   "../models/props.go",
		TailwindConfigPath: "../tailwind.config.js",
		LayoutCSSFilePath:  "input.css",
	}
	engine, err := gossr.New(*engineConfig)

    if err != nil {
        t.Fatal(err)
    }

    api.SetupRouter(router, engine)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ping", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "pong", w.Body.String())
}