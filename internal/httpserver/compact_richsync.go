package httpserver

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// compactRichSync is the response representation of a parsed TTML rich
// payload. Named metadata fields and tuple-shaped lines/words keep the
// payload readable while avoiding repeated object keys in timed content.
type compactRichSync struct {
	Title    string            `json:"title,omitempty"`
	Artist   string            `json:"artist,omitempty"`
	Duration float64           `json:"duration,omitempty"`
	Lines    []compactRichLine `json:"lines"`
}

type compactRichLine struct {
	Begin float64
	End   float64
	Text  string
	Words []compactRichWord
}

func (line compactRichLine) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{line.Begin, line.End, line.Text, line.Words})
}

func (line *compactRichLine) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 4 {
		return fmt.Errorf("invalid compact rich line")
	}
	if err := json.Unmarshal(tuple[0], &line.Begin); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[1], &line.End); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[2], &line.Text); err != nil {
		return err
	}
	return json.Unmarshal(tuple[3], &line.Words)
}

type compactRichWord struct {
	Begin float64
	End   float64
	Text  string
}

func (word compactRichWord) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{word.Begin, word.End, word.Text})
}

func (word *compactRichWord) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 3 {
		return fmt.Errorf("invalid compact rich word")
	}
	if err := json.Unmarshal(tuple[0], &word.Begin); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[1], &word.End); err != nil {
		return err
	}
	return json.Unmarshal(tuple[2], &word.Text)
}

// compactRichSyncContent converts source-native TTML at the API boundary. A
// non-TTML or unparseable payload remains available as a string rather than
// making an otherwise valid rich response fail.
func compactRichSyncContent(content, format string) any {
	if parsed, ok := parseStoredCompactRichSync(content); ok {
		return parsed
	}
	if !strings.EqualFold(strings.TrimSpace(format), "ttml") {
		return content
	}
	parsed, err := parseCompactRichSync(content)
	if err != nil || len(parsed.Lines) == 0 {
		return content
	}
	if parsed.Lines == nil {
		parsed.Lines = make([]compactRichLine, 0)
	}
	return parsed
}

func parseStoredCompactRichSync(content string) (compactRichSync, bool) {
	var parsed compactRichSync
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &parsed); err != nil || parsed.Lines == nil {
		return compactRichSync{}, false
	}
	return parsed, true
}

func compactRichSyncForStorage(content, format string) (string, string, bool) {
	if !strings.EqualFold(strings.TrimSpace(format), "ttml") {
		return "", "", false
	}
	parsed, err := parseCompactRichSync(content)
	if err != nil || len(parsed.Lines) == 0 {
		return "", "", false
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return "", "", false
	}
	return string(encoded), "json", true
}

func parseCompactRichSync(content string) (compactRichSync, error) {
	result := compactRichSync{Lines: make([]compactRichLine, 0)}
	decoder := xml.NewDecoder(strings.NewReader(content))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return compactRichSync{}, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "title":
			text, err := readRichElementText(decoder)
			if err != nil {
				return compactRichSync{}, err
			}
			if result.Title == "" {
				result.Title = text
			}
		case "agent":
			text, err := readRichElementText(decoder)
			if err != nil {
				return compactRichSync{}, err
			}
			if result.Artist == "" {
				result.Artist = text
			}
		case "body":
			if value, ok := richAttribute(start, "dur"); ok {
				if seconds, valid := parseRichTime(value); valid {
					result.Duration = seconds
				}
			}
		case "p":
			line, err := readRichParagraph(decoder, start)
			if err != nil {
				return compactRichSync{}, err
			}
			result.Lines = append(result.Lines, line)
		}
	}
	if result.Duration <= 0 {
		for _, line := range result.Lines {
			if line.End > result.Duration {
				result.Duration = line.End
			}
		}
	}
	return result, nil
}

func readRichElementText(decoder *xml.Decoder) (string, error) {
	var text strings.Builder
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			text.Write([]byte(value))
		}
	}
	return strings.TrimSpace(text.String()), nil
}

type richSpanContext struct {
	begin string
	end   string
	text  strings.Builder
}

func readRichParagraph(decoder *xml.Decoder, start xml.StartElement) (compactRichLine, error) {
	line := compactRichLine{}
	lineBegin, _ := richAttribute(start, "begin")
	lineEnd, _ := richAttribute(start, "end")
	line.Begin, _ = parseRichTime(lineBegin)
	line.End, _ = parseRichTime(lineEnd)
	line.Words = make([]compactRichWord, 0)

	var lineText strings.Builder
	spans := make([]richSpanContext, 0, 4)
	for {
		token, err := decoder.Token()
		if err != nil {
			return compactRichLine{}, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if strings.EqualFold(value.Name.Local, "span") {
				begin, _ := richAttribute(value, "begin")
				end, _ := richAttribute(value, "end")
				spans = append(spans, richSpanContext{begin: begin, end: end})
			}
		case xml.EndElement:
			if strings.EqualFold(value.Name.Local, "span") && len(spans) > 0 {
				span := spans[len(spans)-1]
				spans = spans[:len(spans)-1]
				if text := strings.TrimSpace(span.text.String()); text != "" {
					begin, beginOK := parseRichTime(span.begin)
					end, endOK := parseRichTime(span.end)
					if beginOK && endOK {
						line.Words = append(line.Words, compactRichWord{Begin: begin, End: end, Text: text})
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

func richAttribute(element xml.StartElement, name string) (string, bool) {
	for _, attribute := range element.Attr {
		if strings.EqualFold(attribute.Name.Local, name) {
			return strings.TrimSpace(attribute.Value), true
		}
	}
	return "", false
}

func parseRichTime(value string) (float64, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, false
	}
	if strings.HasSuffix(value, "ms") {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "ms")), 64)
		return seconds / 1000, err == nil
	}
	for _, suffix := range []struct {
		suffix string
		factor float64
	}{
		{suffix: "h", factor: 3600},
		{suffix: "m", factor: 60},
		{suffix: "s", factor: 1},
	} {
		if strings.HasSuffix(value, suffix.suffix) {
			seconds, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, suffix.suffix)), 64)
			return seconds * suffix.factor, err == nil
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) == 2 || len(parts) == 3 {
		var total float64
		for i, part := range parts {
			value, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return 0, false
			}
			switch {
			case len(parts) == 2 && i == 0:
				total += value * 60
			case len(parts) == 3 && i == 0:
				total += value * 3600
			case len(parts) == 3 && i == 1:
				total += value * 60
			default:
				total += value
			}
		}
		return total, true
	}
	seconds, err := strconv.ParseFloat(value, 64)
	return seconds, err == nil
}
