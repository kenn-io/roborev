package main

import (
	"fmt"
	"os"

	"go.kenn.io/roborev/internal/skills"
)

func main() {
	if err := skills.WriteDerivedSkillFiles("."); err != nil {
		fmt.Fprintf(os.Stderr, "generate derived skills: %v\n", err)
		os.Exit(1)
	}
}
