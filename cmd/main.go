package main

import (
	"context"
	"log"
	"os"

	"cloudbeast.doni/m/api"
	"cloudbeast.doni/m/utils"
	"github.com/Backblaze/blazer/b2"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Erreur lors du chargement du fichier .env")
	}
	id := os.Getenv("B2_APPLICATION_KEY_ID")
	key := os.Getenv("B2_APPLICATION_KEY")

	ctx := context.Background()

	// b2_authorize_account
	b2, err := b2.NewClient(ctx, id, key)
	if err != nil {
		log.Fatalln(err)
	}

	buckets, err := b2.ListBuckets(ctx)
	if err != nil {
		log.Fatalln(err)
	}
	println(buckets)
	router := gin.Default()
	pprof.Register(router)
	router.StaticFS("/static", gin.Dir("../client", false))
	router.StaticFile("favicon.ico", "./client/ico.svg")
	api.SetupRouter(router)

	err = router.Run(":8080") // Utilisation du port 8080 pour HTTP
	if err != nil {
		log.Fatal("Error starting server: ", err)
	}
}

func init() {
	utils.CreateDirectories([]string{})
}
