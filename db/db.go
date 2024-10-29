package db

import (
	"cloudbeast.doni/m/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var DB gorm.DB

// Contient la base de données du projet 'cloudbeast' gérée par `gorm ORM`
func init() {
  
  db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
  if err != nil {
    panic("failed to connect database")
  }

  // Migrate the schema
  db.AutoMigrate(&models.Product{}, models.User{})

  // Create
  db.Create(&models.Product{Code: "D42", Price: 100})

  // Read
  var product models.Product
  db.First(&product, 1) // find product with integer primary key
  db.First(&product, "code = ?", "D42") // find product with code D42

  // Update - update product's price to 200
  db.Model(&product).Update("Price", 200)
  // Update - update multiple fields
  db.Model(&product).Updates(models.Product{Price: 200, Code: "F42"}) // non-zero fields
  db.Model(&product).Updates(map[string]interface{}{"Price": 200, "Code": "F42"})

  // Delete - delete product
  db.Delete(&product, 1)

  DB = *db
}
