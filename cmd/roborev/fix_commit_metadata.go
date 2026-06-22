package main

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

func formatFixCommitMetadataInstructions(metadata config.FixCommitMetadata) string {
	if metadata.Empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Commit Metadata\n\n")
	if metadata.Author != "" {
		fmt.Fprintf(&sb, "Use this commit author: `%s`.\n", metadata.Author)
	}
	if len(metadata.CoAuthors) > 0 {
		sb.WriteString("Include these commit trailers exactly:\n")
		for _, coAuthor := range metadata.CoAuthors {
			fmt.Fprintf(&sb, "- `Co-authored-by: %s`\n", coAuthor)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}
