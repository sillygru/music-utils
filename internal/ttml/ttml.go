// Package ttml parses TTML timed-text documents into LRC lyrics.
//
// It is shared by every provider that serves TTML (BetterLyrics, Paxsenix,
// LyricsPlus-Binimum, Apple Music): each client returns LRC produced here so
// the HTTP layer can persist it without knowing TTML. Parsing is done with
// encoding/xml and never resolves external entities or DTDs.
package ttml

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Word is one timed word inside a line.
type Word struct {
	Start float64
	End   float64
	Text  string
}

// Line is one timed lyric line.
type Line struct {
	Start      float64
	End        float64
	Text       string
	Words      []Word
	Background bool
	Agent      string
}

// Parse parses a TTML document into timed lines. Untimed lines are kept with
// Start < 0 so callers can decide whether to drop them.
func Parse(content string) ([]Line, error) {
	decoder := xml.NewDecoder(strings.NewReader(content))
	lines := make([]Line, 0, 32)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "p") {
			continue
		}
		line, err := readParagraph(decoder, start)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// ToLRC converts parsed lines to clean standard LRC text. Lines without timing are skipped.
func ToLRC(lines []Line) string {
	var b strings.Builder
	for _, line := range lines {
		if line.Start < 0 {
			continue
		}
		text := strings.TrimSpace(line.Text)
		if text == "" {
			for _, word := range line.Words {
				if text != "" {
					text += " "
				}
				text += word.Text
			}
			text = strings.TrimSpace(text)
		}
		if text == "" {
			continue
		}
		minutes := int(line.Start) / 60
		seconds := line.Start - float64(minutes*60)
		fmt.Fprintf(&b, "[%02d:%05.2f]%s\n", minutes, seconds, text)
	}
	return b.String()
}

// ParseToLRC is Parse followed by ToLRC.
func ParseToLRC(content string) (string, error) {
	lines, err := Parse(content)
	if err != nil {
		return "", err
	}
	return ToLRC(lines), nil
}

// PlainText extracts untimed text lines from TTML, for providers that need a
// plain fallback when no timing exists.
func PlainText(content string) string {
	lines, err := Parse(content)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, line := range lines {
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

var reLRCVoiceTags = regexp.MustCompile(`\{agent:[^}]*\}|\{bg\}`)

// CleanSyncedLyrics strips non-standard {agent:...} and {bg} tags from LRC lyrics.
func CleanSyncedLyrics(s string) string {
	if s == "" {
		return ""
	}
	return reLRCVoiceTags.ReplaceAllString(s, "")
}

// ExtractPlainFromLRC extracts untimed plain lyric text from LRC content.
func ExtractPlainFromLRC(lrc string) string {
	if lrc == "" {
		return ""
	}
	lines := strings.Split(lrc, "\n")
	var b strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for strings.HasPrefix(line, "[") {
			idx := strings.Index(line, "]")
			if idx == -1 {
				break
			}
			line = strings.TrimSpace(line[idx+1:])
		}
		line = strings.TrimSpace(CleanSyncedLyrics(line))
		if line != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

type spanContext struct {
	begin      string
	end        string
	text       strings.Builder
	background bool
}

func readParagraph(decoder *xml.Decoder, start xml.StartElement) (Line, error) {
	line := Line{Start: -1}
	if value, ok := attr(start, "begin"); ok {
		if seconds, valid := ParseTime(value); valid {
			line.Start = seconds
		}
	}
	if value, ok := attr(start, "end"); ok {
		if seconds, valid := ParseTime(value); valid {
			line.End = seconds
		}
	}
	if value, ok := attr(start, "agent"); ok {
		line.Agent = value
	}
	if value, ok := attr(start, "role"); ok && strings.Contains(strings.ToLower(value), "background") {
		line.Background = true
	}
	if value, ok := attr(start, "ttm:role"); ok && strings.Contains(strings.ToLower(value), "background") {
		line.Background = true
	}
	line.Words = make([]Word, 0, 4)

	var lineText strings.Builder
	spans := make([]spanContext, 0, 4)
	for {
		token, err := decoder.Token()
		if err != nil {
			return Line{}, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if strings.EqualFold(value.Name.Local, "span") {
				begin, _ := attr(value, "begin")
				end, _ := attr(value, "end")
				background := line.Background
				if role, ok := attr(value, "role"); ok && strings.Contains(strings.ToLower(role), "background") {
					background = true
				}
				spans = append(spans, spanContext{begin: begin, end: end, background: background})
			} else if strings.EqualFold(value.Name.Local, "br") {
				lineText.WriteByte('\n')
			}
		case xml.EndElement:
			if strings.EqualFold(value.Name.Local, "span") && len(spans) > 0 {
				span := spans[len(spans)-1]
				spans = spans[:len(spans)-1]
				if text := strings.TrimSpace(span.text.String()); text != "" {
					begin, beginOK := ParseTime(span.begin)
					end, endOK := ParseTime(span.end)
					if beginOK && endOK {
						line.Words = append(line.Words, Word{Start: begin, End: end, Text: text})
					} else if !line.Background && span.background {
						line.Background = true
					}
				}
			}
			if strings.EqualFold(value.Name.Local, start.Name.Local) {
				line.Text = strings.Join(strings.Fields(lineText.String()), " ")
				return line, nil
			}
		case xml.CharData:
			lineText.Write([]byte(value))
			if len(spans) > 0 {
				spans[len(spans)-1].text.Write([]byte(value))
			}
		}
	}
}

func attr(element xml.StartElement, name string) (string, bool) {
	for _, attribute := range element.Attr {
		if strings.EqualFold(attribute.Name.Local, name) {
			return strings.TrimSpace(attribute.Value), true
		}
	}
	return "", false
}

// ParseTime parses TTML time expressions: clock values (mm:ss.mmm,
// hh:mm:ss.mmm, mm:ss:ff frames), and offset values (12.5s, 1500ms, 2m, 1h).
func ParseTime(value string) (float64, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, false
	}
	if strings.HasSuffix(value, "ms") {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "ms")), 64)
		return seconds / 1000, err == nil
	}
	for _, unit := range []struct {
		suffix string
		factor float64
	}{
		{suffix: "h", factor: 3600},
		{suffix: "m", factor: 60},
		{suffix: "s", factor: 1},
	} {
		if strings.HasSuffix(value, unit.suffix) {
			seconds, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, unit.suffix)), 64)
			return seconds * unit.factor, err == nil
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) == 2 || len(parts) == 3 {
		var total float64
		for i, part := range parts {
			number, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return 0, false
			}
			switch {
			case len(parts) == 2 && i == 0:
				total += number * 60
			case len(parts) == 3 && i == 0:
				total += number * 3600
			case len(parts) == 3 && i == 1:
				total += number * 60
			default:
				total += number
			}
		}
		return total, true
	}
	seconds, err := strconv.ParseFloat(value, 64)
	return seconds, err == nil
}
