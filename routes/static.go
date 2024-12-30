package routes

import (
	"net/http"
	"path"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

func Static(ctx *gin.Context) {
	file := ctx.Param("file")
	filePath := path.Join("./client", file)
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"pathError": "the provided data is invalid"})
		return
	}
	// isValidPath := fs.ValidPath(absPath)
	// if !isValidPath {
	// 	ctx.JSON(http.StatusInternalServerError, gin.H{"fileError": "an error with the file " + filePath})
	// 	return
	// }
	ctx.File(absPath)
}
