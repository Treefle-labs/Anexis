package middleware

import (
	"net/http"

	"anexis/server/controllers"
	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
)

func ValidateJWT(c *gin.Context) {
	tokenStr := c.Request.Header.Get("Authorization")
	if tokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "Token is missing"})
		c.Abort()

		return
	}

	claims := &controllers.Claims{}
	token, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(token *jwt.Token) (interface{}, error) {
			return controllers.JwtKey, nil
		},
	)

	if err != nil || !token.Valid {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "Invalid token"})
		c.Abort()

		return
	}

	c.Set("userID", claims.UserId)
	c.Next()
}
