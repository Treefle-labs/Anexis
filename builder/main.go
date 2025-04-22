package main

import (
	"fmt"

	"anexis/builder/build"
)

func main() {
	err := build.BuildAllTSFiles("./src")
	if err != nil {
		fmt.Printf("failed to build files: %v", err)
	}
}
