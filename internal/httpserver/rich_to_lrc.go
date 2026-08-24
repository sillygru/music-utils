package httpserver

import (
	"fmt"

	"github.com/sillygru/music-utils/internal/db"
)

// compactRichSyncToLRC converts timed rich lines to standard LRC. It uses line
// timings only, intentionally dropping word-level detail for ordinary clients.
func compactRichSyncToLRC(rich *db.RichLyrics) string {
	if rich == nil {
		return ""
	}
	content := compactRichSyncContent(rich.Content, rich.Format)
	parsed, ok := content.(compactRichSync)
	if !ok || len(parsed.Lines) == 0 {
		return ""
	}
	out := make([]byte, 0, len(parsed.Lines)*32)
	for _, line := range parsed.Lines {
		if line.Begin < 0 {
			continue
		}
		minutes := int(line.Begin) / 60
		seconds := line.Begin - float64(minutes*60)
		text := line.Text
		if text == "" {
			for _, word := range line.Words {
				if text != "" {
					text += " "
				}
				text += word.Text
			}
		}
		if text == "" {
			continue
		}
		out = append(out, []byte(fmt.Sprintf("[%02d:%05.2f]%s\n", minutes, seconds, text))...)
	}
	return string(out)
}
