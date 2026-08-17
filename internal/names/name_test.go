package names

import "testing"

func TestNormalizeLibraryNames(t *testing.T) {
	tests := []struct {
		name   string
		track  string
		artist string
		wantT  string
		wantA  string
	}{
		{"artist title video", "Avril Lavigne - Girlfriend (Official Video).mp3", "", "Girlfriend", "Avril Lavigne"},
		{"artist title visualizer", "Ari Abdul - Bite Marks (Visualizer).mp3", "", "Bite Marks", "Ari Abdul"},
		{"music video marker", "AGONY & 4GET - Loving You Is Suicide [AMV].mkv", "", "Loving You Is Suicide", "AGONY & 4GET"},
		{"nightcore prefix", "Nightcore - Monster (Female Version) (Lyrics) [yIVW8nih0ss].mp3", "", "Monster", ""},
		{"nightcore suffix", "After Dark - Nightcore.m4a", "", "After Dark", ""},
		{"title artist style", "Fallen Angel - Three Days Grace - Nightcore.m4a", "", "Fallen Angel", "Three Days Grace"},
		{"style suffix", "L'Amour Toujours (Sped Up Hardstyle).m4a", "", "L'Amour Toujours", ""},
		{"vertical artist separator", "Ayesha erotica, Heidi montag ｜ I'll do it (sped up + pitched).mp3", "", "I'll do it", "Ayesha erotica, Heidi montag"},
		{"downloader suffix", "NBSPLV - The Lost Soul Down (sped up/tiktok version).webmexport.mp3", "", "The Lost Soul Down", "NBSPLV"},
		{"fullwidth quote", "Dominic Fike ＂Babydoll＂ (Official Audio).mp3", "", "Babydoll", "Dominic Fike"},
		{"download id", "moves like jagger (sped up) [ZpFU8AizcVI].m4a", "", "moves like jagger", ""},
		{"bracketed title retained", "Song [Remastered].mp3", "", "Song [Remastered]", ""},
		{"high definition label", "Hatsune Miku - Atama no Taisou HD.mp3", "", "Atama no Taisou", "Hatsune Miku"},
		{"explicit artist wins", "YOASOBI - Song (Official Music Video).mp3", "YOASOBI", "Song", "YOASOBI"},
		{"audio artist", "Audio Bullys - Song.mp3", "", "Song", "Audio Bullys"},
		{"official artist", "Official HIGE DANDism - Song.mp3", "", "Song", "Official HIGE DANDism"},
		{"instrumental note", "Lady Gaga - Bad Romance (but instrumental is 10s delay).mp3", "", "Bad Romance", "Lady Gaga"},
		{"webp suffix", "Ari Abdul - Stay (Lyric Video).webp", "", "Stay", "Ari Abdul"},
		{"unicode slowed label", "Schnuffel - Ich hab' Dich lieb (𝑺𝒍𝒐𝒘𝒆𝒅+𝑹𝒆𝒗𝒆𝒓𝒃).mp3", "", "Ich hab' Dich lieb", "Schnuffel"},
		{"uploader tail", "primadonna girl (sped up) would u do anything for me？.mp3", "", "primadonna girl", ""},
		{"instrumental title artist order", "ROI INSTRUMENTAL - FIRONN (TIKTOK VERSION).mp3", "", "ROI", "FIRONN"},
	}
	for _, test := range tests {
		got := Normalize(test.track, test.artist, "")
		if got.TrackName != test.wantT || got.ArtistName != test.wantA {
			t.Errorf("%s: Normalize() = track %q artist %q, want track %q artist %q", test.name, got.TrackName, got.ArtistName, test.wantT, test.wantA)
		}
	}
}

func TestCleansPipeSeparatedUploaderNoise(t *testing.T) {
	got := CleanTrack("Misery (Slowed) pupsies | Misery Slowed TikTok Version | Reverblaster")
	if got != "Misery" {
		t.Fatalf("CleanTrack() = %q, want %q", got, "Misery")
	}
}

func TestKeepsArtistPipeTitleSeparators(t *testing.T) {
	got := CleanTrack("Ayesha erotica | I'll do it")
	if got != "Ayesha erotica | I'll do it" {
		t.Fatalf("CleanTrack() = %q, want artist/title separator preserved", got)
	}
}

func TestCandidatesTryBothHyphenOrders(t *testing.T) {
	candidates := Candidates("falling faster - dylan espeseth (OFFICIAL LYRIC VIDEO).mp3", "", "")
	if len(candidates) != 2 {
		t.Fatalf("expected primary and alternate candidates, got %+v", candidates)
	}
	if candidates[0].ArtistName != "falling faster" || candidates[0].TrackName != "dylan espeseth" {
		t.Fatalf("unexpected primary candidate: %+v", candidates[0])
	}
	if candidates[1].ArtistName != "dylan espeseth" || candidates[1].TrackName != "falling faster" {
		t.Fatalf("unexpected alternate candidate: %+v", candidates[1])
	}
}

func TestNormalizeRemovesAdditionalSafeTags(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"quality tags", CleanTrack("Song [MV] [1080p] [320kbps].mp3"), "Song"},
		{"version labels", CleanTrack("Song (Live Acoustic Demo) [Official].m4a"), "Song"},
		{"radio edit", CleanTrack("Song - Radio Edit - HQ.mp3"), "Song"},
		{"social source", CleanTrack("Song - TikTok Version - 4K.mp4"), "Song"},
		{"web export", CleanTrack("Song Official.webmexport.mp3"), "Song"},
		{"track number", CleanTrack("01. Artist - Song.ape"), "Artist - Song"},
		{"jumpstyle", CleanTrack("Song (Jumpstyle).mp3"), "Song"},
		{"rawstyle", CleanTrack("Song - Rawstyle Remix.m4a"), "Song"},
		{"drift phonk", CleanTrack("Song (Drift Phonk Remix).mp3"), "Song"},
		{"hard techno", CleanTrack("Song - Hard Techno - 4K.mp4"), "Song"},
		{"dnb", CleanTrack("Song (DnB).mp3"), "Song"},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestNormalizePreservesLegitimateNames(t *testing.T) {
	tests := []string{
		"Audio Bullys",
		"Official HIGE DANDism",
		"From The Start",
		"Live Without You",
		"Song [Remastered]",
		"Techno",
		"Phonk",
	}
	for _, value := range tests {
		if got := CleanTrack(value); got != value {
			t.Errorf("CleanTrack(%q) = %q; legitimate name was changed", value, got)
		}
	}
}

func TestNormalizePreservesLegitimateParenthesesAndFeatures(t *testing.T) {
	got := Normalize("(It Goes Like) Nanana (feat. Ciscaux).m4a", "", "")
	if got.TrackName != "(It Goes Like) Nanana (feat. Ciscaux)" {
		t.Fatalf("unexpected normalized title: %q", got.TrackName)
	}
}

func TestCleanSearchRemovesTrailingSourceDescription(t *testing.T) {
	got := CleanSearch("GOOD PUSS (Euphoric Nightcore Techno Remix) ⚡💗 ｜ Sped Up + Bass Boosted ｜ Rave Energy.mp3")
	if got != "GOOD PUSS" {
		t.Fatalf("CleanSearch() = %q, want %q", got, "GOOD PUSS")
	}
}

func TestNormalizeStripsOnlyKnownExtensions(t *testing.T) {
	got := Normalize("Song.v2.m4a", "Artist", "Album")
	if got.TrackName != "Song.v2" {
		t.Fatalf("unexpected dotted title: %q", got.TrackName)
	}
	if got.AlbumName != "Album" {
		t.Fatalf("unexpected album: %q", got.AlbumName)
	}
}
