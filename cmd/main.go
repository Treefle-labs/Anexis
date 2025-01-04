package main

import (
	"log"

	"cloudbeast.doni/m/api"
	"cloudbeast.doni/m/utils"
	"github.com/gin-gonic/gin"
	gossr "github.com/natewong1313/go-react-ssr"
)

func main() {
	router := gin.Default()
	engineConfig := &gossr.Config{
		AssetRoute:         "/static",
		FrontendDir:        "./frontend/src",
		GeneratedTypesPath: "./frontend/src/generated.d.ts",
		PropsStructsPath:   "./models/props.go",
		TailwindConfigPath: "./tailwind.config.js",
		LayoutCSSFilePath:  "input.css",
	}
	engine, enginErr := gossr.New(*engineConfig)
	if enginErr != nil {
		log.Fatal(enginErr)
	}
	router.StaticFS("/static", gin.Dir("../client", false))
    router.StaticFile("favicon.ico", "./client/ico.svg")
	api.SetupRouter(router, engine)

	err := router.Run(":8080") // Utilisation du port 8080 pour HTTP
	if err != nil {
		log.Fatal("Error starting server: ", err)
	}
}

func init() {
	utils.CreateDirectories([]string{})
}
