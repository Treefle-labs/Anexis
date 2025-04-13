package main

import (
	"fmt"

	"cloudbeast.doni/m/build"
)

func main() {
	err := build.BuildAllTSFiles("./src")
	if err != nil {
		fmt.Printf("failed to build files: %v", err)
	}
}
