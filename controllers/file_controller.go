package controllers

import (
    "github.com/gin-gonic/gin"
)

type File struct {
    ID       int    `json:"id"`
    FileName string `json:"file_name"`
    UserID   int    `json:"user_id"`
}

func UploadFile(c *gin.Context) {
    // userID := c.GetInt("userID")  // récupère le userID via JWT

    // ... Chiffrement et stockage du fichier
    
    // Stocker le fichier dans la base de données avec la référence utilisateur
    // db.Create(&File{FileName: "encryptedFileName", UserID: userID})
}

func DownloadFile(c *gin.Context) {
    // Code pour gérer le téléchargement de fichier
	// userID := c.GetInt("userID")
    // fileID := c.Param("fileID")
    
    // Vérifier si le fichier appartient à l'utilisateur
    // var file File
    // db.Where("id = ? AND user_id = ?", fileID, userID).First(&file)
    // if file.ID == 0 {
    //     c.JSON(403, gin.H{"message": "Access denied"})
    //     return
    // }
}
