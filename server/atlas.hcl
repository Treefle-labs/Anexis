data "external_schema" "gorm" {
  program = [
    "go",
    "run",
    "-mod=mod",
    "./cmd/loader",
  ]
}

env "gorm" {
  src = data.external_schema.gorm.url
  dev = "postgresql://doni:DoniLite13@localhost:5432/anexis"
  migration {
    dir = "file://migrations"
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}