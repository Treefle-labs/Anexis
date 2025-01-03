package main

import (
	"log"

	"cloudbeast.doni/m/api"
	"cloudbeast.doni/m/utils"
	"github.com/gin-gonic/gin"
)

func main() {
    router := gin.Default()
    router.LoadHTMLGlob("templates/**/*")
    router.StaticFS("/static", gin.Dir("./client", false))
    api.SetupRouter(router)

    err := router.Run(":8080") // Utilisation du port 8080 pour HTTP
    if err != nil {
        log.Fatal("Error starting server: ", err)
    }
}

func init() {
    utils.CreateDirectories([]string{})
}
