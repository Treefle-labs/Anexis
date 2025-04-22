package controllers

import (
	"net/http"
	"time"

	"anexis/server/db"
	"anexis/server/models"
	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
)

type Claims struct {
	UserId int `json:"userId"`
	jwt.StandardClaims
}

var JwtKey = []byte("your_secret_key") // Remplace par une clé secrète sécurisée

func GenerateToken(c *gin.Context) {
	dbx, err := db.Setup()
	if err != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"message": "Some features not initialize properly"},
		)
	}

	userID, ok := c.Get("userID")

	var user models.User

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "User not authenticated"})

		return
	}

	result := dbx.First(&user, userID.(int))

	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"message": "not found user"})

		return
	}

	claims := &Claims{
		UserId: userID.(int),
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: time.Now().Add(time.Hour * 24).Unix(), // Token expirera dans 24 heures
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	tokenString, err := token.SignedString(JwtKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"message": "Could not create token"})

		return
	}

	c.JSON(http.StatusOK, gin.H{"token": tokenString})
}
