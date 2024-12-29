package main

import (
	"fmt"
	"path/filepath"

	"cloudbeast.doni/m/build"
)

func main() {
	filePath, err := filepath.Abs("./src")
	if err != nil {
		fmt.Printf("failed to get absolute path: %v", err)
	}
	err2 := build.WatchTSFiles(filePath)
	if err2 != nil {
		fmt.Printf("failed to watch files: %v", err2)
	}
}
