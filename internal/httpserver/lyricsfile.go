package httpserver

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sillygru/music-utils/internal/db"
)

// lrcLinePattern matches a run of one or more timestamp tags anchored at the
// line start, capturing the trailing text. Repeated tags like
// "[00:00.00][00:10.00]" all belong to the run, while hour-style or mid-line
// tags make the whole line fall through as untagged. Fractions with one to
// three digits are accepted so centisecond timestamps from LRCLIB and
// milliseconds timestamps from other sources both parse.
var lrcLinePattern = regexp.MustCompile(`^((?:\[\d{1,3}:\d{1,2}(?:\.\d{1,3})?\])+)(.*)$`)

// lrcSingleTagPattern parses the first timestamp tag of a run.
var lrcSingleTagPattern = regexp.MustCompile(`^\[(\d{1,3}):(\d{1,2})(?:\.(\d{1,3}))?\]`)

// lyricsFileLine is one entry of the lines: section of an LRCLIB lyricsfile.
type lyricsFileLine struct {
	startMS int64
	text    string
}

// buildLyricsFile renders the lyricsfile payload LRCLIB serves alongside its
// lyrics JSON, derived entirely from fields the server already stores. It
// mirrors the observed byte format of lrclib.net: a YAML document whose
// lines: section carries LRC timestamps as start_ms/end_ms pairs, followed by
// a plain: block when plain lyrics exist. Instrumental and unsynced rows get
// an empty lines: list, matching LRCLIB.
func buildLyricsFile(track *db.Track, lyrics *db.Lyrics) string {
	if track == nil || lyrics == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("version: '1.0'\n")
	b.WriteString("metadata:\n")
	b.WriteString("  title: " + yamlScalar(track.Name) + "\n")
	b.WriteString("  artist: " + yamlScalar(track.ArtistName) + "\n")
	b.WriteString("  album: " + yamlScalar(track.AlbumName) + "\n")
	b.WriteString("  duration_ms: " + formatDurationMS(track.Duration) + "\n")
	b.WriteString("  instrumental: " + boolString(lyrics.Instrumental) + "\n")
	b.WriteString("lines:")
	lines := parseLRC(lyrics.SyncedLyrics)
	if len(lines) == 0 {
		b.WriteString(" []\n")
	} else {
		b.WriteString("\n")
		for i, line := range lines {
			b.WriteString("- text: " + yamlScalar(line.text) + "\n")
			b.WriteString("  start_ms: " + strconv.FormatInt(line.startMS, 10) + "\n")
			if i+1 < len(lines) {
				b.WriteString("  end_ms: " + strconv.FormatInt(lines[i+1].startMS, 10) + "\n")
			}
		}
	}
	if lyrics.PlainLyrics != "" {
		b.WriteString("plain: |-\n")
		for _, line := range strings.Split(lyrics.PlainLyrics, "\n") {
			if line == "" {
				// LRCLIB leaves paragraph separators unindented inside the
				// block; only text lines carry the two-space prefix.
				b.WriteString("\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
		}
	}
	return b.String()
}

// parseLRC converts LRC synced lyrics into the timestamped lines LRCLIB puts
// in its lyricsfile. Each line's start_ms comes from its first timestamp tag;
// end_ms is assigned by the caller from the following line's start. Untagged
// lines carry the previous timestamp forward so they stay in order; they only
// appear in malformed input, since LRCLIB-sourced synced lyrics are always
// timestamped.
func parseLRC(synced string) []lyricsFileLine {
	if strings.TrimSpace(synced) == "" {
		return nil
	}
	rawLines := strings.Split(synced, "\n")
	// LRC files end with a newline; split leaves a trailing empty element that
	// LRCLIB's parser does not turn into a line. Drop exactly that artifact so
	// the last timestamped line (which may itself be empty) stays the last
	// entry, end_ms and all.
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	lines := make([]lyricsFileLine, 0, len(rawLines))
	var lastMS int64
	for _, raw := range rawLines {
		match := lrcLinePattern.FindStringSubmatchIndex(raw)
		if match == nil {
			lines = append(lines, lyricsFileLine{startMS: lastMS, text: strings.TrimSpace(raw)})
			continue
		}
		lastMS = lrcTimestampMS(raw[match[2]:match[3]])
		lines = append(lines, lyricsFileLine{startMS: lastMS, text: strings.TrimSpace(raw[match[4]:match[5]])})
	}
	return lines
}

// lrcTimestampMS converts the first timestamp tag of a run into milliseconds.
// Fractions expand to milliseconds by digit count: ".5" is 500ms, ".65" is
// 650ms, ".123" is 123ms.
func lrcTimestampMS(tagRun string) int64 {
	match := lrcSingleTagPattern.FindStringSubmatchIndex(tagRun)
	if match == nil {
		return 0
	}
	minutes, _ := strconv.Atoi(tagRun[match[2]:match[3]])
	seconds, _ := strconv.Atoi(tagRun[match[4]:match[5]])
	ms := (int64(minutes)*60 + int64(seconds)) * 1000
	if match[6] >= 0 {
		fraction := tagRun[match[6]:match[7]]
		value := 0
		for _, digit := range fraction {
			value = value*10 + int(digit-'0')
		}
		for i := len(fraction); i < 3; i++ {
			value *= 10
		}
		ms += int64(value)
	}
	return ms
}

// formatDurationMS renders seconds as the integer milliseconds LRCLIB stores
// in lyricsfile metadata. Duration is a float of whole or half seconds in
// practice, so truncation matches LRCLIB's conversion.
func formatDurationMS(duration float64) string {
	return strconv.FormatInt(int64(duration*1000), 10)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// yamlScalar renders a value the way LRCLIB does: plain for ordinary text
// (apostrophes and commas included), single-quoted when the value is empty or
// would otherwise change meaning in YAML.
func yamlScalar(value string) string {
	if value == "" {
		return "''"
	}
	if yamlScalarNeedsQuoting(value) {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return value
}

func yamlScalarNeedsQuoting(value string) bool {
	if strings.TrimSpace(value) != value {
		return true
	}
	switch value[0] {
	case '!', '&', '*', '-', '?', '{', '}', '[', ']', ',', '|', '>', '@', '`', '"', '\'', '%', '#', ':':
		return true
	}
	return strings.Contains(value, ": ") || strings.Contains(value, " #") || strings.HasSuffix(value, ":")
}
