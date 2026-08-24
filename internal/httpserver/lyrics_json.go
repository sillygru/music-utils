package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
)

// writeLyricsResponseJSON serializes a lyrics response in a compact-pretty
// layout: object keys each sit on their own line while the timed line/word
// tuples stay on single lines. Timed payloads otherwise expand to dozens of
// lines under a generic indenting formatter, which buries the actual data.
func writeLyricsResponseJSON(w io.Writer, res lyricsResponse) error {
	fields := make([]renderedField, 0, 10)
	appendJSON := func(key string, value any) {
		fields = append(fields, renderedField{key: key, body: jsonMarshal(value)})
	}
	appendJSON("id", res.ID)
	appendJSON("name", res.Name)
	appendJSON("trackName", res.TrackName)
	appendJSON("artistName", res.ArtistName)
	appendJSON("albumName", res.AlbumName)
	appendJSON("duration", res.Duration)
	appendJSON("instrumental", res.Instrumental)
	if res.PlainLyrics != "" {
		appendJSON("plainLyrics", res.PlainLyrics)
	}
	if res.SyncedLyrics != "" {
		appendJSON("syncedLyrics", res.SyncedLyrics)
	}
	if res.RichSync != nil {
		fields = append(fields, renderedField{key: "richSync", body: buildRichSyncObject(*res.RichSync)})
	}

	var b bytes.Buffer
	b.WriteString("{\n")
	for i, field := range fields {
		b.WriteString("  \"")
		b.WriteString(field.key)
		b.WriteString("\": ")
		b.Write(field.body)
		if i < len(fields)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	_, err := w.Write(b.Bytes())
	return err
}

type renderedField struct {
	key  string
	body []byte
}

// buildRichSyncObject renders richSyncResult with the content laid out in the
// compact-pretty format while the wrapper keeps single-line key/value pairs.
func buildRichSyncObject(rs richSyncResult) []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	b.WriteString("    \"content\": ")
	switch content := rs.Content.(type) {
	case compactRichSync:
		b.Write(buildCompactContent(content))
	case *compactRichSync:
		if content != nil {
			b.Write(buildCompactContent(*content))
		} else {
			b.WriteString("null")
		}
	case string:
		if parsed, ok := parseStoredCompactRichSync(content); ok {
			b.Write(buildCompactContent(parsed))
		} else if parsed, err := parseCompactRichSync(content); err == nil && len(parsed.Lines) > 0 {
			b.Write(buildCompactContent(parsed))
		} else {
			b.Write(jsonMarshal(content))
		}
	default:
		b.WriteString("null")
	}
	b.WriteString(",\n")
	b.WriteString("    \"format\": ")
	b.Write(jsonMarshal(rs.Format))
	b.WriteString(",\n")
	b.WriteString("    \"syncType\": ")
	b.Write(jsonMarshal(rs.SyncType))
	b.WriteString(",\n")
	b.WriteString("    \"source\": ")
	b.Write(jsonMarshal(rs.Source))
	b.WriteString("\n")
	b.WriteString("  }")
	return b.Bytes()
}

// buildCompactContent renders a compactRichSync with the lines array expanded
// and each line/word tuple kept on a single line.
func buildCompactContent(c compactRichSync) []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	if c.Title != "" {
		b.WriteString("      \"title\": ")
		b.Write(jsonMarshal(c.Title))
		b.WriteString(",\n")
	}
	if c.Artist != "" {
		b.WriteString("      \"artist\": ")
		b.Write(jsonMarshal(c.Artist))
		b.WriteString(",\n")
	}
	b.WriteString("      \"duration\": ")
	b.Write(jsonMarshal(c.Duration))
	b.WriteString(",\n")
	b.WriteString("      \"lines\": [\n")
	for i, line := range c.Lines {
		b.WriteString("        [")
		b.Write(jsonMarshal(line.Begin))
		b.WriteString(", ")
		b.Write(jsonMarshal(line.End))
		b.WriteString(", ")
		b.Write(jsonMarshal(line.Text))
		b.WriteString(", [\n")
		for j, word := range line.Words {
			b.WriteString("          [")
			b.Write(jsonMarshal(word.Begin))
			b.WriteString(", ")
			b.Write(jsonMarshal(word.End))
			b.WriteString(", ")
			b.Write(jsonMarshal(word.Text))
			b.WriteString("]")
			if j < len(line.Words)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("        ]]")
		if i < len(c.Lines)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("      ]\n")
	b.WriteString("    }")
	return b.Bytes()
}

// jsonMarshal returns the compact JSON encoding of value. Scalars and strings
// never fail to encode, so the error is intentionally discarded.
func jsonMarshal(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
