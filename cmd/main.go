package main

import (
	"log"

	"cloudbeast.doni/m/api"
	"github.com/gin-gonic/gin"
)

func main() {
    router := gin.Default()
    api.SetupRouter(router)

    err := router.Run(":8080") // Utilisation du port 8080 pour HTTP
    if err != nil {
        log.Fatal("Error starting server: ", err)
    }
}
