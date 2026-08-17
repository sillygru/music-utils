// Package names cleans common media-library filename labels before music lookups.
package names

import (
	"regexp"
	"strings"
	"unicode"
)

// Input is a cleaned music identity. It is deliberately limited to the fields
// used to query metadata, lyrics, and cover providers.
type Input struct {
	TrackName  string
	ArtistName string
	AlbumName  string
}

var (
	bracketedLabel  = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	separator       = regexp.MustCompile(`\s+[-–—]\s+`)
	trackNumber     = regexp.MustCompile(`^\s*\d{1,3}\s*[-._)]\s+`)
	quotedTitle     = regexp.MustCompile(`^\s*(.+?)\s*["“”＂]\s*(.+?)\s*["“”＂]\s*$`)
	downloadID      = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)
	downloadIDDigit = regexp.MustCompile(`[0-9]`)
	qualityTag      = regexp.MustCompile(`(?i)^(hd|hq|uhd|4k|8k|\d{3,4}p|\d{2,3}kbps|hi[- ]?res|lossless|mv|cc)$`)
	styleWords      = regexp.MustCompile(`(?i)(^|[\s_+/,|\-–—]+)(nightcore|hardstyle|jumpstyle|speed\s*up|sped\s*up|super\s*slowed|ultra\s+slowed|slowed|reverb|remix|visualizer|lyrics?|music\s+video|lyric\s+video|official\s+(music\s+)?video|official\s+audio|audio\s+only|lyrics?\s+only|amv|mv|reupload|webmexport|tiktok(\s+version)?|bass\s*boost(ed)?|pitched|club\s+mix|rock\s+version|dubstep|female\s+version|instrumental|karaoke(\s+version)?|a\s*capella|8d|hd\s+version|swedish\s+original|original\s+song|drift\s+music\s+video|from\s+the\s+fast(\s*&\s*furious)?(\s+phonk)?(\s+mixtape)?)([\s_+/,|\-–—.!?]+|$)`)
	trailingLabels  = regexp.MustCompile(`(?i)([\s_+/,|\-–—]+)(radio\s+edit|hard\s+techno|hard\s+trance|hard\s+bass|hard\s+dance|drift\s+phonk|brazilian\s+phonk|drum\s+and\s+bass|jersey\s+club|future\s+bass|bass\s+house|uk\s+garage|rawstyle|jumpstyle|hardstyle|hardcore|gabber|frenchcore|speedcore|terrorcore|uptempo|phonk|breakcore|dnb|techno|trance|psytrance|trap|house|funk|electro|hyperpop|nightcore|speed|official|audio|lyrics?|edit|mix|version|remix|hd|hq|uhd|4k|8k|\d{3,4}p|\d{2,3}kbps|hi[- ]?res|lossless|original|live|acoustic|demo|extended|clean|explicit|mono|stereo|mashup|medley|karaoke|fan\s*made|unofficial|bootleg|rework|vip)[\s_+/,|\-–—.!?]*$`)
	descriptiveTail = regexp.MustCompile(`(?i)\s+would\s+(u|you)\s+do\b.*$`)
	spaces          = regexp.MustCompile(`\s+`)
)

// Normalize cleans all three lookup identity fields. If artist_name is empty,
// a conservative artist-title separator is used to infer it from track_name.
// Explicit artist input always wins over an inferred artist.
func Normalize(trackName, artistName, albumName string) Input {
	track := cleanFilename(trackName)
	artist := cleanIdentity(artistName)
	album := cleanIdentity(albumName)

	if artist == "" {
		var inferredArtist string
		track, inferredArtist = splitArtistTitle(track)
		artist = inferredArtist
	} else {
		// Folder/tag artist names are authoritative; remove a repeated
		// "Artist -" prefix from a filename rather than searching it as part
		// of the title. Do not parse other hyphens when the artist is known.
		track = removeArtistPrefix(track, artist)
	}
	return Input{
		TrackName:  cleanTrack(track),
		ArtistName: cleanArtist(artist),
		AlbumName:  cleanAlbum(album),
	}
}

// Candidates returns the primary cleaned identity plus a conservative alternate
// for filenames whose hyphen order may be title - artist instead of artist -
// title. Explicit artist input disables the alternate because the caller has
// already supplied authoritative identity.
func Candidates(trackName, artistName, albumName string) []Input {
	primary := Normalize(trackName, artistName, albumName)
	if strings.TrimSpace(artistName) != "" {
		return []Input{primary}
	}
	value := strings.TrimSpace(strings.ReplaceAll(cleanFilename(trackName), "｜", "|"))
	value = stripBracketedLabels(value)
	parts := separator.Split(value, -1)
	if len(parts) == 2 || len(parts) == 3 && styleOnly(parts[2]) {
		left := cleanTrack(parts[0])
		right := cleanTrack(parts[1])
		if left != "" && right != "" && !isLabelText(left) && !isLabelText(right) {
			alternate := Input{TrackName: left, ArtistName: right, AlbumName: cleanAlbum(albumName)}
			if !sameInput(primary, alternate) {
				return []Input{primary, alternate}
			}
		}
	}
	return []Input{primary}
}

// CleanSearch removes filename/source labels from a free-text search while
// retaining ordinary words and separators. It does not infer an artist because
// free-text search is intentionally broader than exact lookup.
func CleanSearch(value string) string {
	return cleanTrack(cleanFilename(value))
}

// CleanTrack cleans a title without attempting to infer an artist.
func CleanTrack(value string) string { return cleanTrack(cleanFilename(value)) }

// CleanArtist cleans an explicitly supplied artist name. Artist separators are
// not parsed here because commas, ampersands, and similar punctuation can be
// legitimate multi-artist credits.
func CleanArtist(value string) string { return cleanArtist(cleanFilename(value)) }

// CleanAlbum cleans an explicitly supplied album name.
func CleanAlbum(value string) string { return cleanAlbum(cleanFilename(value)) }

func cleanFilename(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "\\\\"); index >= 0 {
		value = value[index+1:]
	} else if strings.HasPrefix(value, "/") || strings.Contains(value, "://") {
		if index := strings.LastIndex(value, "/"); index >= 0 {
			value = value[index+1:]
		}
	}
	// Media libraries often expose downloader-generated suffixes such as
	// .webmexport.mp3. Remove known media extensions repeatedly, but do not
	// blindly remove arbitrary dot-separated words from real song titles.
	for i := 0; i < 3; i++ {
		lower := strings.ToLower(value)
		removed := false
		for _, extension := range []string{".webmexport", ".mp3", ".m4a", ".mp4", ".mkv", ".webm", ".webp", ".flac", ".wav", ".ogg", ".opus", ".aac", ".wma", ".ape", ".mka", ".m4v", ".avi", ".3gp"} {
			if strings.HasSuffix(lower, extension) {
				value = strings.TrimSpace(value[:len(value)-len(extension)])
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	value = strings.Trim(value, " \t\r\n\"'“”‘’")
	return trackNumber.ReplaceAllString(value, "")
}

func stripDescriptiveTail(value string) string {
	match := descriptiveTail.FindStringIndex(value)
	if match == nil {
		return value
	}
	prefix := strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ").Replace(value[:match[0]])
	if !hasStyleCue(prefix) {
		return value
	}
	return value[:match[0]]
}

func stripPipeUploaderTail(value string) string {
	parts := strings.Split(value, "|")
	if len(parts) < 2 {
		return value
	}
	labelPipe := false
	for _, part := range parts[1:] {
		if isLabelText(part) {
			labelPipe = true
			break
		}
	}
	if !labelPipe {
		return value
	}
	first := parts[0]
	closeIndex := strings.LastIndexAny(first, ")]")
	if closeIndex < 0 || closeIndex == len(first)-1 {
		return value
	}
	openIndex := strings.LastIndexAny(first[:closeIndex], "([")
	if openIndex < 0 || !isLabelText(first[openIndex+1:closeIndex]) {
		return value
	}
	if strings.TrimSpace(first[closeIndex+1:]) == "" {
		return value
	}
	parts[0] = first[:closeIndex+1]
	return strings.Join(parts, "|")
}

func hasStyleCue(value string) bool {
	value = strings.ToLower(value)
	for _, cue := range []string{"nightcore", "hardstyle", "jumpstyle", "rawstyle", "hardcore", "gabber", "frenchcore", "speedcore", "terrorcore", "phonk", "breakcore", "hard techno", "hard trance", "sped up", "speed up", "slowed", "reverb", "remix", "visualizer", "lyrics", "music video", "instrumental", "karaoke"} {
		if strings.Contains(value, cue) {
			return true
		}
	}
	return false
}

func cleanIdentity(value string) string {
	return cleanTrack(value)
}

func cleanTrack(value string) string {
	value = strings.ReplaceAll(value, "｜", "|")
	value = strings.ReplaceAll(value, "⧸", "/")
	value = strings.NewReplacer("↬", " ", "→", " ", "➜", " ").Replace(value)
	value = stripDescriptiveTail(value)
	value = stripPipeUploaderTail(value)
	value = stripBracketedLabels(value)
	value = stripDescriptiveTail(value)
	// A vertical-bar suffix is commonly a downloader title/description. Keep
	// it when it is an artist | title separator, but discard clearly labelled
	// source segments such as "| Sped Up + Bass Boosted | Rave Energy".
	parts := strings.Split(value, "|")
	if len(parts) > 1 {
		kept := parts[:1]
		labelSeen := false
		for _, part := range parts[1:] {
			if labelSeen || isLabelText(part) {
				labelSeen = true
				continue
			}
			kept = append(kept, part)
		}
		value = strings.Join(kept, " | ")
	}
	for i := 0; i < 4; i++ {
		cleaned := styleWords.ReplaceAllString(value, " ")
		cleaned = trailingLabels.ReplaceAllString(cleaned, " ")
		if cleaned == value {
			break
		}
		value = cleaned
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsSymbol(r) && r != '|' {
			return ' '
		}
		return r
	}, value)
	// Keep balanced parentheses/brackets because they can be part of a real
	// title, e.g. "(It Goes Like) Nanana" or "Song [Remastered]".
	value = strings.Trim(value, " \t\r\n\"'“”‘’.-–—|:;")
	value = spaces.ReplaceAllString(value, " ")
	return strings.TrimSpace(value)
}

func cleanArtist(value string) string {
	return cleanTrack(value)
}

func cleanAlbum(value string) string {
	return cleanTrack(value)
}

func stripBracketedLabels(value string) string {
	for i := 0; i < 4; i++ {
		changed := false
		value = bracketedLabel.ReplaceAllStringFunc(value, func(block string) string {
			inside := strings.TrimSpace(block[1 : len(block)-1])
			if isLabelText(inside) || onlyDecorative(inside) || looksLikeDownloadID(inside) {
				changed = true
				return " "
			}
			return block
		})
		if !changed {
			break
		}
	}
	return value
}

func looksLikeDownloadID(value string) bool {
	return downloadID.MatchString(value) && downloadIDDigit.MatchString(value)
}

func normalizeLabelCharacters(value string) string {
	return strings.NewReplacer(
		"𝑺", "S", "𝒍", "l", "𝒐", "o", "𝒘", "w", "𝒆", "e", "𝒅", "d",
		"𝑹", "R", "𝒗", "v", "𝒓", "r", "𝒃", "b",
	).Replace(value)
}

func isLabelText(value string) bool {
	value = strings.ToLower(strings.TrimSpace(normalizeLabelCharacters(value)))
	if value == "" || value == "nv" || qualityTag.MatchString(value) {
		return true
	}
	if onlyDecorative(value) {
		return true
	}
	value = strings.NewReplacer("+", " ", "/", " ", "_", " ", "-", " ", "–", " ", "—", " ").Replace(value)
	value = spaces.ReplaceAllString(value, " ")
	padded := " " + strings.TrimSpace(value) + " "
	markers := []string{
		"music video", "official video", "official audio", "official channel", "lyric video", "lyrics only", "audio only", "instrumental is",
		"bass boost", "club mix", "rock version", "female version", "hd version", "swedish original", "original song",
		"drift music video", "from the fast", "hard techno", "hard trance", "hard bass", "hard dance", "drift phonk", "brazilian phonk", "drum and bass", "jersey club", "future bass", "bass house", "uk garage",
		"nightcore", "hardstyle", "jumpstyle", "rawstyle", "hardcore", "gabber", "frenchcore", "speedcore", "terrorcore", "uptempo", "phonk", "breakcore", "dnb", "techno", "trance", "psytrance", "trap", "house", "funk", "electro", "hyperpop", "sped up", "speed up", "slowed",
		"reverb", "remix", "version", "visualizer", "lyrics", "lyric", "audio", "amv", "mv", "reupload", "webmexport", "youtube",
		"tiktok", "pitched", "dubstep", "instrumental", "speed", "karaoke", "acoustic", "demo", "live", "extended", "clean", "explicit", "mono", "stereo", "mashup", "medley", "fan made", "unofficial", "bootleg", "rework", "vip", "official",
	}
	singleCount := 0
	for _, marker := range markers {
		if strings.Contains(marker, " ") {
			if strings.Contains(padded, " "+marker+" ") {
				return true
			}
			continue
		}
		if value == marker {
			return true
		}
		if strings.Contains(padded, " "+marker+" ") {
			singleCount++
		}
	}
	return singleCount >= 2
}

func onlyDecorative(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func splitArtistTitle(value string) (track, artist string) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "｜", "|"))
	value = stripDescriptiveTail(value)
	value = stripBracketedLabels(value)
	if match := quotedTitle.FindStringSubmatch(value); len(match) == 3 && strings.TrimSpace(match[1]) != "" && strings.TrimSpace(match[2]) != "" {
		return strings.TrimSpace(match[2]), strings.TrimSpace(match[1])
	}
	if value == "" {
		return "", ""
	}
	parts := separator.Split(value, -1)
	if len(parts) == 2 && styleQualified(parts[0]) && !isLabelText(parts[1]) {
		// Some downloaders use "Title INSTRUMENTAL - Artist". Treat a
		// non-empty title plus a version/style qualifier as title-first, while
		// a bare "Nightcore - Song" remains a style prefix with no artist.
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	if len(parts) == 3 && styleOnly(parts[2]) && !isLabelText(parts[0]) && !isLabelText(parts[1]) {
		// Nightcore uploads also commonly use "Title - Artist - Nightcore".
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	if len(parts) < 2 {
		// A full-width bar is handled as an artist/title separator when neither
		// side is a source label.
		barParts := strings.Split(value, "|")
		if len(barParts) == 2 && !isLabelText(barParts[0]) && !isLabelText(barParts[1]) {
			return strings.TrimSpace(barParts[1]), strings.TrimSpace(barParts[0])
		}
		return value, ""
	}
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !isLabelText(part) {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) < 2 {
		if len(cleaned) == 1 {
			return cleaned[0], ""
		}
		return value, ""
	}
	// The public convention is artist - title. Keeping the first two useful
	// segments is safer than guessing at hyphens inside a title; labels have
	// already been removed from either end and from bracketed groups.
	return strings.Join(cleaned[1:], " - "), cleaned[0]
}

func styleOnly(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "nightcore", "hardstyle", "jumpstyle", "rawstyle", "hardcore", "gabber", "frenchcore", "speedcore", "terrorcore", "uptempo", "phonk", "breakcore", "dnb", "techno", "trance", "psytrance", "trap", "house", "funk", "electro", "hyperpop", "slowed", "ultra slowed", "super slowed", "sped up", "speed up", "reverb", "remix", "dubstep", "instrumental", "karaoke", "acoustic", "demo", "live":
		return true
	default:
		return false
	}
}

func sameInput(a, b Input) bool {
	return strings.EqualFold(a.TrackName, b.TrackName) && strings.EqualFold(a.ArtistName, b.ArtistName) && strings.EqualFold(a.AlbumName, b.AlbumName)
}

func styleQualified(value string) bool {
	cleaned := styleWords.ReplaceAllString(value, " ")
	return strings.TrimSpace(cleaned) != "" && cleaned != value
}

func removeArtistPrefix(track, artist string) string {
	parts := separator.Split(strings.TrimSpace(track), 2)
	if len(parts) == 2 && sameName(parts[0], artist) {
		return strings.TrimSpace(parts[1])
	}
	return track
}

func sameName(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	return a != "" && b != "" && a == b
}
